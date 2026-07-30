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

package parse_test

import (
	"bytes"
	"testing"

	"github.com/tochemey/mig/internal/parse"
)

// TestParseClassifiesIndexStatements covers the statements the executor can
// reconcile from the catalog, including the shapes a regular expression would
// misread: quoted identifiers, embedded comments and multi-line text.
func TestParseClassifiesIndexStatements(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want parse.Statement
	}{
		{
			name: "create index",
			sql:  "CREATE INDEX idx_users_email ON users (email)",
			want: parse.Statement{
				Kind:   parse.KindCreateIndex,
				Target: parse.Target{Name: "idx_users_email", On: "users"},
			},
		},
		{
			name: "create index concurrently",
			sql:  "CREATE INDEX CONCURRENTLY idx_users_email ON users (email)",
			want: parse.Statement{
				Kind:   parse.KindCreateIndex,
				Target: parse.Target{Name: "idx_users_email", On: "users", Concurrent: true},
			},
		},
		{
			name: "schema qualified table",
			sql:  "CREATE INDEX CONCURRENTLY idx ON app.users (email)",
			want: parse.Statement{
				Kind:   parse.KindCreateIndex,
				Target: parse.Target{Schema: "app", Name: "idx", On: "users", Concurrent: true},
			},
		},
		{
			name: "quoted identifiers",
			sql:  `CREATE INDEX "Mixed Case" ON "Users" ("Email")`,
			want: parse.Statement{
				Kind:   parse.KindCreateIndex,
				Target: parse.Target{Name: "Mixed Case", On: "Users"},
			},
		},
		{
			name: "partial index with a where clause",
			sql:  "CREATE INDEX CONCURRENTLY idx ON users (email) WHERE email IS NOT NULL",
			want: parse.Statement{
				Kind:   parse.KindCreateIndex,
				Target: parse.Target{Name: "idx", On: "users", Concurrent: true},
			},
		},
		{
			name: "multi-line with a trailing comment",
			sql:  "CREATE INDEX CONCURRENTLY idx\n  ON users (email) -- the point of the migration",
			want: parse.Statement{
				Kind:   parse.KindCreateIndex,
				Target: parse.Target{Name: "idx", On: "users", Concurrent: true},
			},
		},
		{
			name: "drop index",
			sql:  "DROP INDEX idx_users_email",
			want: parse.Statement{
				Kind:   parse.KindDropIndex,
				Target: parse.Target{Name: "idx_users_email"},
			},
		},
		{
			name: "drop index concurrently if exists",
			sql:  "DROP INDEX CONCURRENTLY IF EXISTS app.idx_users_email",
			want: parse.Statement{
				Kind:   parse.KindDropIndex,
				Target: parse.Target{Schema: "app", Name: "idx_users_email", Concurrent: true},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			statements, err := parse.Parse(tc.sql)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}

			if len(statements) != 1 {
				t.Fatalf("got %d statements, want 1", len(statements))
			}

			got := statements[0]

			if got.Kind != tc.want.Kind {
				t.Fatalf("kind is %q, want %q", got.Kind, tc.want.Kind)
			}

			if got.Target != tc.want.Target {
				t.Fatalf("target is %+v, want %+v", got.Target, tc.want.Target)
			}
		})
	}
}

// TestParseClassifiesTheRestOfTheTable covers every remaining statement the
// executor can reconcile from the catalog.
func TestParseClassifiesTheRestOfTheTable(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want parse.Statement
	}{
		{
			name: "add column",
			sql:  "ALTER TABLE users ADD COLUMN email text",
			want: parse.Statement{
				Kind:   parse.KindAddColumn,
				Target: parse.Target{Name: "users", Member: "email"},
			},
		},
		{
			name: "add column if not exists, schema qualified",
			sql:  "ALTER TABLE app.users ADD COLUMN IF NOT EXISTS email text",
			want: parse.Statement{
				Kind:   parse.KindAddColumn,
				Target: parse.Target{Schema: "app", Name: "users", Member: "email"},
			},
		},
		{
			name: "drop column",
			sql:  "ALTER TABLE users DROP COLUMN legacy_email",
			want: parse.Statement{
				Kind:   parse.KindDropColumn,
				Target: parse.Target{Name: "users", Member: "legacy_email"},
			},
		},
		{
			name: "add constraint not valid",
			sql:  "ALTER TABLE users ADD CONSTRAINT users_email_nn CHECK (email IS NOT NULL) NOT VALID",
			want: parse.Statement{
				Kind:   parse.KindAddConstraint,
				Target: parse.Target{Name: "users", Member: "users_email_nn"},
			},
		},
		{
			name: "validate constraint",
			sql:  "ALTER TABLE users VALIDATE CONSTRAINT users_email_nn",
			want: parse.Statement{
				Kind:   parse.KindValidateConstraint,
				Target: parse.Target{Name: "users", Member: "users_email_nn"},
			},
		},
		{
			name: "create table",
			sql:  "CREATE TABLE audit (id bigint PRIMARY KEY)",
			want: parse.Statement{
				Kind:   parse.KindCreateTable,
				Target: parse.Target{Name: "audit"},
			},
		},
		{
			name: "create table if not exists, schema qualified",
			sql:  "CREATE TABLE IF NOT EXISTS app.audit (id bigint)",
			want: parse.Statement{
				Kind:   parse.KindCreateTable,
				Target: parse.Target{Schema: "app", Name: "audit"},
			},
		},
		{
			name: "add enum value",
			sql:  "ALTER TYPE mood ADD VALUE 'ok'",
			want: parse.Statement{
				Kind:   parse.KindAddEnumValue,
				Target: parse.Target{Name: "mood", Member: "ok"},
			},
		},
		{
			name: "add enum value if not exists, schema qualified",
			sql:  "ALTER TYPE app.mood ADD VALUE IF NOT EXISTS 'great' BEFORE 'ok'",
			want: parse.Statement{
				Kind:   parse.KindAddEnumValue,
				Target: parse.Target{Schema: "app", Name: "mood", Member: "great"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			statements, err := parse.Parse(tc.sql)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}

			got := statements[0]

			if got.Kind != tc.want.Kind {
				t.Fatalf("kind is %q, want %q", got.Kind, tc.want.Kind)
			}

			if got.Target != tc.want.Target {
				t.Fatalf("target is %+v, want %+v", got.Target, tc.want.Target)
			}
		})
	}
}

// TestParseLeavesOtherStatementsUnclassified covers the statements with no
// inferred predicate. A non-transactional step among them cannot converge, so
// the plan refuses it rather than guessing.
func TestParseLeavesOtherStatementsUnclassified(t *testing.T) {
	cases := []string{
		"UPDATE users SET email = legacy_email WHERE id > 0",
		"VACUUM ANALYZE users",
		// Several indexes at once: the caller needs one to reason about.
		"DROP INDEX idx_a, idx_b",
		// A DROP that is not a drop of an index.
		"DROP TABLE users",
		// Several actions at once: they have no single predicate between them.
		"ALTER TABLE users ADD COLUMN a text, ADD COLUMN b text",
		// An action outside the table: nothing to look for afterwards.
		"ALTER TABLE users ALTER COLUMN name SET DEFAULT 'x'",
		// A constraint the server will name, so there is no name to check.
		"ALTER TABLE users ADD CHECK (name <> '')",
		// Renaming a label rather than adding one.
		"ALTER TYPE mood RENAME VALUE 'ok' TO 'fine'",
	}

	for _, sql := range cases {
		t.Run(sql, func(t *testing.T) {
			statements, err := parse.Parse(sql)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}

			if got := statements[0].Kind; got != parse.KindOther {
				t.Fatalf("kind is %q, want %q", got, parse.KindOther)
			}
		})
	}
}

// TestParseSplitsMultipleStatements covers a step that holds more than one
// statement.
func TestParseSplitsMultipleStatements(t *testing.T) {
	const sql = `
CREATE INDEX CONCURRENTLY idx_a ON users (email);
-- a comment between statements
DROP INDEX CONCURRENTLY IF EXISTS idx_b;
`

	statements, err := parse.Parse(sql)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if len(statements) != 2 {
		t.Fatalf("got %d statements, want 2", len(statements))
	}

	if statements[0].Kind != parse.KindCreateIndex || statements[1].Kind != parse.KindDropIndex {
		t.Fatalf("kinds are %q and %q", statements[0].Kind, statements[1].Kind)
	}
}

// TestParseRejectsInvalidSQL keeps a typo from reaching the database as a
// step that fails half way through a migration.
func TestParseRejectsInvalidSQL(t *testing.T) {
	if _, err := parse.Parse("CREATE INDEX ON"); err == nil {
		t.Fatal("parse accepted invalid SQL")
	}
}

// TestSplitIgnoresEmptyInput covers a step whose body is only comments.
func TestSplitIgnoresEmptyInput(t *testing.T) {
	statements, err := parse.Split("-- nothing but a comment\n")
	if err != nil {
		t.Fatalf("split: %v", err)
	}

	if len(statements) != 0 {
		t.Fatalf("got %d statements, want none: %q", len(statements), statements)
	}
}

// TestChecksumIgnoresFormatting is what keeps drift detection usable:
// reformatting a migration or editing its comments must not read as a change.
func TestChecksumIgnoresFormatting(t *testing.T) {
	const original = "CREATE INDEX CONCURRENTLY idx_users_email ON users (email)"
	const reformatted = "-- rebuilt for the new query\nCREATE  INDEX  CONCURRENTLY\n  idx_users_email\n  ON users (email)"

	first := checksum(t, original)

	if second := checksum(t, reformatted); !bytes.Equal(first, second) {
		t.Fatal("reformatting changed the checksum")
	}
}

// TestChecksumDistinguishesStatements keeps the checksum from being so
// forgiving that a genuine edit slips past.
func TestChecksumDistinguishesStatements(t *testing.T) {
	cases := map[string]string{
		"different index name": "CREATE INDEX CONCURRENTLY idx_other ON users (email)",
		"different table":      "CREATE INDEX CONCURRENTLY idx_users_email ON accounts (email)",
		"different column":     "CREATE INDEX CONCURRENTLY idx_users_email ON users (name)",
		"no longer concurrent": "CREATE INDEX idx_users_email ON users (email)",
		"now unique":           "CREATE UNIQUE INDEX CONCURRENTLY idx_users_email ON users (email)",
	}

	original := checksum(t, "CREATE INDEX CONCURRENTLY idx_users_email ON users (email)")

	for name, sql := range cases {
		t.Run(name, func(t *testing.T) {
			if bytes.Equal(original, checksum(t, sql)) {
				t.Fatalf("%q has the same checksum as the original", sql)
			}
		})
	}
}

// TestChecksumRejectsInvalidSQL covers a checksum of something unparseable.
func TestChecksumRejectsInvalidSQL(t *testing.T) {
	if _, err := parse.Checksum("CREATE INDEX ON"); err == nil {
		t.Fatal("checksum accepted invalid SQL")
	}
}

// TestTargetQualified covers the name used in reconciliation errors.
func TestTargetQualified(t *testing.T) {
	bare := parse.Target{Name: "idx"}
	if got := bare.Qualified(); got != "idx" {
		t.Fatalf("bare index renders as %q", got)
	}

	qualified := parse.Target{Schema: "app", Name: "idx"}
	if got := qualified.Qualified(); got != "app.idx" {
		t.Fatalf("qualified index renders as %q", got)
	}
}

// checksum hashes sql or fails the test.
func checksum(t *testing.T, sql string) []byte {
	t.Helper()

	sum, err := parse.Checksum(sql)
	if err != nil {
		t.Fatalf("checksum %q: %v", sql, err)
	}

	return sum
}
