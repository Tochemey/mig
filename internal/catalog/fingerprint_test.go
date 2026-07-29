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
	"database/sql"
	"strings"
	"testing"

	"github.com/tochemey/mig/internal/catalog"
	"github.com/tochemey/mig/test/harness"
)

// The fingerprint is what decides whether a database that was killed part-way
// through a migration ended up where an uninterrupted run would have left it.
// It has to be stable across reads, identical between identical databases, and
// different the moment anything about the schema is.

// TestFingerprintIsStableAndShared covers the two halves of that claim.
func TestFingerprintIsStableAndShared(t *testing.T) {
	first, second := newDatabase(t), newDatabase(t)

	before := fingerprint(t, first)

	if again := fingerprint(t, first); again != before {
		t.Fatal("fingerprint is not stable across reads of one database")
	}

	if other := fingerprint(t, second); other != before {
		t.Fatal("two identical databases have different fingerprints")
	}
}

// TestFingerprintDetectsSchemaChanges covers each section of the fingerprint.
// A section that never changes the hash is a section that is not being read.
func TestFingerprintDetectsSchemaChanges(t *testing.T) {
	cases := map[string]string{
		"added column":     "ALTER TABLE users ADD COLUMN email text",
		"changed default":  "ALTER TABLE users ALTER COLUMN name SET DEFAULT 'anonymous'",
		"dropped not null": "ALTER TABLE users ALTER COLUMN name DROP NOT NULL",
		"added index":      "CREATE INDEX idx_users_name ON users (name)",
		"added constraint": "ALTER TABLE users ADD CONSTRAINT users_name_len CHECK (length(name) > 0)",
		"added sequence":   "CREATE SEQUENCE counter",
		"added table":      "CREATE TABLE audit (id bigint PRIMARY KEY)",
		"granted select":   "GRANT SELECT ON users TO PUBLIC",
	}

	for name, stmt := range cases {
		t.Run(name, func(t *testing.T) {
			db := newDatabase(t)

			was := fingerprint(t, db)
			exec(t, db, stmt)

			if now := fingerprint(t, db); now == was {
				t.Fatalf("%q did not change the fingerprint", stmt)
			}
		})
	}
}

// TestFingerprintDistinguishesAnInterruptedBuild is the case the whole oracle
// exists for. An index left invalid by a killed build has the same name and
// definition as a finished one, so a fingerprint that ignored indisvalid would
// call a broken database converged.
func TestFingerprintDistinguishesAnInterruptedBuild(t *testing.T) {
	partial, complete := newDatabase(t), newDatabase(t)

	// A unique index over duplicated values leaves exactly what a killed
	// CREATE INDEX CONCURRENTLY leaves: the index exists and is not valid.
	exec(t, partial, "INSERT INTO users (id, name, legacy_email) VALUES (1, 'a', 'x'), (2, 'b', 'x')")

	_, err := partial.ExecContext(t.Context(),
		"CREATE UNIQUE INDEX CONCURRENTLY idx_users_legacy_email ON users (legacy_email)")
	if err == nil {
		t.Fatal("a unique index over duplicate values was accepted")
	}

	exec(t, complete, "INSERT INTO users (id, name, legacy_email) VALUES (1, 'a', 'x'), (2, 'b', 'y')")
	exec(t, complete, "CREATE UNIQUE INDEX CONCURRENTLY idx_users_legacy_email ON users (legacy_email)")

	if fingerprint(t, partial) == fingerprint(t, complete) {
		t.Fatal("an invalid index fingerprints the same as a finished one")
	}
}

// TestFingerprintIgnoresTheLedger keeps attempt counts and timestamps out of
// the comparison: an interrupted run and a clean one differ there while their
// schemas agree.
func TestFingerprintIgnoresTheLedger(t *testing.T) {
	db := newDatabase(t)

	before := fingerprint(t, db)

	exec(t, db, "CREATE SCHEMA mig")
	exec(t, db, "CREATE TABLE mig.migrations (id text PRIMARY KEY, attempts int)")
	exec(t, db, "CREATE INDEX idx_mig_migrations ON mig.migrations (attempts)")

	if after := fingerprint(t, db); after != before {
		t.Fatal("the ledger's own schema changed the fingerprint")
	}
}

// TestFingerprintIgnoresSystemSchemas keeps the comparison to the user's
// schema. TOAST index names embed the OID of the table they serve, so two
// databases holding an identical schema disagree about them.
func TestFingerprintIgnoresSystemSchemas(t *testing.T) {
	first, second := newDatabase(t), newDatabase(t)

	// A wide text column gives each table its own TOAST relation, with a name
	// derived from an OID that differs between the two databases.
	exec(t, first, "CREATE TABLE wide (id bigint PRIMARY KEY, body text)")
	exec(t, second, "CREATE TABLE wide (id bigint PRIMARY KEY, body text)")

	if toastOf(t, first) == toastOf(t, second) {
		t.Skip("both databases happened to assign the same TOAST name")
	}

	if fingerprint(t, first) != fingerprint(t, second) {
		t.Fatalf("TOAST naming leaked into the fingerprint:\n%s",
			diff(t, first, second))
	}
}

// TestDescribeMatchesTheFingerprint covers the readable form: it must cover
// every section, so that a mismatch can be diagnosed rather than merely
// reported.
func TestDescribeMatchesTheFingerprint(t *testing.T) {
	db := newDatabase(t)

	exec(t, db, "CREATE INDEX idx_users_name ON users (name)")
	exec(t, db, "CREATE SEQUENCE counter")

	described, err := catalog.Describe(t.Context(), db)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}

	sections := []string{
		"[columns]", "[indexes]", "[constraints]",
		"[sequences]", "[ownership]", "[grants]",
	}

	for _, section := range sections {
		if !strings.Contains(described, section) {
			t.Fatalf("description is missing %s:\n%s", section, described)
		}
	}

	for _, detail := range []string{"public.users.legacy_email", "idx_users_name", "valid=t", "counter"} {
		if !strings.Contains(described, detail) {
			t.Fatalf("description does not mention %q:\n%s", detail, described)
		}
	}

	// Two reads of an unchanged database describe it identically, which is what
	// makes a diff of two descriptions meaningful.
	again, err := catalog.Describe(t.Context(), db)
	if err != nil {
		t.Fatalf("describe again: %v", err)
	}

	if again != described {
		t.Fatal("description is not stable across reads")
	}
}

// TestFingerprintReportsQueryFailure covers a database that went away mid-read,
// which must not be reported as a schema that simply has nothing in it.
func TestFingerprintReportsQueryFailure(t *testing.T) {
	db := newDatabase(t)

	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, err := catalog.Fingerprint(t.Context(), db); err == nil {
		t.Fatal("fingerprint of a closed database returned no error")
	}

	if _, err := catalog.Describe(t.Context(), db); err == nil {
		t.Fatal("describe of a closed database returned no error")
	}
}

// toastOf returns the name of the TOAST relation serving the wide table.
func toastOf(t *testing.T, db *sql.DB) string {
	t.Helper()

	var name string

	const query = `
SELECT t.relname
  FROM pg_class c
  JOIN pg_class t ON t.oid = c.reltoastrelid
 WHERE c.relname = 'wide'`

	if err := db.QueryRowContext(t.Context(), query).Scan(&name); err != nil {
		t.Fatalf("read toast name: %v", err)
	}

	return name
}

// diff renders what two databases disagree about.
func diff(t *testing.T, a, b *sql.DB) string {
	t.Helper()

	first, err := catalog.Describe(t.Context(), a)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}

	second, err := catalog.Describe(t.Context(), b)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}

	return harness.DiffSchema(first, second)
}
