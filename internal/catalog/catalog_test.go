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

package catalog_test

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/tochemey/mig/internal/catalog"
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

// TestLookupIndexReportsAbsence covers the state before a step has run. An
// index that is not there is not an error.
func TestLookupIndexReportsAbsence(t *testing.T) {
	db := newDatabase(t)

	index, err := catalog.LookupIndex(t.Context(), db, "", "no_such_index")
	if err != nil {
		t.Fatalf("look up: %v", err)
	}

	if index.Exists || index.Usable() {
		t.Fatalf("absent index reported as %+v", index)
	}
}

// TestLookupIndexReportsAFinishedBuild covers the state a satisfied step sees.
func TestLookupIndexReportsAFinishedBuild(t *testing.T) {
	db := newDatabase(t)

	exec(t, db, "CREATE INDEX idx_users_name ON users (name)")

	index, err := catalog.LookupIndex(t.Context(), db, "", "idx_users_name")
	if err != nil {
		t.Fatalf("look up: %v", err)
	}

	if !index.Usable() {
		t.Fatalf("finished index reported as %+v", index)
	}
}

// TestLookupIndexReportsAnInterruptedBuild is the distinction the whole
// reconciliation rests on: an index left by an interrupted concurrent build
// exists but is not valid, and treating it as finished would ship a migration
// the planner ignores.
func TestLookupIndexReportsAnInterruptedBuild(t *testing.T) {
	db := newDatabase(t)

	// A unique index over duplicated values fails part-way and leaves exactly
	// the state a killed CREATE INDEX CONCURRENTLY does.
	exec(t, db, "INSERT INTO users (id, name, legacy_email) VALUES (1, 'a', 'x'), (2, 'b', 'x')")

	_, err := db.ExecContext(t.Context(),
		"CREATE UNIQUE INDEX CONCURRENTLY idx_users_legacy_email ON users (legacy_email)")
	if err == nil {
		t.Fatal("a unique index over duplicate values was accepted")
	}

	index, err := catalog.LookupIndex(t.Context(), db, "", "idx_users_legacy_email")
	if err != nil {
		t.Fatalf("look up: %v", err)
	}

	if !index.Exists {
		t.Fatal("the failed build left no index")
	}

	if index.Usable() {
		t.Fatalf("the failed build's index is reported usable: %+v", index)
	}
}

// TestLookupIndexResolvesSchemaAndQuoting covers names that need qualifying or
// quoting, which a string-matching lookup would resolve to the wrong object.
func TestLookupIndexResolvesSchemaAndQuoting(t *testing.T) {
	db := newDatabase(t)

	exec(t, db, `CREATE SCHEMA app`)
	exec(t, db, `CREATE TABLE app.users (id bigint)`)
	exec(t, db, `CREATE INDEX "Mixed Case" ON app.users (id)`)

	qualified, err := catalog.LookupIndex(t.Context(), db, "app", "Mixed Case")
	if err != nil {
		t.Fatalf("look up qualified: %v", err)
	}

	if !qualified.Usable() {
		t.Fatalf("qualified index reported as %+v", qualified)
	}

	// The same name in the default schema is a different object.
	unqualified, err := catalog.LookupIndex(t.Context(), db, "", "Mixed Case")
	if err != nil {
		t.Fatalf("look up unqualified: %v", err)
	}

	if unqualified.Exists {
		t.Fatal("an index in another schema was found without qualification")
	}
}

// TestLookupIndexReportsQueryFailure covers a broken connection, which must not
// be reported as an absent index and so as work still to do.
func TestLookupIndexReportsQueryFailure(t *testing.T) {
	db := newDatabase(t)

	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, err := catalog.LookupIndex(t.Context(), db, "", "anything"); err == nil {
		t.Fatal("look up on a closed database returned no error")
	}
}

// TestQualifiedIdent covers the quoting that keeps a name needing quotes from
// resolving to a different object.
func TestQualifiedIdent(t *testing.T) {
	cases := map[string]struct{ schema, name, want string }{
		"bare":            {name: "idx", want: `"idx"`},
		"qualified":       {schema: "app", name: "idx", want: `"app"."idx"`},
		"needs quoting":   {name: "Mixed Case", want: `"Mixed Case"`},
		"embedded quotes": {name: `we"ird`, want: `"we""ird"`},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := catalog.QualifiedIdent(tc.schema, tc.name); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
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

// fingerprint hashes a database or fails the test.
func fingerprint(t *testing.T, db *sql.DB) string {
	t.Helper()

	sum, err := catalog.Fingerprint(t.Context(), db)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}

	return sum
}
