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
package lockmatrix

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tochemey/mig/internal/lint/lockmodel"
	"github.com/tochemey/mig/test/harness"
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
}

// Observation is what the server did.
type Observation struct {
	// Locks holds the strongest granted or requested mode per relation,
	// restricted to relations that existed before the statement.
	Locks map[string]lockmodel.LockMode

	// Rewritten holds the relations whose relfilenode changed. A dropped
	// relation is absent rather than rewritten.
	Rewritten map[string]bool

	// RefusedTx reports that the statement was rejected inside a transaction
	// block, checked only for blocked cases.
	RefusedTx bool
}

// relState is one relation before or after the statement.
type relState struct {
	name     string
	filenode int64
}

const (
	// pollEvery paces the wait for a blocked statement's lock request.
	pollEvery = 25 * time.Millisecond

	// pollFor bounds it.
	pollFor = 30 * time.Second
)

// Probe runs one case in a fresh clone and reports what the server did.
func Probe(ctx context.Context, h *harness.Harness, c Case) (observation Observation, err error) {
	name, err := h.Clone(ctx, harness.Template)
	if err != nil {
		return Observation{}, err
	}

	db, err := h.Open(ctx, name)
	if err != nil {
		return Observation{}, err
	}

	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close %q: %w", name, closeErr))
		}
	}()

	for _, stmt := range c.Seed {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return Observation{}, fmt.Errorf("seed %q: %w", stmt, err)
		}
	}

	before, err := snapshot(ctx, db)
	if err != nil {
		return Observation{}, err
	}

	observation, err = observe(ctx, db, c, before)
	if err != nil {
		return Observation{}, err
	}

	after, err := snapshot(ctx, db)
	if err != nil {
		return Observation{}, err
	}

	observation.Rewritten = rewritten(before, after)

	return observation, nil
}

// observe picks the strategy the case calls for.
func observe(ctx context.Context, db *sql.DB, c Case, before map[int64]relState) (Observation, error) {
	if c.Blocked {
		return observeBlocked(ctx, db, c, before)
	}

	return observeInTx(ctx, db, c, before)
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

	defer func() {
		_ = rows.Close()
	}()

	relations := make(map[int64]relState)

	for rows.Next() {
		var oid, filenode int64
		var name string

		if err := rows.Scan(&oid, &name, &filenode); err != nil {
			return nil, fmt.Errorf("scan pg_class row: %w", err)
		}

		relations[oid] = relState{name: name, filenode: filenode}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read pg_class: %w", err)
	}

	return relations, nil
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
func observeInTx(ctx context.Context, db *sql.DB, c Case, before map[int64]relState) (Observation, error) {
	conn, pid, err := session(ctx, db)
	if err != nil {
		return Observation{}, err
	}

	defer func() {
		_ = conn.Close()
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
func observeBlocked(ctx context.Context, db *sql.DB, c Case, before map[int64]relState) (Observation, error) {
	refused, err := refusesTransaction(ctx, db, c.SQL)
	if err != nil {
		return Observation{}, err
	}

	guard, _, err := session(ctx, db)
	if err != nil {
		return Observation{}, err
	}

	defer func() {
		_ = guard.Close()
	}()

	if _, err := guard.ExecContext(ctx, "BEGIN"); err != nil {
		return Observation{}, fmt.Errorf("begin guard: %w", err)
	}

	if _, err := guard.ExecContext(ctx, c.Guard); err != nil {
		return Observation{}, fmt.Errorf("guard %q: %w", c.Guard, err)
	}

	worker, pid, err := session(ctx, db)
	if err != nil {
		return Observation{}, err
	}

	defer func() {
		_ = worker.Close()
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
func refusesTransaction(ctx context.Context, db *sql.DB, stmt string) (bool, error) {
	conn, _, err := session(ctx, db)
	if err != nil {
		return false, err
	}

	defer func() {
		_ = conn.Close()
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
	if errors.As(execErr, &pgErr) && pgErr.Code == "25001" {
		// active_sql_transaction: the refusal NoTx is about.
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

	defer func() {
		_ = rows.Close()
	}()

	locks := make(map[string]lockmodel.LockMode)

	for rows.Next() {
		var oid int64
		var name string

		if err := rows.Scan(&oid, &name); err != nil {
			return nil, fmt.Errorf("scan pg_locks row: %w", err)
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

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read pg_locks: %w", err)
	}

	return locks, nil
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
