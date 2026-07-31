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

package lockmodel_test

import (
	"reflect"
	"testing"

	pgquery "github.com/pganalyze/pg_query_go/v6"

	"github.com/tochemey/mig/internal/lint/lockmodel"
)

// one shortens the single-effect ALTER TABLE analyses, which all land on t.
func one(mode lockmodel.LockMode, duration lockmodel.DurationClass, reason string) lockmodel.Analysis {
	return lockmodel.Analysis{Effects: []lockmodel.LockEffect{{
		Relation: lockmodel.Relation{Name: "t"},
		Mode:     mode,
		Duration: duration,
		Reason:   reason,
	}}}
}

func TestAlterTableAddColumn(t *testing.T) {
	run(t, []analyzeCase{
		{
			name: "no default is catalog work",
			sql:  "ALTER TABLE t ADD COLUMN c text",
			want: one(lockmodel.AccessExclusive, lockmodel.Instant, "catalog only: add column"),
		},
		{
			name: "a constant default is stored, not backfilled",
			sql:  "ALTER TABLE t ADD COLUMN c int DEFAULT 0",
			want: one(lockmodel.AccessExclusive, lockmodel.Instant,
				"catalog only: non-volatile default stored as a missing value"),
		},
		{
			name:    "the same default rewrote the table before 11",
			sql:     "ALTER TABLE t ADD COLUMN c int DEFAULT 0",
			version: 10,
			want: one(lockmodel.AccessExclusive, lockmodel.Rewrite,
				"table rewrite: add column with default before Postgres 11"),
		},
		{
			name: "a volatile default is evaluated per row",
			sql:  "ALTER TABLE t ADD COLUMN c float DEFAULT random()",
			want: one(lockmodel.AccessExclusive, lockmodel.Rewrite,
				"table rewrite: volatile default evaluated per row"),
		},
		{
			name: "an unknown function counts as volatile",
			sql:  "ALTER TABLE t ADD COLUMN c uuid DEFAULT gen_random_uuid()",
			want: one(lockmodel.AccessExclusive, lockmodel.Rewrite,
				"table rewrite: volatile default evaluated per row"),
		},
		{
			name: "now is stable and stored once",
			sql:  "ALTER TABLE t ADD COLUMN c timestamptz DEFAULT now()",
			want: one(lockmodel.AccessExclusive, lockmodel.Instant,
				"catalog only: non-volatile default stored as a missing value"),
		},
		{
			name: "current_timestamp is stable and stored once",
			sql:  "ALTER TABLE t ADD COLUMN c timestamptz DEFAULT current_timestamp",
			want: one(lockmodel.AccessExclusive, lockmodel.Instant,
				"catalog only: non-volatile default stored as a missing value"),
		},
		{
			name: "a cast of a constant stays constant",
			sql:  "ALTER TABLE t ADD COLUMN c jsonb DEFAULT '{}'::jsonb",
			want: one(lockmodel.AccessExclusive, lockmodel.Instant,
				"catalog only: non-volatile default stored as a missing value"),
		},
		{
			name: "arithmetic over constants stays constant",
			sql:  "ALTER TABLE t ADD COLUMN c int DEFAULT 60 * 60 * 24",
			want: one(lockmodel.AccessExclusive, lockmodel.Instant,
				"catalog only: non-volatile default stored as a missing value"),
		},
		{
			name: "a stable function of a constant stays constant",
			sql:  "ALTER TABLE t ADD COLUMN c text DEFAULT current_setting('server_version')",
			want: one(lockmodel.AccessExclusive, lockmodel.Instant,
				"catalog only: non-volatile default stored as a missing value"),
		},
		{
			name: "a stable function of a volatile argument is volatile",
			sql:  "ALTER TABLE t ADD COLUMN c text DEFAULT current_setting(random()::text)",
			want: one(lockmodel.AccessExclusive, lockmodel.Rewrite,
				"table rewrite: volatile default evaluated per row"),
		},
		{
			name: "an expression the walk does not know counts as volatile",
			sql:  "ALTER TABLE t ADD COLUMN c int DEFAULT CASE WHEN true THEN 1 ELSE 2 END",
			want: one(lockmodel.AccessExclusive, lockmodel.Rewrite,
				"table rewrite: volatile default evaluated per row"),
		},
		{
			name: "serial backfills from a sequence",
			sql:  "ALTER TABLE t ADD COLUMN c bigserial",
			want: one(lockmodel.AccessExclusive, lockmodel.Rewrite,
				"table rewrite: serial column backfills from a sequence"),
		},
		{
			name: "identity backfills from a sequence",
			sql:  "ALTER TABLE t ADD COLUMN c bigint GENERATED ALWAYS AS IDENTITY",
			want: one(lockmodel.AccessExclusive, lockmodel.Rewrite,
				"table rewrite: identity column backfills from a sequence"),
		},
		{
			name: "a stored generated column is computed per row",
			sql:  "ALTER TABLE t ADD COLUMN c int GENERATED ALWAYS AS (1 + 1) STORED",
			want: one(lockmodel.AccessExclusive, lockmodel.Rewrite,
				"table rewrite: stored generated column"),
		},
		{
			name: "an inline unique builds its index under the table lock",
			sql:  "ALTER TABLE t ADD COLUMN c text UNIQUE",
			want: one(lockmodel.AccessExclusive, lockmodel.IndexBuild,
				"index build under the table lock"),
		},
		{
			name: "an inline primary key builds its index under the table lock",
			sql:  "ALTER TABLE t ADD COLUMN c int PRIMARY KEY",
			want: one(lockmodel.AccessExclusive, lockmodel.IndexBuild,
				"index build under the table lock"),
		},
		{
			name: "a rewrite outweighs the index build",
			sql:  "ALTER TABLE t ADD COLUMN c text DEFAULT random()::text UNIQUE",
			want: one(lockmodel.AccessExclusive, lockmodel.Rewrite,
				"table rewrite: volatile default evaluated per row"),
		},
		{
			name: "a rewrite outweighs the index build in either spelling order",
			sql:  "ALTER TABLE t ADD COLUMN c text UNIQUE DEFAULT random()::text",
			want: one(lockmodel.AccessExclusive, lockmodel.Rewrite,
				"table rewrite: volatile default evaluated per row"),
		},
		{
			name: "a harmless default does not mask the index build",
			sql:  "ALTER TABLE t ADD COLUMN c int DEFAULT 5 UNIQUE",
			want: one(lockmodel.AccessExclusive, lockmodel.IndexBuild,
				"index build under the table lock"),
		},
		{
			name: "a harmless default does not mask the index build in either spelling order",
			sql:  "ALTER TABLE t ADD COLUMN c int UNIQUE DEFAULT 5",
			want: one(lockmodel.AccessExclusive, lockmodel.IndexBuild,
				"index build under the table lock"),
		},
		{
			name: "a quoted name is not the builtin it resembles",
			sql:  `ALTER TABLE t ADD COLUMN c timestamptz DEFAULT "NOW"()`,
			want: one(lockmodel.AccessExclusive, lockmodel.Rewrite,
				"table rewrite: volatile default evaluated per row"),
		},
	})
}

// TestAnalyzeAddColumn pins the per-action entry point to the whole-statement
// analysis: one action, same effect either way.
func TestAnalyzeAddColumn(t *testing.T) {
	const sql = "ALTER TABLE t ADD COLUMN c float DEFAULT random()"

	tree, err := pgquery.Parse(sql)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	column := tree.Stmts[0].GetStmt().GetAlterTableStmt().
		GetCmds()[0].GetAlterTableCmd().GetDef().GetColumnDef()

	effect := lockmodel.AnalyzeAddColumn(lockmodel.Relation{Name: "t"}, column, current)

	whole := analyze(t, sql, current)
	if !reflect.DeepEqual([]lockmodel.LockEffect{effect}, whole.Effects) {
		t.Errorf("AnalyzeAddColumn = %+v, Analyze effects = %+v", effect, whole.Effects)
	}
}

func TestAlterTableColumns(t *testing.T) {
	run(t, []analyzeCase{
		{
			name: "drop column",
			sql:  "ALTER TABLE t DROP COLUMN c",
			want: one(lockmodel.AccessExclusive, lockmodel.Instant, "catalog only: drop column"),
		},
		{
			name: "set default",
			sql:  "ALTER TABLE t ALTER COLUMN c SET DEFAULT 0",
			want: one(lockmodel.AccessExclusive, lockmodel.Instant,
				"catalog only: change column default"),
		},
		{
			name: "drop not null",
			sql:  "ALTER TABLE t ALTER COLUMN c DROP NOT NULL",
			want: one(lockmodel.AccessExclusive, lockmodel.Instant, "catalog only: drop not null"),
		},
		{
			name: "set not null scans",
			sql:  "ALTER TABLE t ALTER COLUMN c SET NOT NULL",
			want: one(lockmodel.AccessExclusive, lockmodel.Scan,
				"full scan verifies no null exists"),
		},
		{
			name: "a type change rewrites",
			sql:  "ALTER TABLE t ALTER COLUMN c TYPE bigint",
			want: one(lockmodel.AccessExclusive, lockmodel.Rewrite,
				"table rewrite: column type change"),
		},
		{
			name: "set statistics",
			sql:  "ALTER TABLE t ALTER COLUMN c SET STATISTICS 500",
			want: one(lockmodel.ShareUpdateExclusive, lockmodel.Instant,
				"catalog only: per-column setting"),
		},
		{
			name: "set attribute options",
			sql:  "ALTER TABLE t ALTER COLUMN c SET (n_distinct = 4)",
			want: one(lockmodel.ShareUpdateExclusive, lockmodel.Instant,
				"catalog only: per-column setting"),
		},
		{
			name: "reset attribute options",
			sql:  "ALTER TABLE t ALTER COLUMN c RESET (n_distinct)",
			want: one(lockmodel.ShareUpdateExclusive, lockmodel.Instant,
				"catalog only: per-column setting"),
		},
		{
			name: "set storage parameters",
			sql:  "ALTER TABLE t SET (fillfactor = 70)",
			want: one(lockmodel.ShareUpdateExclusive, lockmodel.Instant,
				"catalog only: storage parameter"),
		},
		{
			name: "reset storage parameters",
			sql:  "ALTER TABLE t RESET (fillfactor)",
			want: one(lockmodel.ShareUpdateExclusive, lockmodel.Instant,
				"catalog only: storage parameter"),
		},
		{
			name: "an unmodelled action errs strong and short",
			sql:  "ALTER TABLE t OWNER TO someone",
			want: one(lockmodel.AccessExclusive, lockmodel.Instant,
				"alter table action, assumed catalog only"),
		},
		{
			name: "each action contributes its own effect",
			sql:  "ALTER TABLE t ADD COLUMN a int, ALTER COLUMN b SET NOT NULL",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{
				{Relation: lockmodel.Relation{Name: "t"}, Mode: lockmodel.AccessExclusive,
					Duration: lockmodel.Instant, Reason: "catalog only: add column"},
				{Relation: lockmodel.Relation{Name: "t"}, Mode: lockmodel.AccessExclusive,
					Duration: lockmodel.Scan, Reason: "full scan verifies no null exists"},
			}},
		},
	})
}

func TestAlterTableConstraints(t *testing.T) {
	run(t, []analyzeCase{
		{
			name: "a foreign key locks both tables and scans",
			sql:  "ALTER TABLE t ADD CONSTRAINT fk FOREIGN KEY (r) REFERENCES app.parents (id)",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{
				{Relation: lockmodel.Relation{Name: "t"}, Mode: lockmodel.ShareRowExclusive,
					Duration: lockmodel.Scan,
					Reason:   "foreign key locks both tables; validation scans the table"},
				{Relation: lockmodel.Relation{Schema: "app", Name: "parents"},
					Mode: lockmodel.ShareRowExclusive, Duration: lockmodel.Scan, Implicit: true,
					Reason: "foreign key locks the referenced table"},
			}},
		},
		{
			name: "not valid defers the foreign key scan",
			sql:  "ALTER TABLE t ADD CONSTRAINT fk FOREIGN KEY (r) REFERENCES parents (id) NOT VALID",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{
				{Relation: lockmodel.Relation{Name: "t"}, Mode: lockmodel.ShareRowExclusive,
					Duration: lockmodel.Instant,
					Reason:   "foreign key locks both tables; not valid: validation deferred"},
				{Relation: lockmodel.Relation{Name: "parents"},
					Mode: lockmodel.ShareRowExclusive, Duration: lockmodel.Instant, Implicit: true,
					Reason: "foreign key locks the referenced table"},
			}},
		},
		{
			name: "a check constraint scans under the table lock",
			sql:  "ALTER TABLE t ADD CONSTRAINT ck CHECK (c > 0)",
			want: one(lockmodel.AccessExclusive, lockmodel.Scan, "validation scans the table"),
		},
		{
			name: "not valid defers the check scan",
			sql:  "ALTER TABLE t ADD CONSTRAINT ck CHECK (c > 0) NOT VALID",
			want: one(lockmodel.AccessExclusive, lockmodel.Instant,
				"not valid: validation deferred"),
		},
		{
			name: "a primary key builds its index under the table lock",
			sql:  "ALTER TABLE t ADD CONSTRAINT pk PRIMARY KEY (id)",
			want: one(lockmodel.AccessExclusive, lockmodel.IndexBuild,
				"index build under the table lock"),
		},
		{
			name: "a unique constraint builds its index under the table lock",
			sql:  "ALTER TABLE t ADD CONSTRAINT uq UNIQUE (c)",
			want: one(lockmodel.AccessExclusive, lockmodel.IndexBuild,
				"index build under the table lock"),
		},
		{
			name: "using index adopts the finished build",
			sql:  "ALTER TABLE t ADD CONSTRAINT pk PRIMARY KEY USING INDEX idx",
			want: one(lockmodel.AccessExclusive, lockmodel.Instant, "adopts an existing index"),
		},
		{
			name: "an exclusion constraint builds its index under the table lock",
			sql:  "ALTER TABLE t ADD CONSTRAINT ex EXCLUDE USING gist (c WITH =)",
			want: one(lockmodel.AccessExclusive, lockmodel.IndexBuild,
				"index build under the table lock"),
		},
		{
			name: "validate constraint scans without blocking traffic",
			sql:  "ALTER TABLE t VALIDATE CONSTRAINT ck",
			want: one(lockmodel.ShareUpdateExclusive, lockmodel.Scan,
				"validation scans without blocking traffic"),
		},
		{
			name: "drop constraint",
			sql:  "ALTER TABLE t DROP CONSTRAINT ck",
			want: one(lockmodel.AccessExclusive, lockmodel.Instant,
				"catalog only: drop constraint"),
		},
	})
}

func TestAlterTablePartitions(t *testing.T) {
	parent := lockmodel.Relation{Name: "t"}
	child := lockmodel.Relation{Name: "p1"}

	run(t, []analyzeCase{
		{
			name: "attach validates the incoming partition",
			sql:  "ALTER TABLE t ATTACH PARTITION p1 FOR VALUES FROM (1) TO (10)",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{
				{Relation: parent, Mode: lockmodel.ShareUpdateExclusive, Duration: lockmodel.Instant,
					Reason: "catalog only: attach partition"},
				{Relation: child, Mode: lockmodel.AccessExclusive, Duration: lockmodel.Scan,
					Reason: "scan verifies the partition bound"},
			}},
		},
		{
			name:    "attach locked the parent exclusively before 12",
			sql:     "ALTER TABLE t ATTACH PARTITION p1 FOR VALUES FROM (1) TO (10)",
			version: 11,
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{
				{Relation: parent, Mode: lockmodel.AccessExclusive, Duration: lockmodel.Instant,
					Reason: "catalog only: attach partition"},
				{Relation: child, Mode: lockmodel.AccessExclusive, Duration: lockmodel.Scan,
					Reason: "scan verifies the partition bound"},
			}},
		},
		{
			name: "detach locks parent and partition",
			sql:  "ALTER TABLE t DETACH PARTITION p1",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{
				{Relation: parent, Mode: lockmodel.AccessExclusive, Duration: lockmodel.Instant,
					Reason: "catalog only: detach partition"},
				{Relation: child, Mode: lockmodel.AccessExclusive, Duration: lockmodel.Instant,
					Reason: "catalog only: detach partition"},
			}},
		},
		{
			name: "detach concurrently waits instead of blocking",
			sql:  "ALTER TABLE t DETACH PARTITION p1 CONCURRENTLY",
			want: lockmodel.Analysis{NoTx: true, Effects: []lockmodel.LockEffect{
				{Relation: parent, Mode: lockmodel.ShareUpdateExclusive, Duration: lockmodel.Instant,
					Reason: "concurrent detach waits out open transactions"},
				{Relation: child, Mode: lockmodel.ShareUpdateExclusive, Duration: lockmodel.Instant,
					Reason: "concurrent detach waits out open transactions"},
			}},
		},
	})
}
