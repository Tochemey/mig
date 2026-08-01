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

// The flip matrix runs the behaviour the linter conditions on the target
// version against servers either side of each change: the major before it and
// the first major with it. The version-wide matrix runs on the majors the
// project supports and so only ever sees the modern half of every flip, which
// leaves the other half asserted by documentation alone.
//
// It is gated because it pulls images of majors Postgres no longer supports,
// and because released behaviour does not change: what this guards against is
// the linter's own version conditions drifting. Nightly is often enough.
package flipmatrix_test

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/tochemey/mig/internal/lint/lockmodel"
	"github.com/tochemey/mig/internal/lint/rules"
	"github.com/tochemey/mig/pkg/mig"
	"github.com/tochemey/mig/test/harness"
	"github.com/tochemey/mig/test/lockmatrix"
)

// seeded is the fixture the cases operate on, small enough to run a matrix on
// and large enough that a rewrite writes real pages.
var seeded = []string{
	"CREATE TABLE t (id int, c int)",
	"INSERT INTO t SELECT g, g FROM generate_series(1, 500) g",
}

// partitioned is a partitioned parent with a table waiting to be attached.
var partitioned = []string{
	"CREATE TABLE parted (id int) PARTITION BY RANGE (id)",
	"CREATE TABLE part1 PARTITION OF parted FOR VALUES FROM (0) TO (100)",
	"CREATE TABLE part2 (id int)",
}

// notNullPattern is the safe route to NOT NULL: prove the column with a
// check, validate it without blocking writes, then set the flag.
var notNullPattern = slices.Concat(seeded, []string{
	"UPDATE t SET id = 1 WHERE id IS NULL",
	"ALTER TABLE t ADD CONSTRAINT t_id_nn CHECK (id IS NOT NULL) NOT VALID",
	"ALTER TABLE t VALIDATE CONSTRAINT t_id_nn",
})

// servers holds one container per major, since several flips share a side.
var (
	serversMu sync.Mutex
	servers   = map[int]*harness.Harness{}
)

// flip is one behaviour the linter conditions on the version, stated as the
// same statement observed on both sides of the change. The two cases differ
// only where the server does.
type flip struct {
	name         string
	below, above int
	before       lockmatrix.Case
	after        lockmatrix.Case
}

var flips = []flip{
	{
		// Before 11 a default meant every existing row was written out with
		// it. From 11 the value is stored in the catalog as a missing value
		// and the rows are left alone.
		name:  "add_column_with_a_constant_default",
		below: beforeDefaults,
		above: withDefaults,
		before: lockmatrix.Case{
			Name:   "add_column_constant_default",
			Seed:   seeded,
			SQL:    "ALTER TABLE t ADD COLUMN d int DEFAULT 42",
			Visits: []string{"t"},
		},
		after: lockmatrix.Case{
			Name: "add_column_constant_default",
			Seed: seeded,
			SQL:  "ALTER TABLE t ADD COLUMN d int DEFAULT 42",
		},
	},
	{
		// Attaching a partition took ACCESS EXCLUSIVE on the parent, which
		// stopped every read of the whole partitioned table. From 12 the
		// parent is only held at SHARE UPDATE EXCLUSIVE.
		name:  "attach_partition",
		below: beforeSkip,
		above: withSkip,
		before: lockmatrix.Case{
			Name:   "attach_partition",
			Seed:   partitioned,
			SQL:    "ALTER TABLE parted ATTACH PARTITION part2 FOR VALUES FROM (100) TO (200)",
			Visits: []string{"part2"},
		},
		after: lockmatrix.Case{
			Name:   "attach_partition",
			Seed:   partitioned,
			SQL:    "ALTER TABLE parted ATTACH PARTITION part2 FOR VALUES FROM (100) TO (200)",
			Visits: []string{"part2"},
		},
	},
	{
		// Renaming an index took ACCESS EXCLUSIVE, blocking reads of the
		// table it belongs to for a catalog update. From 12 it needs only
		// SHARE UPDATE EXCLUSIVE.
		name:  "rename_index",
		below: beforeSkip,
		above: withSkip,
		before: lockmatrix.Case{
			Name: "rename_index",
			Seed: slices.Concat(seeded, []string{"CREATE INDEX t_idx ON t (id)"}),
			SQL:  "ALTER INDEX t_idx RENAME TO t_idx2",
		},
		after: lockmatrix.Case{
			Name: "rename_index",
			Seed: slices.Concat(seeded, []string{"CREATE INDEX t_idx ON t (id)"}),
			SQL:  "ALTER INDEX t_idx RENAME TO t_idx2",
		},
	},
}

// EnvFlips turns the flip matrix on. Without it the package skips, so a
// developer's ./... stays on the majors the project supports.
const EnvFlips = "MIG_FLIP_MATRIX"

// The majors either side of the changes the linter knows about.
const (
	beforeDefaults = 10 // ADD COLUMN with a default still rewrote
	withDefaults   = 11 // and from here it does not
	beforeSkip     = 11 // SET NOT NULL still scanned, enums refused transactions
	withSkip       = 12 // and from here neither is so
)

// TestMain keeps the containers for the whole package and closes them once.
func TestMain(m *testing.M) {
	code := m.Run()

	for _, h := range servers {
		if err := h.Close(context.Background()); err != nil {
			fmt.Fprintln(os.Stderr, strings.TrimSpace(err.Error()))
		}
	}

	os.Exit(code)
}

// TestFlips holds the model's version-conditional predictions to the servers
// either side of each change. The lock modes, the rewrite and the scan are
// compared exactly as the version-wide matrix compares them.
func TestFlips(t *testing.T) {
	for _, f := range flips {
		t.Run(f.name, func(t *testing.T) {
			sides := []struct {
				major int
				c     lockmatrix.Case
			}{
				{f.below, f.before},
				{f.above, f.after},
			}

			for _, side := range sides {
				t.Run(fmt.Sprint(side.major), func(t *testing.T) {
					lockmatrix.Verify(t, server(t, side.major), side.c, side.major)
				})
			}
		})
	}
}

// TestNotNullScanSkipMatchesTheRule is L005's version condition, held to the
// server from both ends: the rule stays silent exactly on the majors where a
// validated check really does spare SET NOT NULL its scan.
func TestNotNullScanSkipMatchesTheRule(t *testing.T) {
	migration := "-- +mig step: prove\n" +
		"ALTER TABLE t ADD CONSTRAINT t_id_nn CHECK (id IS NOT NULL) NOT VALID;\n\n" +
		"-- +mig step: validate\n-- +mig notx\n" +
		"ALTER TABLE t VALIDATE CONSTRAINT t_id_nn;\n\n" +
		"-- +mig step: require\n" +
		"ALTER TABLE t ALTER COLUMN id SET NOT NULL;\n"

	for _, major := range []int{beforeSkip, withSkip} {
		t.Run(fmt.Sprint(major), func(t *testing.T) {
			h := server(t, major)

			observed, err := lockmatrix.Probe(t.Context(), h, lockmatrix.Case{
				Name: "set_not_null_proven",
				Seed: notNullPattern,
				SQL:  "ALTER TABLE t ALTER COLUMN id SET NOT NULL",
			})
			if err != nil {
				t.Fatalf("probe: %v", err)
			}

			flagged := flaggedBy(t, migration, major, rules.L005)

			if scanned := observed.Scanned["t"]; scanned != flagged {
				t.Errorf("the server %s the table and the rule %s: %q",
					reported(scanned, "scanned", "left alone"),
					reported(flagged, "flags the pattern", "stays silent"),
					observed.Debug)
			}
		})
	}
}

// TestEnumValueTransactionRefusalMatchesTheModel is the enum condition: before
// 12 a new value could not be added inside a transaction block at all, which
// is what the model marks notx.
func TestEnumValueTransactionRefusalMatchesTheModel(t *testing.T) {
	const (
		seed = "CREATE TYPE mood AS ENUM ('flat')"
		sql  = "ALTER TYPE mood ADD VALUE 'curious'"
	)

	for _, major := range []int{beforeSkip, withSkip} {
		t.Run(fmt.Sprint(major), func(t *testing.T) {
			h := server(t, major)

			analysis, err := lockmodel.Analyze(sql, major)
			if err != nil {
				t.Fatalf("analyze: %v", err)
			}

			refused, err := lockmatrix.RefusesTransaction(t.Context(), h, []string{seed}, sql)
			if err != nil {
				t.Fatalf("refuses transaction: %v", err)
			}

			if refused != analysis.NoTx {
				t.Errorf("the server %s the value inside a transaction, the model predicts notx = %v",
					reported(refused, "refused", "accepted"), analysis.NoTx)
			}
		})
	}
}

// reported picks the wording for an observation, so a failure reads as a
// sentence rather than as two booleans.
func reported(yes bool, when, otherwise string) string {
	if yes {
		return when
	}

	return otherwise
}

// flaggedBy reports whether linting the migration at major produces the rule.
func flaggedBy(t *testing.T, migration string, major int, rule string) bool {
	t.Helper()

	fsys := fstest.MapFS{"20240817120000_pattern.sql": &fstest.MapFile{Data: []byte(migration)}}

	linted, err := mig.Lint(fsys, major, nil)
	if err != nil {
		t.Fatalf("lint: %v", err)
	}

	for _, finding := range linted.Findings {
		if finding.RuleID == rule {
			return true
		}
	}

	return false
}

// server returns the container for a major, starting it on first use. The
// whole package skips without the gate, and skips without docker.
func server(t *testing.T, major int) *harness.Harness {
	t.Helper()

	if os.Getenv(EnvFlips) == "" {
		t.Skipf("set %s to run the flip matrix against unsupported majors", EnvFlips)
	}

	serversMu.Lock()
	defer serversMu.Unlock()

	if h, ok := servers[major]; ok {
		return h
	}

	ctx := context.Background()

	h, err := harness.New(ctx, harness.WithImage(image(major)))
	if err != nil {
		t.Skipf("postgres %d unavailable: %v", major, err)
	}

	if err := h.SeedTemplate(ctx, harness.Template, 0); err != nil {
		if closeErr := h.Close(ctx); closeErr != nil {
			t.Errorf("close the postgres %d container: %v", major, closeErr)
		}

		t.Fatalf("seed template on postgres %d: %v", major, err)
	}

	servers[major] = h

	return h
}

// image names the alpine image for a major.
func image(major int) string {
	return fmt.Sprintf("postgres:%d-alpine", major)
}
