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

package predicate_test

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/tochemey/mig/internal/parse"
	"github.com/tochemey/mig/internal/predicate"
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

// TestCreateIndexPredicate covers the three states an index build passes
// through, since only the last of them means the work is done.
func TestCreateIndexPredicate(t *testing.T) {
	db := newDatabase(t)
	check := infer(t, "CREATE INDEX CONCURRENTLY idx_users_name ON users (name)")

	if satisfied(t, check, db) {
		t.Fatal("satisfied before the index existed")
	}

	// An interrupted build leaves an index that exists and is not valid.
	exec(t, db, "INSERT INTO users (id, name, legacy_email) VALUES (1, 'a', 'x'), (2, 'b', 'x')")

	_, err := db.ExecContext(t.Context(),
		"CREATE UNIQUE INDEX CONCURRENTLY idx_users_legacy_email ON users (legacy_email)")
	if err == nil {
		t.Fatal("a unique index over duplicate values was accepted")
	}

	partial := infer(t, "CREATE INDEX CONCURRENTLY idx_users_legacy_email ON users (legacy_email)")

	if satisfied(t, partial, db) {
		t.Fatal("satisfied by an index left behind by an interrupted build")
	}

	exec(t, db, "CREATE INDEX idx_users_name ON users (name)")

	if !satisfied(t, check, db) {
		t.Fatal("not satisfied by a finished index")
	}
}

// TestDropIndexPredicate covers the mirror case: the work is done when the
// index is gone.
func TestDropIndexPredicate(t *testing.T) {
	db := newDatabase(t)
	check := infer(t, "DROP INDEX CONCURRENTLY IF EXISTS idx_users_name")

	if !satisfied(t, check, db) {
		t.Fatal("not satisfied when the index was never there")
	}

	exec(t, db, "CREATE INDEX idx_users_name ON users (name)")

	if satisfied(t, check, db) {
		t.Fatal("satisfied while the index still existed")
	}

	exec(t, db, "DROP INDEX idx_users_name")

	if !satisfied(t, check, db) {
		t.Fatal("not satisfied once the index was dropped")
	}
}

// TestMultiStatementPredicate covers a step holding more than one statement:
// it is satisfied only when every statement is, since a partly-done step would
// otherwise be recorded as finished.
func TestMultiStatementPredicate(t *testing.T) {
	db := newDatabase(t)

	check := infer(t, `
CREATE INDEX CONCURRENTLY idx_users_name ON users (name);
CREATE INDEX CONCURRENTLY idx_users_legacy_email ON users (legacy_email);
`)

	exec(t, db, "CREATE INDEX idx_users_name ON users (name)")

	if satisfied(t, check, db) {
		t.Fatal("satisfied with only the first statement applied")
	}

	exec(t, db, "CREATE INDEX idx_users_legacy_email ON users (legacy_email)")

	if !satisfied(t, check, db) {
		t.Fatal("not satisfied once both statements were applied")
	}
}

// TestInferDeclinesWhatItCannotCheck covers the statements that carry no
// predicate. Guessing one would be worse than admitting there is none.
func TestInferDeclinesWhatItCannotCheck(t *testing.T) {
	cases := map[string]string{
		"alter type": "ALTER TYPE mood ADD VALUE 'ok'",
		"update":     "UPDATE users SET name = 'x'",
		// One unclassified statement makes the whole step uninferable.
		"mixed": "CREATE INDEX CONCURRENTLY idx ON users (name); VACUUM users;",
	}

	for name, sql := range cases {
		t.Run(name, func(t *testing.T) {
			statements, err := parse.Parse(sql)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}

			if predicate.Infer(statements) != nil {
				t.Fatalf("inferred a predicate for %q", sql)
			}
		})
	}

	if predicate.Infer(nil) != nil {
		t.Fatal("inferred a predicate for no statements at all")
	}
}

// TestSQLPredicate covers the escape hatch, which is what makes refusing an
// uninferable step reasonable rather than a dead end.
func TestSQLPredicate(t *testing.T) {
	db := newDatabase(t)

	check := predicate.SQL(
		"SELECT EXISTS (SELECT 1 FROM information_schema.columns " +
			"WHERE table_name = 'users' AND column_name = 'email')")

	if satisfied(t, check, db) {
		t.Fatal("satisfied before the column existed")
	}

	exec(t, db, "ALTER TABLE users ADD COLUMN email text")

	if !satisfied(t, check, db) {
		t.Fatal("not satisfied once the column existed")
	}
}

// TestSQLPredicateReportsFailure covers a predicate that does not run, which
// must not be read as "the work is not done" and cause it to run again.
func TestSQLPredicateReportsFailure(t *testing.T) {
	db := newDatabase(t)

	if _, err := predicate.SQL("SELECT nonsense FROM nowhere")(t.Context(), db); err == nil {
		t.Fatal("a broken predicate reported a result")
	}
}

// TestPredicateReportsQueryFailure covers the inferred predicates failing to
// reach the database.
func TestPredicateReportsQueryFailure(t *testing.T) {
	db := newDatabase(t)

	create := infer(t, "CREATE INDEX CONCURRENTLY idx_users_name ON users (name)")
	drop := infer(t, "DROP INDEX CONCURRENTLY idx_users_name")

	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	for name, check := range map[string]step.Predicate{"create": create, "drop": drop} {
		t.Run(name, func(t *testing.T) {
			if _, err := check(t.Context(), db); err == nil {
				t.Fatal("predicate on a closed database reported a result")
			}
		})
	}
}

// infer builds the predicate for a step's SQL, requiring one to exist.
func infer(t *testing.T, sql string) step.Predicate {
	t.Helper()

	statements, err := parse.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}

	check := predicate.Infer(statements)
	if check == nil {
		t.Fatalf("no predicate inferred for %q", sql)
	}

	return check
}

// satisfied evaluates a predicate or fails the test.
func satisfied(t *testing.T, check step.Predicate, db *sql.DB) bool {
	t.Helper()

	ok, err := check(t.Context(), db)
	if err != nil {
		t.Fatalf("evaluate predicate: %v", err)
	}

	return ok
}

// newDatabase gives the test its own database holding the fixture.
func newDatabase(t *testing.T) *sql.DB {
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

	t.Cleanup(func() {
		_ = db.Close()

		if err := shared.DropDatabase(context.Background(), name); err != nil {
			t.Errorf("drop clone %q: %v", name, err)
		}
	})

	return db
}

// exec runs a statement or fails the test.
func exec(t *testing.T, db *sql.DB, stmt string) {
	t.Helper()

	if _, err := db.ExecContext(t.Context(), stmt); err != nil {
		t.Fatalf("exec %q: %v", stmt, err)
	}
}
