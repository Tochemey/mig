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

package lockmatrix_test

import (
	"context"
	"os"
	"slices"
	"testing"

	"github.com/tochemey/mig/internal/lint/lockmodel"
	"github.com/tochemey/mig/test/harness"
	"github.com/tochemey/mig/test/lockmatrix"
)

// seeded is the base fixture: one plain table with enough rows that a rewrite
// writes real pages.
var seeded = []string{
	"CREATE TABLE t (id int, c int)",
	"INSERT INTO t SELECT g, g FROM generate_series(1, 500) g",
}

// withIndex adds the index some cases operate on.
var withIndex = slices.Concat(seeded, []string{
	"CREATE INDEX t_idx ON t (id)"})

// withParent adds a table for foreign keys to point at, populated so that
// validating a key over the seeded rows succeeds.
var withParent = slices.Concat(seeded, []string{
	"CREATE TABLE parent (id int PRIMARY KEY)",
	"INSERT INTO parent SELECT g FROM generate_series(1, 500) g"})

// withPartitions is a partitioned parent and one attached partition.
var withPartitions = []string{
	"CREATE TABLE parted (id int) PARTITION BY RANGE (id)",
	"CREATE TABLE part1 PARTITION OF parted FOR VALUES FROM (0) TO (100)",
}

// matrix is the version matrix: every statement the model predicts, next to
// the schema it runs against and the locks only the catalog could have named.
var matrix = []lockmatrix.Case{
	{
		Name: "create_index",
		Seed: seeded,
		SQL:  "CREATE INDEX t_idx ON t (id)",
	},
	{
		Name:  "drop_index",
		Seed:  withIndex,
		SQL:   "DROP INDEX t_idx",
		Extra: map[string]lockmodel.LockMode{"t": lockmodel.AccessExclusive},
	},
	{
		Name: "rename_index",
		Seed: withIndex,
		SQL:  "ALTER INDEX t_idx RENAME TO t_idx2",
	},
	{
		Name: "reindex_index",
		Seed: withIndex,
		SQL:  "REINDEX INDEX t_idx",
		Extra: map[string]lockmodel.LockMode{
			"t": lockmodel.Share,
		},
		ExtraRewrites: []string{"t_idx"},
	},
	{
		Name: "add_column",
		Seed: seeded,
		SQL:  "ALTER TABLE t ADD COLUMN d text",
	},
	{
		Name: "add_column_constant_default",
		Seed: seeded,
		SQL:  "ALTER TABLE t ADD COLUMN d int DEFAULT 42",
	},
	{
		Name:   "add_column_volatile_default",
		Seed:   seeded,
		SQL:    "ALTER TABLE t ADD COLUMN d float8 DEFAULT random()",
		Visits: []string{"t"},
	},
	{
		Name:   "add_column_identity",
		Seed:   seeded,
		SQL:    "ALTER TABLE t ADD COLUMN d bigint GENERATED ALWAYS AS IDENTITY",
		Visits: []string{"t"},
	},
	{
		Name: "add_column_unique",
		Seed: seeded,
		SQL:  "ALTER TABLE t ADD COLUMN d int UNIQUE",
	},
	{
		Name: "drop_column",
		Seed: seeded,
		SQL:  "ALTER TABLE t DROP COLUMN c",
	},
	{
		Name: "set_default",
		Seed: seeded,
		SQL:  "ALTER TABLE t ALTER COLUMN c SET DEFAULT 0",
	},
	{
		Name:   "set_not_null",
		Seed:   seeded,
		SQL:    "ALTER TABLE t ALTER COLUMN id SET NOT NULL",
		Visits: []string{"t"},
	},
	{
		Name:   "alter_column_type",
		Seed:   seeded,
		SQL:    "ALTER TABLE t ALTER COLUMN id TYPE bigint",
		Visits: []string{"t"},
	},
	{
		Name: "set_statistics",
		Seed: seeded,
		SQL:  "ALTER TABLE t ALTER COLUMN id SET STATISTICS 200",
	},
	{
		Name: "set_storage_parameter",
		Seed: seeded,
		SQL:  "ALTER TABLE t SET (fillfactor = 70)",
	},
	{
		Name:   "add_check",
		Seed:   seeded,
		SQL:    "ALTER TABLE t ADD CONSTRAINT ck CHECK (id > 0)",
		Visits: []string{"t"},
	},
	{
		Name: "add_check_not_valid",
		Seed: seeded,
		SQL:  "ALTER TABLE t ADD CONSTRAINT ck CHECK (id > 0) NOT VALID",
	},
	{
		Name: "validate_constraint",
		Seed: slices.Concat(seeded, []string{
			"ALTER TABLE t ADD CONSTRAINT ck CHECK (id > 0) NOT VALID"}),
		SQL:    "ALTER TABLE t VALIDATE CONSTRAINT ck",
		Visits: []string{"t"},
	},
	{
		Name: "add_foreign_key",
		Seed: withParent,
		SQL:  "ALTER TABLE t ADD CONSTRAINT fk FOREIGN KEY (c) REFERENCES parent (id)",
		Extra: map[string]lockmodel.LockMode{
			"parent_pkey": lockmodel.AccessShare,
		},
		Visits: []string{"t"},
	},
	{
		Name: "add_foreign_key_not_valid",
		Seed: withParent,
		SQL:  "ALTER TABLE t ADD CONSTRAINT fk FOREIGN KEY (c) REFERENCES parent (id) NOT VALID",
	},
	{
		Name:   "add_primary_key",
		Seed:   seeded,
		SQL:    "ALTER TABLE t ADD CONSTRAINT t_pk PRIMARY KEY (id)",
		Visits: []string{"t"},
	},
	{
		Name: "add_primary_key_using_index",
		Seed: slices.Concat(seeded, []string{
			"CREATE UNIQUE INDEX t_key ON t (id)",
			"ALTER TABLE t ALTER COLUMN id SET NOT NULL"}),
		SQL: "ALTER TABLE t ADD CONSTRAINT t_pk PRIMARY KEY USING INDEX t_key",
		Extra: map[string]lockmodel.LockMode{
			"t_key": lockmodel.ShareUpdateExclusive,
		},
	},
	{
		Name: "drop_constraint",
		Seed: slices.Concat(seeded, []string{
			"ALTER TABLE t ADD CONSTRAINT ck CHECK (id > 0)"}),
		SQL: "ALTER TABLE t DROP CONSTRAINT ck",
	},
	{
		Name: "drop_table",
		Seed: seeded,
		SQL:  "DROP TABLE t",
	},
	{
		Name: "drop_foreign_key_target",
		Seed: slices.Concat(withParent, []string{
			"ALTER TABLE t ADD CONSTRAINT fk FOREIGN KEY (c) REFERENCES parent (id) NOT VALID"}),
		SQL: "DROP TABLE parent CASCADE",
		Extra: map[string]lockmodel.LockMode{
			"t":           lockmodel.AccessExclusive,
			"parent_pkey": lockmodel.AccessExclusive,
		},
	},
	{
		Name:          "truncate",
		Seed:          seeded,
		SQL:           "TRUNCATE t",
		ExtraRewrites: []string{"t"},
	},
	{
		Name: "rename_table",
		Seed: seeded,
		SQL:  "ALTER TABLE t RENAME TO t2",
	},
	{
		Name: "rename_column",
		Seed: seeded,
		SQL:  "ALTER TABLE t RENAME COLUMN c TO c2",
	},
	{
		Name: "cluster",
		Seed: withIndex,
		SQL:  "CLUSTER t USING t_idx",
	},
	{
		Name: "refresh_materialized_view",
		Seed: slices.Concat(seeded, []string{
			"CREATE MATERIALIZED VIEW mv AS SELECT id FROM t"}),
		SQL: "REFRESH MATERIALIZED VIEW mv",
		Extra: map[string]lockmodel.LockMode{
			"t": lockmodel.AccessShare,
		},
	},
	{
		Name: "lock_table",
		Seed: seeded,
		SQL:  "LOCK TABLE t IN SHARE ROW EXCLUSIVE MODE",
	},
	{
		Name: "select",
		Seed: seeded,
		SQL:  "SELECT count(*) FROM t",
	},
	{
		Name: "select_for_update",
		Seed: seeded,
		SQL:  "SELECT * FROM t WHERE id = 1 FOR UPDATE",
	},
	{
		Name: "insert",
		Seed: seeded,
		SQL:  "INSERT INTO t VALUES (1000, 1000)",
	},
	{
		Name: "update",
		Seed: seeded,
		SQL:  "UPDATE t SET c = c + 1 WHERE id = 1",
	},
	{
		Name: "delete",
		Seed: seeded,
		SQL:  "DELETE FROM t WHERE id = 1",
	},
	{
		Name: "analyze",
		Seed: seeded,
		SQL:  "ANALYZE t",
	},
	{
		Name: "create_table_with_foreign_key",
		Seed: withParent,
		SQL:  "CREATE TABLE child (id int REFERENCES parent (id))",
	},
	{
		Name: "create_partition",
		Seed: withPartitions,
		SQL:  "CREATE TABLE part2 PARTITION OF parted FOR VALUES FROM (100) TO (200)",
	},
	{
		Name: "attach_partition",
		Seed: slices.Concat(withPartitions, []string{
			"CREATE TABLE part2 (id int)"}),
		SQL:    "ALTER TABLE parted ATTACH PARTITION part2 FOR VALUES FROM (100) TO (200)",
		Visits: []string{"part2"},
	},
	{
		Name: "detach_partition",
		Seed: withPartitions,
		SQL:  "ALTER TABLE parted DETACH PARTITION part1",
	},
	{
		Name: "add_enum_value",
		Seed: []string{"CREATE TYPE mood AS ENUM ('flat')"},
		SQL:  "ALTER TYPE mood ADD VALUE 'curious'",
	},

	// The statements below refuse transaction blocks. Each is observed while
	// queued behind a conflicting guard lock.
	{
		Name:    "create_index_concurrently",
		Seed:    seeded,
		SQL:     "CREATE INDEX CONCURRENTLY t_idx ON t (id)",
		Blocked: true,
		Guard:   "LOCK TABLE t IN SHARE UPDATE EXCLUSIVE MODE",
	},
	{
		Name:    "drop_index_concurrently",
		Seed:    withIndex,
		SQL:     "DROP INDEX CONCURRENTLY t_idx",
		Blocked: true,
		Guard:   "LOCK TABLE t IN SHARE UPDATE EXCLUSIVE MODE",
		Extra:   map[string]lockmodel.LockMode{"t": lockmodel.ShareUpdateExclusive},
	},
	{
		Name:    "reindex_index_concurrently",
		Seed:    withIndex,
		SQL:     "REINDEX (CONCURRENTLY) INDEX t_idx",
		Blocked: true,
		Guard:   "LOCK TABLE t IN SHARE UPDATE EXCLUSIVE MODE",
		Extra:   map[string]lockmodel.LockMode{"t": lockmodel.ShareUpdateExclusive},
	},
	{
		Name:    "vacuum_full",
		Seed:    seeded,
		SQL:     "VACUUM FULL t",
		Blocked: true,
		Guard:   "LOCK TABLE t IN ACCESS SHARE MODE",
	},
	{
		Name:    "vacuum",
		Seed:    seeded,
		SQL:     "VACUUM t",
		Blocked: true,
		Guard:   "LOCK TABLE t IN SHARE UPDATE EXCLUSIVE MODE",
	},
	{
		Name: "refresh_materialized_view_concurrently",
		Seed: slices.Concat(seeded, []string{
			"CREATE MATERIALIZED VIEW mv AS SELECT id FROM t",
			"CREATE UNIQUE INDEX mv_key ON mv (id)"}),
		SQL: "REFRESH MATERIALIZED VIEW CONCURRENTLY mv",
		Extra: map[string]lockmodel.LockMode{
			"t":      lockmodel.AccessShare,
			"mv_key": lockmodel.RowExclusive,
		},
	},
	{
		Name:    "detach_partition_concurrently",
		Seed:    withPartitions,
		SQL:     "ALTER TABLE parted DETACH PARTITION part1 CONCURRENTLY",
		Blocked: true,
		Guard:   "LOCK TABLE parted IN SHARE UPDATE EXCLUSIVE MODE",
	},
}

// shared is the container for this package, or nil when docker is absent.
var shared *harness.Harness

// major is the server's major version, which is what the model's predictions
// are conditioned on.
var major int

// TestMain brings up one container for the package.
func TestMain(m *testing.M) {
	os.Exit(harness.Main(m, 0, func(_ context.Context, h *harness.Harness) error {
		shared = h
		major = h.Major()

		return nil
	}))
}

// TestProbeFailures covers the probe's own refusals: a broken fixture, a
// statement the server rejects, and a transactional statement posing as a
// blocked one.
func TestProbeFailures(t *testing.T) {
	if shared == nil {
		t.Skip("docker unavailable")
	}

	fails := []lockmatrix.Case{
		{
			Name: "a broken seed statement",
			Seed: []string{"CREATE TABLE"},
			SQL:  "SELECT 1",
		},
		{
			Name: "a failing statement inside the transaction",
			Seed: seeded,
			SQL:  "ALTER TABLE t DROP COLUMN missing",
		},
		{
			Name:    "a broken guard statement",
			Seed:    seeded,
			SQL:     "VACUUM t",
			Blocked: true,
			Guard:   "LOCK TABLE missing IN ACCESS SHARE MODE",
		},
		{
			Name: "a statement that fails once the guard lifts",
			Seed: withIndex,
			SQL:  "CREATE INDEX CONCURRENTLY t_idx ON t (id)",
			// The name collision is only checked after the table lock is
			// granted, so the failure lands on the far side of the wait.
			Blocked: true,
			Guard:   "LOCK TABLE t IN SHARE UPDATE EXCLUSIVE MODE",
		},
	}

	for _, c := range fails {
		t.Run(c.Name, func(t *testing.T) {
			if _, err := lockmatrix.Probe(t.Context(), shared, c); err == nil {
				t.Error("probe reported success")
			}
		})
	}
}

// TestProbeReportsAcceptedTransaction runs a transactional statement through
// the blocked strategy. The probe must not fail; it reports the acceptance
// and leaves the verdict to the matrix, which is what catches a statement
// wrongly modelled as notx.
func TestProbeReportsAcceptedTransaction(t *testing.T) {
	if shared == nil {
		t.Skip("docker unavailable")
	}

	observed, err := lockmatrix.Probe(t.Context(), shared, lockmatrix.Case{
		Name:    "drop_table_as_blocked",
		Seed:    seeded,
		SQL:     "DROP TABLE t",
		Blocked: true,
		Guard:   "LOCK TABLE t IN ACCESS SHARE MODE",
	})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}

	if observed.RefusedTx {
		t.Error("DROP TABLE reported as refusing a transaction block")
	}
}

// TestLockMatrix holds every prediction to the server: the locks taken, the
// refusal of transaction blocks, whether the storage was rewritten, and what
// the server reported visiting.
func TestLockMatrix(t *testing.T) {
	if shared == nil {
		t.Skip("docker unavailable")
	}

	for _, c := range matrix {
		t.Run(c.Name, func(t *testing.T) {
			lockmatrix.Verify(t, shared, c, major)
		})
	}
}
