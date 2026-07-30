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

package step_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"

	"github.com/tochemey/mig/internal/catalog"
	"github.com/tochemey/mig/internal/parse"
	"github.com/tochemey/mig/internal/step"
	"github.com/tochemey/mig/test/harness"
)

// shared is the container for this package, or nil when docker is absent.
var shared *harness.Harness

// TestMain brings up one container for the package.
func TestMain(m *testing.M) {
	os.Exit(harness.Main(m, 0, func(_ context.Context, h *harness.Harness) error {
		shared = h
		return nil
	}))
}

// TestNewDDLNoTxRequiresAPredicate is the refusal that keeps a step which
// cannot recognise its own finished work from ever running.
func TestNewDDLNoTxRequiresAPredicate(t *testing.T) {
	_, err := step.NewDDLNoTx(step.Meta{Name: "add_enum_value"}, nil, nil)

	if !errors.Is(err, step.ErrNoPredicate) {
		t.Fatalf("construction returned %v, want ErrNoPredicate", err)
	}
}

// TestDDLNoTxAppliesAndRepairs covers the whole non-transactional cycle against
// a real database: unsatisfied, applied, satisfied, and a partial state cleared.
func TestDDLNoTxAppliesAndRepairs(t *testing.T) {
	conn := newConn(t)
	ctx := t.Context()

	work := noTxStep(t, "CREATE INDEX CONCURRENTLY idx_users_name ON users (name)")

	if satisfied(t, work, conn) {
		t.Fatal("satisfied before the index existed")
	}

	// Repair is safe before anything has run.
	if err := work.Repair(ctx, conn); err != nil {
		t.Fatalf("repair before apply: %v", err)
	}

	if err := work.Apply(ctx, conn); err != nil {
		t.Fatalf("apply: %v", err)
	}

	if !satisfied(t, work, conn) {
		t.Fatal("not satisfied once the index was built")
	}

	// Repair leaves a finished index alone: that index is the work.
	if err := work.Repair(ctx, conn); err != nil {
		t.Fatalf("repair after apply: %v", err)
	}

	if !satisfied(t, work, conn) {
		t.Fatal("repair dropped a finished index")
	}
}

// TestDDLNoTxRepairClearsAnInterruptedBuild covers the state a killed build
// leaves. Postgres will not resume it, so it has to be dropped before the step
// can run again.
func TestDDLNoTxRepairClearsAnInterruptedBuild(t *testing.T) {
	conn := newConn(t)
	ctx := t.Context()

	exec(t, conn, "INSERT INTO users (id, name, legacy_email) VALUES (1, 'a', 'x'), (2, 'b', 'x')")

	_, err := conn.ExecContext(ctx,
		"CREATE UNIQUE INDEX CONCURRENTLY idx_users_legacy_email ON users (legacy_email)")
	if err == nil {
		t.Fatal("a unique index over duplicate values was accepted")
	}

	work := noTxStep(t, "CREATE INDEX CONCURRENTLY idx_users_legacy_email ON users (legacy_email)")

	before, err := catalog.LookupIndex(ctx, conn, "", "idx_users_legacy_email")
	if err != nil {
		t.Fatalf("look up: %v", err)
	}

	if !before.Exists || before.Usable() {
		t.Fatalf("the failed build left %+v, want an unusable index", before)
	}

	if err := work.Repair(ctx, conn); err != nil {
		t.Fatalf("repair: %v", err)
	}

	after, err := catalog.LookupIndex(ctx, conn, "", "idx_users_legacy_email")
	if err != nil {
		t.Fatalf("look up after repair: %v", err)
	}

	if after.Exists {
		t.Fatal("repair left the invalid index in place")
	}

	// Repair is re-entrant: the recovery path can itself be killed and re-run.
	if err := work.Repair(ctx, conn); err != nil {
		t.Fatalf("second repair: %v", err)
	}
}

// TestDDLNoTxReportsApplyFailure covers SQL the server rejects.
func TestDDLNoTxReportsApplyFailure(t *testing.T) {
	conn := newConn(t)

	work := noTxStep(t, "CREATE INDEX CONCURRENTLY idx_absent ON no_such_table (id)")

	if err := work.Apply(t.Context(), conn); err == nil {
		t.Fatal("apply accepted an index over a table that does not exist")
	}
}

// TestDDLTxNeedsNoPredicate covers the difference between the two kinds. A
// transactional step commits its ledger row with its DDL, so it has no window
// to reconcile and reports false rather than refusing to be built.
func TestDDLTxNeedsNoPredicate(t *testing.T) {
	conn := newConn(t)
	ctx := t.Context()

	statements, err := parse.Parse("ALTER TABLE users ADD COLUMN email text")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	work := step.NewDDLTx(step.Meta{Name: "add_email", Kind: step.KindDDLTx}, statements, nil)

	done, err := work.Satisfied(ctx, conn)
	if err != nil {
		t.Fatalf("satisfied: %v", err)
	}

	if done {
		t.Fatal("a step with no predicate reported its work as done")
	}

	// Repair is a no-op: a transactional step cannot be left part-way through.
	if err := work.Repair(ctx, conn); err != nil {
		t.Fatalf("repair: %v", err)
	}

	if work.Meta().Name != "add_email" {
		t.Fatalf("meta is %+v", work.Meta())
	}
}

// TestDDLTxUsesAPredicateWhenItHasOne covers the other half: an inferable
// transactional step still answers from the catalog.
func TestDDLTxUsesAPredicateWhenItHasOne(t *testing.T) {
	conn := newConn(t)
	ctx := t.Context()

	statements, err := parse.Parse("CREATE INDEX idx_users_name ON users (name)")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	check := func(ctx context.Context, q catalog.Querier) (bool, error) {
		index, err := catalog.LookupIndex(ctx, q, "", "idx_users_name")
		if err != nil {
			return false, err
		}

		return index.Usable(), nil
	}

	work := step.NewDDLTx(step.Meta{Name: "index_name"}, statements, check)

	if done, err := work.Satisfied(ctx, conn); err != nil || done {
		t.Fatalf("satisfied before the index existed: %v %v", done, err)
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	if err := work.ApplyTx(ctx, tx); err != nil {
		t.Fatalf("apply: %v", err)
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if done, err := work.Satisfied(ctx, conn); err != nil || !done {
		t.Fatalf("not satisfied once the index existed: %v %v", done, err)
	}
}

// TestDDLTxRollsBackOnFailure covers a step whose second statement fails: the
// transaction is the caller's, so nothing it did survives.
func TestDDLTxRollsBackOnFailure(t *testing.T) {
	conn := newConn(t)
	ctx := t.Context()

	statements, err := parse.Parse(`
ALTER TABLE users ADD COLUMN email text;
ALTER TABLE no_such_table ADD COLUMN email text;
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	work := step.NewDDLTx(step.Meta{Name: "two_statements"}, statements, nil)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	if err := work.ApplyTx(ctx, tx); err == nil {
		t.Fatal("apply accepted a statement against a table that does not exist")
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	var exists bool

	const query = `SELECT exists(
        SELECT 1 FROM information_schema.columns
         WHERE table_name = 'users' AND column_name = 'email')`

	if err := conn.QueryRowContext(ctx, query).Scan(&exists); err != nil {
		t.Fatalf("check for column: %v", err)
	}

	if exists {
		t.Fatal("the first statement survived a rolled-back transaction")
	}
}

// noTxStep builds a non-transactional step with an inferred predicate.
func noTxStep(t *testing.T, sql string) *step.DDLNoTx {
	t.Helper()

	statements, err := parse.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}

	target := statements[0].Target

	check := func(ctx context.Context, q catalog.Querier) (bool, error) {
		index, err := catalog.LookupIndex(ctx, q, target.Schema, target.Name)
		if err != nil {
			return false, err
		}

		return index.Usable(), nil
	}

	work, err := step.NewDDLNoTx(step.Meta{Name: "step", Kind: step.KindDDLNoTx}, statements, check)
	if err != nil {
		t.Fatalf("build step: %v", err)
	}

	return work
}

// satisfied evaluates a step's predicate or fails the test.
func satisfied(t *testing.T, work step.Step, conn *sql.Conn) bool {
	t.Helper()

	done, err := work.Satisfied(t.Context(), conn)
	if err != nil {
		t.Fatalf("satisfied: %v", err)
	}

	return done
}

// newConn pins a connection to a database of the test's own.
func newConn(t *testing.T) *sql.Conn {
	t.Helper()

	if shared == nil {
		t.Skip("postgres container not available")
	}

	name, err := shared.Clone(t.Context(), harness.Template)
	if err != nil {
		t.Fatalf("clone template: %v", err)
	}

	db, err := shared.Open(t.Context(), name)
	if err != nil {
		t.Fatalf("open clone: %v", err)
	}

	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatalf("pin connection: %v", err)
	}

	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close connection: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Errorf("close pool: %v", err)
		}

		if err := shared.DropDatabase(context.Background(), name); err != nil {
			t.Errorf("drop clone %q: %v", name, err)
		}
	})

	return conn
}

// exec runs a statement or fails the test.
func exec(t *testing.T, conn *sql.Conn, stmt string) {
	t.Helper()

	if _, err := conn.ExecContext(t.Context(), stmt); err != nil {
		t.Fatalf("exec %q: %v", stmt, err)
	}
}
