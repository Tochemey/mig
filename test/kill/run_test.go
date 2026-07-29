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

package kill_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tochemey/mig/internal/catalog"
	"github.com/tochemey/mig/internal/crash"
	"github.com/tochemey/mig/internal/exec"
	"github.com/tochemey/mig/test/harness"
)

const (
	// indexName is the index the migration under test builds.
	indexName = "idx_users_legacy_email"

	// migrationBody is a single non-transactional step, which is the case with
	// no atomic solution: the DDL cannot share a transaction with its ledger
	// write.
	migrationBody = `-- +mig step: index_legacy_email
-- +mig notx
CREATE INDEX CONCURRENTLY ` + indexName + ` ON users (legacy_email);
`

	// migrationFile is named for the timestamp the format requires.
	migrationFile = "20240817120000_index_legacy_email.sql"
)

// run drives one database through the migration.
type run struct {
	t        *testing.T
	database string
	dir      string
}

// newRun gives the test its own database and migration directory.
func newRun(t *testing.T) *run {
	t.Helper()

	if shared == nil {
		t.Skip("postgres container not available")
	}

	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, migrationFile), []byte(migrationBody), 0o600); err != nil {
		t.Fatalf("write migration: %v", err)
	}

	return &run{t: t, database: newDatabase(t), dir: dir}
}

// start spawns the migrator, arming a crash point when one is given.
func (r *run) start(point string) *harness.Process {
	r.t.Helper()

	var env []string
	if point != "" {
		env = append(env, crash.EnvPoint+"="+point)
	}

	// A short lease keeps the wait after a crash proportionate: a killed runner
	// leaves its lease behind, and the next run cannot start until it lapses.
	args := []string{
		"up",
		"--dsn", shared.DSN(r.database),
		"--dir", r.dir,
		"--on-locked", "wait",
		"--lease-ttl", "3s",
	}

	proc, err := harness.Start(migBin, args, env)
	if err != nil {
		r.t.Fatalf("start mig: %v", err)
	}

	r.t.Cleanup(proc.Cleanup)

	return proc
}

// run applies the migration and requires it to succeed.
func (r *run) run() exec.Summary {
	r.t.Helper()

	proc := r.start("")

	code, err := proc.Wait(2 * time.Minute)
	if err != nil {
		r.t.Fatalf("wait for mig: %v", err)
	}

	if code != 0 {
		r.t.Fatalf("mig exited %d\nstdout: %s\nstderr: %s", code, proc.Stdout(), proc.Stderr())
	}

	r.drain()

	return r.summaryOf(proc)
}

// crash runs the migrator with a crash point armed and requires it to die.
func (r *run) crash(point string) {
	r.t.Helper()

	proc := r.start(point)

	code, err := proc.Wait(2 * time.Minute)
	if err != nil {
		r.t.Fatalf("wait for mig killed at %q: %v", point, err)
	}

	// A process killed by a signal reports -1; a clean exit means the crash
	// point was never reached and the test proved nothing.
	if code != -1 {
		r.t.Fatalf("mig exited %d rather than dying at %q\nstdout: %s\nstderr: %s",
			code, point, proc.Stdout(), proc.Stderr())
	}

	r.drain()
}

// killMidBuild kills the migrator part-way through the index build, leaving
// the invalid index behind.
//
// The build is pinned open rather than raced. CREATE INDEX CONCURRENTLY creates
// its catalog entry, commits it invalid, and only then waits for transactions
// holding a conflicting lock on the table; holding one keeps it in that state
// for as long as the test needs. Racing a build that takes a few hundred
// milliseconds would leave the kill landing after it, sometimes.
func (r *run) killMidBuild() {
	r.t.Helper()

	release := r.blockIndexBuild()

	proc := r.start("")

	r.waitForIndexBuild()

	if err := proc.Kill(); err != nil {
		r.t.Fatalf("kill mig: %v", err)
	}

	if _, err := proc.Wait(30 * time.Second); err != nil {
		r.t.Fatalf("wait for killed mig: %v", err)
	}

	// Released before draining, since the blocker is itself a backend.
	release()

	r.drain()
}

// blockIndexBuild holds a lock that CREATE INDEX CONCURRENTLY waits on, and
// returns the function that lets it proceed.
//
// ROW EXCLUSIVE is what an ordinary writer holds. It does not stop the build
// from starting, so the invalid catalog entry is created and committed, and it
// does stop the build from finishing.
func (r *run) blockIndexBuild() func() {
	r.t.Helper()

	ctx := context.Background()

	db, err := shared.Open(ctx, r.database)
	if err != nil {
		r.t.Fatalf("open blocker: %v", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		r.t.Fatalf("begin blocker: %v", err)
	}

	if _, err := tx.ExecContext(ctx, "LOCK TABLE users IN ROW EXCLUSIVE MODE"); err != nil {
		r.t.Fatalf("lock users: %v", err)
	}

	return func() {
		if err := tx.Rollback(); err != nil {
			r.t.Errorf("release blocker: %v", err)
		}

		if err := db.Close(); err != nil {
			r.t.Errorf("close blocker: %v", err)
		}
	}
}

// waitForIndexBuild blocks until the server is executing the index build, so
// the kill lands during it rather than before it starts.
func (r *run) waitForIndexBuild() {
	r.t.Helper()

	deadline := time.Now().Add(60 * time.Second)

	for {
		backends, err := shared.Backends(r.t.Context(), r.database)
		if err != nil {
			r.t.Fatalf("list backends: %v", err)
		}

		for _, backend := range backends {
			if strings.Contains(backend.Query, "CREATE INDEX CONCURRENTLY") {
				return
			}
		}

		if !time.Now().Before(deadline) {
			r.t.Fatalf("index build never started: %v", backends)
		}

		time.Sleep(5 * time.Millisecond)
	}
}

// drain waits for the server-side backends of a dead runner to disappear.
// Restarting before they do races a backend that may still finish the build.
func (r *run) drain() {
	r.t.Helper()

	if err := shared.WaitBackendsGone(r.t.Context(), r.database, 2*time.Minute); err != nil {
		r.t.Fatalf("backends did not drain: %v", err)
	}
}

// converge re-runs until the migration is applied, and requires it to take one
// run. Needing more would mean recovery is not converging but oscillating.
func (r *run) converge() {
	r.t.Helper()

	summary := r.run()
	if summary.Applied+summary.Skipped != summary.StepsTotal {
		r.t.Fatalf("summary does not account for every step: %+v", summary)
	}
}

// requireInvalidIndex asserts the interrupted build left the state the repair
// exists to clear. Without it, a test could pass because the build never
// started.
func (r *run) requireInvalidIndex() {
	r.t.Helper()

	r.withDB(func(db *sql.DB) {
		index, err := catalog.LookupIndex(r.t.Context(), db, "", indexName)
		if err != nil {
			r.t.Fatalf("look up index: %v", err)
		}

		if !index.Exists {
			r.t.Fatal("interrupted build left no index at all")
		}

		if index.Usable() {
			r.t.Fatal("interrupted build left a usable index; the kill was too late")
		}
	})
}

// assertMatchesGolden compares the converged database against one produced by
// an uninterrupted run.
func (r *run) assertMatchesGolden() {
	r.t.Helper()

	ctx := r.t.Context()
	want := golden(r.t)

	r.withDB(func(db *sql.DB) {
		problems, err := harness.Problems(ctx, db)
		if err != nil {
			r.t.Fatalf("inspect database: %v", err)
		}

		if len(problems) > 0 {
			r.t.Fatalf("converged database has problems:\n  %s", strings.Join(problems, "\n  "))
		}

		state, err := harness.Capture(ctx, db)
		if err != nil {
			r.t.Fatalf("capture state: %v", err)
		}

		if state.Fingerprint != want.Fingerprint {
			r.t.Fatalf("schema differs from an uninterrupted run:\n%s",
				harness.DiffSchema(want.Schema, state.Schema))
		}

		if state.Data != want.Data {
			r.t.Fatalf("data checksum is %s, want %s", state.Data, want.Data)
		}
	})
}

// assertSecondRunDoesNothing is the convergence assertion. Asserting only that
// the schema matches would accept a migrator that reapplies its work forever.
func (r *run) assertSecondRunDoesNothing() {
	r.t.Helper()

	summary := r.run()

	if summary.Applied != 0 {
		r.t.Fatalf("a converged database still applied %d steps: %+v", summary.Applied, summary)
	}

	if summary.Repaired != 0 {
		r.t.Fatalf("a converged database still repaired %d steps: %+v", summary.Repaired, summary)
	}
}

// summaryOf decodes the machine-readable summary the run wrote to stdout.
func (r *run) summaryOf(proc *harness.Process) exec.Summary {
	r.t.Helper()

	stdout := strings.TrimSpace(proc.Stdout())

	lines := strings.Split(stdout, "\n")
	if len(lines) == 0 || lines[len(lines)-1] == "" {
		r.t.Fatalf("mig printed no summary\nstdout: %q\nstderr: %s", stdout, proc.Stderr())
	}

	var summary exec.Summary

	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &summary); err != nil {
		r.t.Fatalf("decode summary %q: %v", lines[len(lines)-1], err)
	}

	return summary
}

// withDB connects to the run's database and closes the pool before returning.
//
// The pool is not left open for the test's lifetime, because a later restart
// waits for the database to have no backends at all and would otherwise be
// waiting for this one.
func (r *run) withDB(fn func(*sql.DB)) {
	r.t.Helper()

	db, err := shared.Open(r.t.Context(), r.database)
	if err != nil {
		r.t.Fatalf("open %q: %v", r.database, err)
	}

	defer func() {
		if err := db.Close(); err != nil {
			r.t.Errorf("close %q: %v", r.database, err)
		}
	}()

	fn(db)
}

// golden captures the state of an uninterrupted run, once, and reuses it.
func golden(t *testing.T) harness.State {
	t.Helper()

	goldenOnce.Do(func() {
		env := &run{t: t, database: newGoldenDatabase(), dir: t.TempDir()}

		if err := os.WriteFile(filepath.Join(env.dir, migrationFile), []byte(migrationBody), 0o600); err != nil {
			t.Fatalf("write golden migration: %v", err)
		}

		env.run()

		db, err := shared.Open(context.Background(), env.database)
		if err != nil {
			t.Fatalf("open golden database: %v", err)
		}

		defer func() {
			_ = db.Close()
		}()

		state, err := harness.Capture(context.Background(), db)
		if err != nil {
			t.Fatalf("capture golden state: %v", err)
		}

		goldenState = state
	})

	return goldenState
}

// newGoldenDatabase clones a database that outlives the test that created it,
// since the golden state is shared by the whole package.
func newGoldenDatabase() string {
	name, err := shared.Clone(context.Background(), harness.Template)
	if err != nil {
		panic("clone golden template: " + err.Error())
	}

	return name
}
