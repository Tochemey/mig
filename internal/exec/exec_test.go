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

package exec_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"testing/fstest"
	"time"

	"github.com/tochemey/mig/internal/exec"
	"github.com/tochemey/mig/internal/lease"
	"github.com/tochemey/mig/internal/ledger"
	"github.com/tochemey/mig/internal/plan"
	"github.com/tochemey/mig/test/faultdb"
	"github.com/tochemey/mig/test/harness"
)

// shared is the container for this package, or nil when docker is absent.
var shared *harness.Harness

// seedRows is enough rows for the fixture backfill to take several batches.
const seedRows = 5

// TestMain brings up one container for the package.
//
// The template is seeded, because a backfill over an empty table finishes
// before it opens a single batch and would leave that whole path unrun.
func TestMain(m *testing.M) {
	os.Exit(harness.Main(m, seedRows, func(_ context.Context, h *harness.Harness) error {
		shared = h
		return nil
	}))
}

// One step of each kind: transactional, concurrent, and batched. Between them
// they take every branch the executor has.
var migrations = fstest.MapFS{
	"20240817120000_add_email.sql": &fstest.MapFile{Data: []byte(
		"-- +mig step: add_email\n" +
			"ALTER TABLE users ADD COLUMN email text;\n" +
			"\n" +
			"-- +mig step: index_email\n" +
			"-- +mig notx\n" +
			"CREATE INDEX CONCURRENTLY idx_users_email ON users (email);\n" +
			"\n" +
			"-- +mig step: fill_email\n" +
			"-- +mig backfill: table=users key=id batch=2\n" +
			"-- +mig satisfied: sql(SELECT NOT EXISTS " +
			"(SELECT 1 FROM users WHERE email IS NULL))\n" +
			"UPDATE users SET email = id || '@example.test'\n" +
			" WHERE id BETWEEN :cursor_lo AND :cursor_hi AND email IS NULL;\n")},
}

// The executor is driven directly here rather than through the command, because
// a fault has to be attached to the pool before the run starts.

// TestRunApplies covers the ordinary path, and the convergence that follows: a
// second run over an already-migrated database applies nothing.
func TestRunApplies(t *testing.T) {
	database := newDatabase(t)
	db := openPlain(t, database)

	summary := mustRun(t, database, db, loadPlan(t))

	if summary.Applied != 3 {
		t.Fatalf("applied %d steps, want 3: %+v", summary.Applied, summary)
	}

	if again := mustRun(t, database, db, loadPlan(t)); again.Applied != 0 {
		t.Fatalf("the second run applied %d steps: %+v", again.Applied, again)
	}
}

// TestRunReportsEveryFailedWrite is the point of injecting faults: each of the
// executor's writes has a failure path that a crash cannot reach, because the
// process has to survive to report it.
//
// Every case must surface the failure. A write that failed and was reported as
// success is the one outcome that leaves a database nobody can reason about.
func TestRunReportsEveryFailedWrite(t *testing.T) {
	cases := map[string][]faultdb.Fault{
		"recording the migration": {{Op: faultdb.OpExec, Match: "INSERT INTO mig.migrations"}},
		"recording the step":      {{Op: faultdb.OpExec, Match: "INSERT INTO mig.steps"}},
		"the fence guard":         {{Op: faultdb.OpQuery, Match: "FROM mig.lease"}},
		"marking it running":      {{Op: faultdb.OpQuery, Match: "UPDATE mig.steps"}},
		"counting the attempt":    {{Op: faultdb.OpQuery, Match: "SET attempts"}},
		"the migration status":    {{Op: faultdb.OpQuery, Match: "UPDATE mig.migrations"}},
		"the batch lock timeout":  {{Op: faultdb.OpExec, Match: "set_config('lock_timeout'"}},
		"reading the checkpoint":  {{Op: faultdb.OpQuery, Match: "checkpoint"}},
		"opening a transaction":   {{Op: faultdb.OpBegin}},
		"committing":              {{Op: faultdb.OpCommit}},

		// A rollback only runs once something else has failed, so the two are
		// injected together: this is the cleanup path failing after the work
		// already went wrong, which is where a swallowed error would hide.
		"rolling back after a failure": {
			{Op: faultdb.OpExec, Match: "INSERT INTO mig.migrations"},
			{Op: faultdb.OpRollback},
		},
	}

	for name, faultSet := range cases {
		t.Run(name, func(t *testing.T) {
			database := newDatabase(t)
			faults := faultdb.NewFaults(faultSet...)
			db := openFaulted(t, database, faults)

			_, err := run(t, database, db, loadPlan(t))
			if err == nil {
				t.Fatalf("the run reported success with %q broken", name)
			}

			if faults.Fired() == 0 {
				t.Fatalf("no fault was taken, so %q proves nothing", name)
			}
		})
	}
}

// TestRunRefusesAnUnbuildableStepBeforeApplyingAnything is the plan-time
// refusal, checked where it matters: the first migration must not be applied
// before the second is found to be unreconcilable.
func TestRunRefusesAnUnbuildableStepBeforeApplyingAnything(t *testing.T) {
	database := newDatabase(t)
	db := openPlain(t, database)

	loaded, err := plan.LoadFS(fstest.MapFS{
		"20240817120000_first.sql": &fstest.MapFile{Data: []byte(
			"ALTER TABLE users ADD COLUMN email text;\n")},
		"20240817120001_second.sql": &fstest.MapFile{Data: []byte(
			"-- +mig step: vacuum\n-- +mig notx\nVACUUM users;\n")},
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if _, err := run(t, database, db, loaded); err == nil {
		t.Fatal("the run accepted a step it cannot reconcile")
	}

	var exists bool

	const query = `SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		 WHERE table_name = 'users' AND column_name = 'email')`

	if err := db.QueryRowContext(t.Context(), query).Scan(&exists); err != nil {
		t.Fatalf("look for the column: %v", err)
	}

	if exists {
		t.Fatal("the first migration was applied before the second was refused")
	}
}

// TestRunDetectsChecksumDrift covers a migration edited after it was applied,
// which means the database no longer matches what the file says it did.
func TestRunDetectsChecksumDrift(t *testing.T) {
	database := newDatabase(t)
	db := openPlain(t, database)

	mustRun(t, database, db, loadPlan(t))

	edited, err := plan.LoadFS(fstest.MapFS{
		"20240817120000_add_email.sql": &fstest.MapFile{Data: []byte(
			"-- +mig step: add_email\n" +
				"ALTER TABLE users ADD COLUMN email varchar(320);\n" +
				"\n" +
				"-- +mig step: index_email\n" +
				"-- +mig notx\n" +
				"CREATE INDEX CONCURRENTLY idx_users_email ON users (email);\n")},
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	_, err = run(t, database, db, edited)
	if !errors.Is(err, exec.ErrChecksumDrift) {
		t.Fatalf("the run returned %v, want ErrChecksumDrift", err)
	}

	// The escape hatch, for a team that has decided the edit was harmless.
	if _, err := runWith(t, database, db, edited, func(o *exec.Options) { o.AllowDrift = true }); err != nil {
		t.Fatalf("run with --allow-drift: %v", err)
	}
}

// TestPendingReadsWithoutWriting covers the startup check against every state a
// database can be in, including one no run has ever touched.
func TestPendingReadsWithoutWriting(t *testing.T) {
	database := newDatabase(t)
	db := openPlain(t, database)

	outstanding, err := exec.Pending(t.Context(), db, loadPlan(t))
	if err != nil {
		t.Fatalf("pending: %v", err)
	}

	if len(outstanding) != 3 {
		t.Fatalf("pending is %v, want every step", outstanding)
	}

	mustRun(t, database, db, loadPlan(t))

	outstanding, err = exec.Pending(t.Context(), db, loadPlan(t))
	if err != nil {
		t.Fatalf("pending after the run: %v", err)
	}

	if len(outstanding) != 0 {
		t.Fatalf("pending is %v after a converged run", outstanding)
	}
}

// TestPendingReportsEveryFailedRead covers the check failing rather than
// reporting work outstanding. A service retries one and refuses to start on the
// other, so the two must never be confused.
func TestPendingReportsEveryFailedRead(t *testing.T) {
	cases := map[string]faultdb.Fault{
		"looking for the ledger": {Op: faultdb.OpQuery, Match: "to_regclass"},
		"asking the catalog":     {Op: faultdb.OpQuery, Match: "pg_attribute"},
		"reading the ledger":     {Op: faultdb.OpQuery, Match: "FROM mig.steps"},
	}

	for name, fault := range cases {
		t.Run(name, func(t *testing.T) {
			database := newDatabase(t)

			// The ledger has to exist, and the work must not: the fall back to
			// the ledger only happens for a step the catalog says is absent.
			control := openPlain(t, database)

			if err := ledger.EnsureSchema(t.Context(), control); err != nil {
				t.Fatalf("ensure schema: %v", err)
			}

			faults := faultdb.NewFaults(fault)
			db := openFaulted(t, database, faults)

			if _, err := exec.Pending(t.Context(), db, loadPlan(t)); err == nil {
				t.Fatalf("pending reported success with %q broken", name)
			}

			if faults.Fired() == 0 {
				t.Fatalf("no fault was taken, so %q proves nothing", name)
			}
		})
	}
}

// TestPendingReportsAClosedPool covers the check being unable to take a
// connection at all, which a service must not read as work outstanding.
func TestPendingReportsAClosedPool(t *testing.T) {
	database := newDatabase(t)
	db := openPlain(t, database)

	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, err := exec.Pending(t.Context(), db, loadPlan(t)); err == nil {
		t.Fatal("pending accepted a closed pool")
	}
}

// TestPendingRefusesAnUnbuildablePlan covers the check meeting the same step a
// run would refuse. Reporting it as pending would have a service wait forever
// for a migration that can never be applied.
func TestPendingRefusesAnUnbuildablePlan(t *testing.T) {
	database := newDatabase(t)
	db := openPlain(t, database)

	loaded, err := plan.LoadFS(fstest.MapFS{
		"20240817120000_vacuum.sql": &fstest.MapFile{Data: []byte(
			"-- +mig step: vacuum\n-- +mig notx\nVACUUM users;\n")},
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if _, err := exec.Pending(t.Context(), db, loaded); err == nil {
		t.Fatal("pending accepted a step that cannot be reconciled")
	}
}

// loadPlan builds the fixture plan.
func loadPlan(t *testing.T) *plan.Plan {
	t.Helper()

	loaded, err := plan.LoadFS(migrations)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	return loaded
}

// mustRun applies the plan and fails the test if it does not converge.
func mustRun(t *testing.T, database string, db *sql.DB, p *plan.Plan) exec.Summary {
	t.Helper()

	summary, err := run(t, database, db, p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	return summary
}

// run applies the plan under a real lease, as the command does.
func run(t *testing.T, database string, db *sql.DB, p *plan.Plan) (exec.Summary, error) {
	t.Helper()

	return runWith(t, database, db, p, func(*exec.Options) {})
}

// runWith applies the plan with the options a case needs.
func runWith(t *testing.T, database string, db *sql.DB, p *plan.Plan,
	configure func(*exec.Options)) (exec.Summary, error) {
	t.Helper()

	// The schema and the lease are set up on a pool with no faults, so that a
	// case breaks the run rather than the scaffolding around it.
	control := openPlain(t, database)

	if err := ledger.EnsureSchema(t.Context(), control); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	held, err := lease.Acquire(t.Context(), control, lease.Config{
		Owner:    lease.NewOwner(),
		TTL:      time.Minute,
		OnLocked: lease.Fail,
	})
	if err != nil {
		t.Fatalf("acquire the lease: %v", err)
	}

	opts := exec.Options{Fence: held.Fence()}
	configure(&opts)

	summary, runErr := exec.New(db, db, opts).Run(t.Context(), p)

	if err := held.Release(t.Context()); err != nil {
		t.Fatalf("release the lease: %v", err)
	}

	return summary, runErr
}

// newDatabase gives the test its own database.
func newDatabase(t *testing.T) string {
	t.Helper()

	if shared == nil {
		t.Skip("postgres container not available")
	}

	name, err := shared.Clone(t.Context(), harness.Template)
	if err != nil {
		t.Fatalf("clone template: %v", err)
	}

	t.Cleanup(func() {
		if err := shared.DropDatabase(context.Background(), name); err != nil {
			t.Errorf("drop clone %q: %v", name, err)
		}
	})

	return name
}

// openPlain connects without faults.
func openPlain(t *testing.T, database string) *sql.DB {
	t.Helper()

	db, err := shared.Open(t.Context(), database)
	if err != nil {
		t.Fatalf("open %q: %v", database, err)
	}

	closeAfter(t, db)

	return db
}

// openFaulted connects through the fault-injecting driver.
//
// Every pool gets its own driver name, because database/sql keeps its drivers
// in a process-wide registry that cannot be replaced.
func openFaulted(t *testing.T, database string, faults *faultdb.Faults) *sql.DB {
	t.Helper()

	db, err := faultdb.Open(fmt.Sprintf("faulted_%s_%s", t.Name(), database),
		shared.DSN(database), faults)
	if err != nil {
		t.Fatalf("open %q with faults: %v", database, err)
	}

	closeAfter(t, db)

	return db
}

// closeAfter returns the pool when the test ends.
func closeAfter(t *testing.T, db *sql.DB) {
	t.Helper()

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close pool: %v", err)
		}
	})
}
