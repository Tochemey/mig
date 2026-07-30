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
	"strings"
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
		"update":          "UPDATE users SET name = 'x'",
		"set default":     "ALTER TABLE users ALTER COLUMN name SET DEFAULT 'x'",
		"unnamed check":   "ALTER TABLE users ADD CHECK (name <> '')",
		"several actions": "ALTER TABLE users ADD COLUMN a text, ADD COLUMN b text",
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
			"WHERE table_name = 'users' AND column_name = 'email')").Satisfied

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

	broken := predicate.SQL("SELECT nonsense FROM nowhere")

	if _, err := broken.Satisfied(t.Context(), db); err == nil {
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

	check := inferCheck(t, sql)

	return check.Satisfied
}

// inferCheck builds the whole check, predicate and description together.
func inferCheck(t *testing.T, sql string) *predicate.Check {
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
		if err := db.Close(); err != nil {
			t.Errorf("close pool: %v", err)
		}

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

// TestInferCoversTheWholeTable walks every statement the design says carries a
// predicate, against a real database, through the three states each passes
// through: before the work, after it, and — where the two differ — part-way.
func TestInferCoversTheWholeTable(t *testing.T) {
	cases := []struct {
		name    string
		sql     string
		prepare []string
		satisfy []string
	}{
		{
			name:    "add column",
			sql:     "ALTER TABLE users ADD COLUMN email text",
			satisfy: []string{"ALTER TABLE users ADD COLUMN email text"},
		},
		{
			name:    "drop column",
			sql:     "ALTER TABLE users DROP COLUMN legacy_email",
			satisfy: []string{"ALTER TABLE users DROP COLUMN legacy_email"},
		},
		{
			name: "add constraint",
			sql:  "ALTER TABLE users ADD CONSTRAINT users_name_len CHECK (length(name) > 0) NOT VALID",
			satisfy: []string{
				"ALTER TABLE users ADD CONSTRAINT users_name_len CHECK (length(name) > 0) NOT VALID",
			},
		},
		{
			name: "validate constraint",
			sql:  "ALTER TABLE users VALIDATE CONSTRAINT users_name_len",
			// Adding it is not enough: a constraint added NOT VALID exists and
			// is not validated, which is exactly the distinction this predicate
			// has to make.
			prepare: []string{
				"ALTER TABLE users ADD CONSTRAINT users_name_len CHECK (length(name) > 0) NOT VALID",
			},
			satisfy: []string{"ALTER TABLE users VALIDATE CONSTRAINT users_name_len"},
		},
		{
			name:    "create table",
			sql:     "CREATE TABLE audit (id bigint PRIMARY KEY)",
			satisfy: []string{"CREATE TABLE audit (id bigint PRIMARY KEY)"},
		},
		{
			name:    "add enum value",
			sql:     "ALTER TYPE mood ADD VALUE 'ok'",
			prepare: []string{"CREATE TYPE mood AS ENUM ('bad')"},
			satisfy: []string{"ALTER TYPE mood ADD VALUE 'ok'"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newDatabase(t)
			check := infer(t, tc.sql)

			for _, stmt := range tc.prepare {
				exec(t, db, stmt)
			}

			if satisfied(t, check, db) {
				t.Fatal("satisfied before the work was done")
			}

			for _, stmt := range tc.satisfy {
				exec(t, db, stmt)
			}

			if !satisfied(t, check, db) {
				t.Fatal("not satisfied once the work was done")
			}
		})
	}
}

// TestDropColumnNeedsItsTable covers a predicate that would otherwise be
// satisfied for the wrong reason.
//
// A column of a table that does not exist is absent, so a bare absence check
// would report the drop as done and hide a migration series in which the table
// was never created.
func TestDropColumnNeedsItsTable(t *testing.T) {
	db := newDatabase(t)

	missing := infer(t, "ALTER TABLE no_such_table DROP COLUMN legacy_email")

	if satisfied(t, missing, db) {
		t.Fatal("a drop against a table that does not exist was reported as done")
	}

	present := infer(t, "ALTER TABLE users DROP COLUMN legacy_email")

	if satisfied(t, present, db) {
		t.Fatal("satisfied while the column was still there")
	}

	exec(t, db, "ALTER TABLE users DROP COLUMN legacy_email")

	if !satisfied(t, present, db) {
		t.Fatal("not satisfied once the column was dropped")
	}
}

// TestInferDescribesWhatItChecks covers the words `mig plan` prints. They come
// from the same switch as the predicate, so a description that drifts from what
// is evaluated is a description that cannot be trusted.
func TestInferDescribesWhatItChecks(t *testing.T) {
	cases := map[string]string{
		"CREATE INDEX CONCURRENTLY idx ON users (email)": "index idx exists and is valid and ready",
		"DROP INDEX idx": "index idx is absent",
		"ALTER TABLE users ADD COLUMN email text":                   "column users.email exists",
		"ALTER TABLE users DROP COLUMN email":                       "table users exists and column users.email does not",
		"ALTER TABLE users ADD CONSTRAINT k CHECK (true) NOT VALID": "constraint users.k exists",
		"ALTER TABLE users VALIDATE CONSTRAINT k":                   "constraint users.k exists and is validated",
		"CREATE TABLE audit (id bigint)":                            "relation audit exists",
		"ALTER TYPE mood ADD VALUE 'ok'":                            `enum mood has label "ok"`,
	}

	for sql, want := range cases {
		t.Run(want, func(t *testing.T) {
			if got := inferCheck(t, sql).Describe; got != want {
				t.Fatalf("described as %q, want %q", got, want)
			}
		})
	}
}

// TestInferJoinsDescriptions covers a step holding several statements, which is
// satisfied only when all of them are.
func TestInferJoinsDescriptions(t *testing.T) {
	const sql = `
ALTER TABLE users ADD COLUMN email text;
CREATE INDEX idx_users_email ON users (email);
`

	const want = "column users.email exists, and index idx_users_email exists and is valid and ready"

	if got := inferCheck(t, sql).Describe; got != want {
		t.Fatalf("described as %q, want %q", got, want)
	}
}

// TestSQLPredicateDescribesItself covers the escape hatch showing what it will
// run, since nothing else can say what it means.
func TestSQLPredicateDescribesItself(t *testing.T) {
	check := predicate.SQL("SELECT true")

	if !strings.Contains(check.Describe, "SELECT true") {
		t.Fatalf("described as %q", check.Describe)
	}
}
