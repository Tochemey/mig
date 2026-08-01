// MIT License
//
// Copyright (c) 2026 Arsene Tochemey Gandote
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.
//

// Package lockmatrix observes the locks a statement really takes, so the lock
// model's predictions are held to a running server rather than to the
// documentation.
//
// A transactional statement runs inside an open transaction and its locks are
// read from pg_locks before the commit. A statement that refuses transaction
// blocks is started behind a conflicting guard lock instead, and the mode it
// requests is read while it waits; pg_locks shows a waiting request the same
// way it shows a granted lock. Whether a rewrite occurred is read afterwards
// from pg_class.relfilenode, which a rewrite always replaces.
//
// A scan leaves neither trace: it takes no extra lock and replaces no storage
// file. The only evidence is what the server says about it, so the statement
// runs on a connection with client_min_messages at debug1 and the lines it
// logs about verifying and validating are collected.
package lockmatrix

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/tochemey/mig/internal/lint/lockmodel"
	"github.com/tochemey/mig/test/harness"
)

const (
	// pollEvery paces the wait for a blocked statement's lock request.
	pollEvery = 25 * time.Millisecond

	// pollFor bounds it.
	pollFor = 30 * time.Second
)

// sqlstateActiveTransaction is active_sql_transaction, the refusal a notx
// statement answers a transaction block with.
const sqlstateActiveTransaction = "25001"

// The debug lines that report row-by-row work, each naming what it visited.
// The server writes them at debug1 through errmsg_internal, so they are not
// translated and the prefixes hold whatever the locale.
const (
	noticeVerifying    = `verifying table "`
	noticeRewriting    = `rewriting table "`
	noticeValidatingFK = `validating foreign key constraint "`
)

// Case is one statement of the matrix.
type Case struct {
	Name string

	// Seed prepares the case's schema in a fresh clone.
	Seed []string

	// SQL is the statement under observation.
	SQL string

	// Blocked marks a statement that refuses transaction blocks. It is
	// observed while waiting behind Guard rather than inside a transaction.
	Blocked bool

	// Guard is a statement run in an open transaction to make the observed
	// statement queue, for example LOCK TABLE in a conflicting mode.
	Guard string

	// Extra lists locks the server takes that the statement does not name,
	// such as the table of a dropped index. The offline model cannot predict
	// them; the matrix still requires them to be exactly these.
	Extra map[string]lockmodel.LockMode

	// ExtraRewrites lists relations whose relfilenode changes without the
	// model predicting a rewrite, which is how TRUNCATE swaps its storage.
	ExtraRewrites []string

	// Visits lists the relations the server reports going through row by row.
	// It is the server's account, not the model's: a query, a DML statement
	// and the maintenance commands all read every row without a word at
	// debug1, and their entry is empty even though they plainly scan.
	Visits []string
}

// Observation is what the server did.
type Observation struct {
	// Locks holds the strongest granted or requested mode per relation,
	// restricted to relations that existed before the statement.
	Locks map[string]lockmodel.LockMode

	// Rewritten holds the relations whose relfilenode changed. A dropped
	// relation is absent rather than rewritten.
	Rewritten map[string]bool

	// Scanned holds the relations the server reported visiting row by row,
	// read from its debug output rather than from the catalog.
	Scanned map[string]bool

	// RefusedTx reports that the statement was rejected inside a transaction
	// block, checked only for blocked cases.
	RefusedTx bool

	// Debug is every debug line the statement produced, which is what a new
	// case is written against.
	Debug []string
}

// relState is one relation before or after the statement.
type relState struct {
	name     string
	filenode int64
}

// noticeLog collects the debug output of one probe. The observed statement
// runs on one connection, but its notices arrive on the driver's goroutine,
// so the collection is guarded.
type noticeLog struct {
	mu       sync.Mutex
	messages []string
}

func (l *noticeLog) add(message string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.messages = append(l.messages, message)
}

func (l *noticeLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return slices.Clone(l.messages)
}

// openObserved opens the pool the observed statement runs on: one connection,
// at debug1, with the server's notices collected. It is separate from the
// harness's own pool because only this statement's output is evidence.
func openObserved(ctx context.Context, dsn string) (*sql.DB, *noticeLog, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("parse dsn: %w", err)
	}

	log := &noticeLog{}

	cfg.RuntimeParams["client_min_messages"] = "debug1"
	cfg.OnNotice = func(_ *pgconn.PgConn, notice *pgconn.Notice) {
		log.add(notice.Message)
	}

	db := stdlib.OpenDB(*cfg)

	// One connection, so every notice belongs to the statement under
	// observation rather than to whatever else the pool was doing.
	db.SetMaxOpenConns(1)

	if err := db.PingContext(ctx); err != nil {
		return nil, nil, errors.Join(fmt.Errorf("ping observed connection: %w", err), db.Close())
	}

	return db, log, nil
}

// scanned reads the relations the server reported visiting. A foreign key is
// reported by constraint name, so it is resolved to the table that was
// scanned; a constraint the statement rolled back is gone by then and is left
// out rather than guessed at.
func scanned(ctx context.Context, db *sql.DB, messages []string) (map[string]bool, error) {
	relations := make(map[string]bool)

	for _, message := range messages {
		switch {
		case strings.HasPrefix(message, noticeVerifying):
			relations[quotedName(message, noticeVerifying)] = true

		case strings.HasPrefix(message, noticeRewriting):
			relations[quotedName(message, noticeRewriting)] = true

		case strings.HasPrefix(message, noticeValidatingFK):
			table, err := constraintTable(ctx, db, quotedName(message, noticeValidatingFK))
			if err != nil {
				return nil, err
			}

			if table != "" {
				relations[table] = true
			}
		}
	}

	return relations, nil
}

// quotedName reads the identifier a debug line names after its prefix.
func quotedName(message, prefix string) string {
	return strings.TrimSuffix(strings.TrimPrefix(message, prefix), `"`)
}

// constraintTable names the table a constraint sits on, empty when the
// constraint no longer exists.
func constraintTable(ctx context.Context, db *sql.DB, name string) (string, error) {
	var table sql.NullString

	err := db.QueryRowContext(ctx, `
		SELECT c.conrelid::regclass::text
		  FROM pg_constraint c
		 WHERE c.conname = $1
		 LIMIT 1`, name).Scan(&table)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("resolve constraint %q: %w", name, err)
	}

	return table.String, nil
}

// Verify holds one case's predictions at major to what the server at major
// actually does, reporting every disagreement. It is what both matrices are:
// the version-wide one runs it over every statement the model knows, and the
// flip one runs the same cases either side of a change in behaviour.
func Verify(t *testing.T, h *harness.Harness, c Case, major int) {
	t.Helper()

	analysis, err := lockmodel.Analyze(c.SQL, major)
	if err != nil {
		t.Fatalf("Analyze(%q): %v", c.SQL, err)
	}

	if analysis.NoTx != c.Blocked {
		t.Fatalf("NoTx = %v, but the case says blocked = %v", analysis.NoTx, c.Blocked)
	}

	observed, err := Probe(t.Context(), h, c)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}

	if c.Blocked && !observed.RefusedTx {
		t.Error("predicted notx, but the server accepted the statement in a transaction")
	}

	expected := strongest(analysis)
	maps.Copy(expected, c.Extra)

	// A blocked statement is caught at its first lock request, so only the
	// locks seen have to match. A transactional statement holds everything at
	// once and has to match exactly.
	for name, mode := range observed.Locks {
		if expected[name] != mode {
			t.Errorf("observed %s on %q, predicted %s", mode, name, expected[name])
		}
	}

	if !c.Blocked {
		for name, mode := range expected {
			if _, ok := observed.Locks[name]; !ok {
				t.Errorf("predicted %s on %q, observed nothing", mode, name)
			}
		}
	}

	rewrites := rewriteSet(analysis, c.ExtraRewrites)

	for name := range observed.Rewritten {
		if !rewrites[name] {
			t.Errorf("%q was rewritten, and no rewrite was predicted", name)
		}
	}

	for name := range rewrites {
		if !observed.Rewritten[name] {
			t.Errorf("predicted a rewrite of %q, and none happened", name)
		}
	}

	// A scan takes no extra lock and leaves the storage file alone, so
	// neither pg_locks nor relfilenode can see one. What the server says it
	// visited is the evidence, and the fixture states it exactly: the server
	// reports the ALTER TABLE family's row work and stays quiet about
	// everyone else's.
	visited := make(map[string]bool, len(c.Visits))

	for _, name := range c.Visits {
		visited[name] = true
	}

	if !maps.Equal(observed.Scanned, visited) {
		t.Errorf("server visited %v, fixture says %v: %q",
			slices.Sorted(maps.Keys(observed.Scanned)), c.Visits, observed.Debug)
	}

	// Whatever the server visited, the model may not have called it catalog
	// work. That is the direction a reader is hurt by.
	for name, duration := range durationOf(analysis) {
		if duration == lockmodel.Instant && observed.Scanned[name] {
			t.Errorf("predicted catalog-only work on %q, and the server visited its rows: %q",
				name, observed.Debug)
		}
	}
}

// strongest folds an analysis into its strongest predicted mode per relation.
// The fixtures never qualify a name, so the relation name alone is the key.
func strongest(analysis lockmodel.Analysis) map[string]lockmodel.LockMode {
	predicted := make(map[string]lockmodel.LockMode)

	for _, effect := range analysis.Effects {
		if effect.Mode > predicted[effect.Relation.Name] {
			predicted[effect.Relation.Name] = effect.Mode
		}
	}

	return predicted
}

// durationOf folds an analysis into the heaviest duration predicted per
// relation, which is the same reading the reports take of a statement whose
// actions cost different amounts.
func durationOf(analysis lockmodel.Analysis) map[string]lockmodel.DurationClass {
	predicted := make(map[string]lockmodel.DurationClass)

	for _, effect := range analysis.Effects {
		if effect.Duration > predicted[effect.Relation.Name] {
			predicted[effect.Relation.Name] = effect.Duration
		}
	}

	return predicted
}

// rewriteSet folds an analysis into the relations it predicts a rewrite for.
func rewriteSet(analysis lockmodel.Analysis, extra []string) map[string]bool {
	rewrites := make(map[string]bool)

	for _, effect := range analysis.Effects {
		if effect.Duration == lockmodel.Rewrite {
			rewrites[effect.Relation.Name] = true
		}
	}

	for _, name := range extra {
		rewrites[name] = true
	}

	return rewrites
}

// RefusesTransaction reports whether the server rejects the statement inside
// a transaction block. It is the notx half of a probe on its own, for a
// statement whose locks are not on a relation and so cannot be observed while
// it waits behind a guard.
func RefusesTransaction(ctx context.Context, h *harness.Harness, seed []string, sql string) (refused bool, err error) {
	db, name, err := prepare(ctx, h, seed)
	if err != nil {
		return false, err
	}

	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close %q: %w", name, closeErr))
		}
	}()

	return refusesTransaction(ctx, db, sql)
}

// prepare clones the template, opens it and runs the seed.
func prepare(ctx context.Context, h *harness.Harness, seed []string) (*sql.DB, string, error) {
	name, err := h.Clone(ctx, harness.Template)
	if err != nil {
		return nil, "", err
	}

	db, err := h.Open(ctx, name)
	if err != nil {
		return nil, "", err
	}

	for _, stmt := range seed {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return nil, "", errors.Join(fmt.Errorf("seed %q: %w", stmt, err), db.Close())
		}
	}

	return db, name, nil
}

// Probe runs one case in a fresh clone and reports what the server did.
func Probe(ctx context.Context, h *harness.Harness, c Case) (observation Observation, err error) {
	db, name, err := prepare(ctx, h, c.Seed)
	if err != nil {
		return Observation{}, err
	}

	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close %q: %w", name, closeErr))
		}
	}()

	observed, notices, err := openObserved(ctx, h.DSN(name))
	if err != nil {
		return Observation{}, err
	}

	defer func() {
		if closeErr := observed.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close observed connection: %w", closeErr))
		}
	}()

	before, err := snapshot(ctx, db)
	if err != nil {
		return Observation{}, err
	}

	observation, err = observe(ctx, db, observed, c, before)
	if err != nil {
		return Observation{}, err
	}

	after, err := snapshot(ctx, db)
	if err != nil {
		return Observation{}, err
	}

	observation.Rewritten = rewritten(before, after)
	observation.Debug = notices.all()

	observation.Scanned, err = scanned(ctx, db, observation.Debug)
	if err != nil {
		return Observation{}, err
	}

	return observation, nil
}

// observe picks the strategy the case calls for. The statement runs on the
// observed connection either way; the locks are read from the plain pool,
// which the statement is not holding.
func observe(ctx context.Context, db, observed *sql.DB, c Case, before map[int64]relState) (Observation, error) {
	if c.Blocked {
		return observeBlocked(ctx, db, observed, c, before)
	}

	return observeInTx(ctx, db, observed, c, before)
}

// snapshot records every user relation with its storage file, keyed by oid so
// a rename stays the same relation.
func snapshot(ctx context.Context, db *sql.DB) (map[int64]relState, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT c.oid, c.relname, c.relfilenode
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public'`)
	if err != nil {
		return nil, fmt.Errorf("snapshot pg_class: %w", err)
	}

	relations, err := scanSnapshot(rows)

	// The close is joined onto the read rather than dropped: it is the one
	// place that reports what the iteration could not.
	if err := errors.Join(err, rows.Close()); err != nil {
		return nil, fmt.Errorf("read pg_class: %w", err)
	}

	return relations, nil
}

// scanSnapshot reads the relations out of one answer to the pg_class query.
func scanSnapshot(rows *sql.Rows) (map[int64]relState, error) {
	relations := make(map[int64]relState)

	for rows.Next() {
		var oid, filenode int64
		var name string

		if err := rows.Scan(&oid, &name, &filenode); err != nil {
			return nil, err
		}

		relations[oid] = relState{name: name, filenode: filenode}
	}

	return relations, rows.Err()
}

// rewritten reports the relations whose storage file changed.
func rewritten(before, after map[int64]relState) map[string]bool {
	changed := make(map[string]bool)

	for oid, was := range before {
		if now, ok := after[oid]; ok && now.filenode != was.filenode {
			changed[was.name] = true
		}
	}

	return changed
}

// observeInTx runs the statement in an open transaction, reads the locks it
// holds, and commits so the rewrite check sees the result.
func observeInTx(ctx context.Context, db, observed *sql.DB, c Case,
	before map[int64]relState) (observation Observation, err error) {
	conn, pid, err := session(ctx, observed)
	if err != nil {
		return Observation{}, err
	}

	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("return the observed session: %w", closeErr))
		}
	}()

	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		return Observation{}, fmt.Errorf("begin: %w", err)
	}

	if _, err := conn.ExecContext(ctx, c.SQL); err != nil {
		_, rollbackErr := conn.ExecContext(ctx, "ROLLBACK")
		return Observation{}, errors.Join(fmt.Errorf("run %q: %w", c.SQL, err), rollbackErr)
	}

	locks, err := heldLocks(ctx, db, pid, before)
	if err != nil {
		_, rollbackErr := conn.ExecContext(ctx, "ROLLBACK")
		return Observation{}, errors.Join(err, rollbackErr)
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return Observation{}, fmt.Errorf("commit: %w", err)
	}

	return Observation{Locks: locks}, nil
}

// observeBlocked first confirms the statement refuses a transaction block,
// then starts it behind the guard and reads the mode it requests while it
// waits. The guard is then released and the statement runs to completion.
func observeBlocked(ctx context.Context, db, observed *sql.DB, c Case,
	before map[int64]relState) (observation Observation, err error) {
	refused, err := refusesTransaction(ctx, db, c.SQL)
	if err != nil {
		return Observation{}, err
	}

	guard, _, err := session(ctx, db)
	if err != nil {
		return Observation{}, err
	}

	defer func() {
		if closeErr := guard.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("return the guard session: %w", closeErr))
		}
	}()

	if _, err := guard.ExecContext(ctx, "BEGIN"); err != nil {
		return Observation{}, fmt.Errorf("begin guard: %w", err)
	}

	if _, err := guard.ExecContext(ctx, c.Guard); err != nil {
		return Observation{}, fmt.Errorf("guard %q: %w", c.Guard, err)
	}

	worker, pid, err := session(ctx, observed)
	if err != nil {
		return Observation{}, err
	}

	defer func() {
		if closeErr := worker.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("return the worker session: %w", closeErr))
		}
	}()

	done := make(chan error, 1)

	go func() {
		_, execErr := worker.ExecContext(ctx, c.SQL)
		done <- execErr
	}()

	locks, pollErr := pollLocks(ctx, db, pid, before)

	// The guard is released whatever the poll saw, or the worker never
	// finishes and the failure is reported as a hang instead of a diff.
	if _, err := guard.ExecContext(ctx, "ROLLBACK"); err != nil {
		return Observation{}, errors.Join(pollErr, fmt.Errorf("release guard: %w", err))
	}

	if err := <-done; err != nil {
		return Observation{}, errors.Join(pollErr, fmt.Errorf("run %q: %w", c.SQL, err))
	}

	if pollErr != nil {
		return Observation{}, pollErr
	}

	return Observation{Locks: locks, RefusedTx: refused}, nil
}

// session pins one connection and reads its backend pid.
func session(ctx context.Context, db *sql.DB) (*sql.Conn, int, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("open session: %w", err)
	}

	var pid int

	if err := conn.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		return nil, 0, errors.Join(fmt.Errorf("read backend pid: %w", err), conn.Close())
	}

	return conn, pid, nil
}

// refusesTransaction reports whether the server rejects the statement inside
// a transaction block, which is the behaviour NoTx predicts.
func refusesTransaction(ctx context.Context, db *sql.DB, stmt string) (refused bool, err error) {
	conn, _, err := session(ctx, db)
	if err != nil {
		return false, err
	}

	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("return the refusal session: %w", closeErr))
		}
	}()

	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}

	_, execErr := conn.ExecContext(ctx, stmt)

	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		return false, errors.Join(fmt.Errorf("rollback: %w", err), execErr)
	}

	if execErr == nil {
		return false, nil
	}

	var pgErr *pgconn.PgError
	if errors.As(execErr, &pgErr) && pgErr.Code == sqlstateActiveTransaction {
		return true, nil
	}

	return false, fmt.Errorf("failed inside a transaction for another reason: %w", execErr)
}

// heldLocks reads the strongest lock per pre-existing user relation that pid
// holds or waits for. pg_locks lists a waiting request like a granted lock,
// which is what makes a blocked statement observable.
func heldLocks(ctx context.Context, db *sql.DB, pid int, before map[int64]relState) (map[string]lockmodel.LockMode, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT relation, mode
		  FROM pg_locks
		 WHERE pid = $1 AND locktype = 'relation'`, pid)
	if err != nil {
		return nil, fmt.Errorf("read pg_locks: %w", err)
	}

	locks, err := scanLocks(rows, before)

	if err := errors.Join(err, rows.Close()); err != nil {
		return nil, fmt.Errorf("read pg_locks: %w", err)
	}

	return locks, nil
}

// scanLocks folds one answer to the pg_locks query into the strongest mode
// per relation.
func scanLocks(rows *sql.Rows, before map[int64]relState) (map[string]lockmodel.LockMode, error) {
	locks := make(map[string]lockmodel.LockMode)

	for rows.Next() {
		var oid int64
		var name string

		if err := rows.Scan(&oid, &name); err != nil {
			return nil, err
		}

		// A relation born inside the statement, or outside the public
		// schema, is not part of the matrix.
		relation, ok := before[oid]
		if !ok {
			continue
		}

		mode, ok := lockmodel.ModeFromPgLocks(name)
		if !ok {
			return nil, fmt.Errorf("pg_locks reported %q, not a table lock mode", name)
		}

		if mode > locks[relation.name] {
			locks[relation.name] = mode
		}
	}

	return locks, rows.Err()
}

// pollLocks waits until pid holds or requests at least one observable lock.
func pollLocks(ctx context.Context, db *sql.DB, pid int, before map[int64]relState) (map[string]lockmodel.LockMode, error) {
	deadline := time.Now().Add(pollFor)

	for {
		locks, err := heldLocks(ctx, db, pid, before)
		if err != nil {
			return nil, err
		}

		if len(locks) > 0 {
			return locks, nil
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("no lock observed within %s", pollFor)
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollEvery):
		}
	}
}
