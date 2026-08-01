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

package rules_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/tochemey/mig/internal/lint"
	"github.com/tochemey/mig/internal/lint/lockmodel"
	"github.com/tochemey/mig/internal/lint/report"
	"github.com/tochemey/mig/internal/lint/rules"
	"github.com/tochemey/mig/internal/lint/stats"
	"github.com/tochemey/mig/internal/plan"
)

// sizedHazard is one statement whose cost is the table it names, so a run of
// it says what the grader made of that table.
const sizedHazard = "-- +mig step: widen\nALTER TABLE t ALTER COLUMN c TYPE bigint;\n"

// update rewrites the golden files instead of comparing against them. Run
// with: go test ./internal/lint/rules -update, then audit the diff by hand.
var update = flag.Bool("update", false, "rewrite the golden files")

// golden runs one fixture through the whole pipeline at the newest supported
// major and compares the findings against the fixture's golden JSON.
func golden(t *testing.T, id string) {
	t.Helper()
	goldenAt(t, id, 18)
}

// goldenAt is golden pinned to an older target version, for the rules whose
// behaviour flips with it.
func goldenAt(t *testing.T, id string, version int) {
	t.Helper()

	source, err := os.ReadFile(filepath.Join("testdata", id+".sql"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	fsys := fstest.MapFS{"1_" + id + ".sql": &fstest.MapFile{Data: source}}

	loaded, err := plan.LoadFS(fsys)
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}

	result, err := lint.Run(fsys, loaded, version, nil, nil)
	if err != nil {
		t.Fatalf("lint fixture: %v", err)
	}

	var got bytes.Buffer

	if err := report.JSON(&got, result.Findings); err != nil {
		t.Fatalf("render findings: %v", err)
	}

	path := filepath.Join("testdata", id+".json")

	if *update {
		if err := os.WriteFile(path, got.Bytes(), 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (create it with -update): %v", err)
	}

	if !bytes.Equal(got.Bytes(), want) {
		t.Errorf("findings diverge from %s\ngot:\n%s\nwant:\n%s", path, got.Bytes(), want)
	}
}

// lintWith runs one statement against a snapshot and returns what came back.
func lintWith(t *testing.T, snapshot *stats.Snapshot) rules.Finding {
	t.Helper()

	fsys := fstest.MapFS{"1_m.sql": &fstest.MapFile{Data: []byte(sizedHazard)}}

	loaded, err := plan.LoadFS(fsys)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	result, err := lint.Run(fsys, loaded, 18, snapshot, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	findings := result.Findings

	if len(findings) != 1 {
		t.Fatalf("found %d findings, want the rewrite: %+v", len(findings), findings)
	}

	return findings[0]
}

// snapshotOf builds a snapshot holding one table t, measured at a rate that
// makes the arithmetic in the expectations legible: ten megabytes a second.
func snapshotOf(table stats.Table) *stats.Snapshot {
	return stats.Of(map[lockmodel.Relation]stats.Table{{Name: "t"}: table}).
		WithThroughput(stats.Throughput{
			Rewrite: 10 << 20, IndexBuild: 10 << 20,
			// Fast enough per row that the size is what decides these
			// estimates; the two costs are weighed against each other in the
			// stats package's own tests.
			RewriteRows: 1 << 30, IndexRows: 1 << 30,
		})
}

// TestSeverityFollowsTheTableSize is §6, the rule the linter's credibility
// rests on: the same statement is an outage, a warning or an observation
// depending on what it is pointed at, and offline it is always a warning
// because nothing there knows.
func TestSeverityFollowsTheTableSize(t *testing.T) {
	cases := []struct {
		name     string
		snapshot *stats.Snapshot
		want     rules.Severity
	}{
		{
			name: "offline nothing is known and nothing is claimed",
			want: rules.SeverityWarn,
		},
		{
			name:     "a table the catalog does not have yet",
			snapshot: snapshotOf(stats.Table{}),
			want:     rules.SeverityWarn,
		},
		{
			name:     "a lookup table",
			snapshot: snapshotOf(stats.Table{Exists: true, Rows: 12, Bytes: 8 << 10}),
			want:     rules.SeverityInfo,
		},
		{
			name:     "a table between the two",
			snapshot: snapshotOf(stats.Table{Exists: true, Rows: 200_000, Bytes: 64 << 20}),
			want:     rules.SeverityWarn,
		},
		{
			name:     "a table large enough by rows",
			snapshot: snapshotOf(stats.Table{Exists: true, Rows: 41_200_000, Bytes: 64 << 20}),
			want:     rules.SeverityError,
		},
		{
			name:     "a table large enough by size, never analysed",
			snapshot: snapshotOf(stats.Table{Exists: true, Bytes: 2 << 30}),
			want:     rules.SeverityError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lintWith(t, tc.snapshot).Severity; got != tc.want {
				t.Errorf("severity = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestDetailCarriesTheSizeItWasGradedOn covers the reporting half: a reader
// who disagrees with the grade has to be able to see what it was based on.
func TestDetailCarriesTheSizeItWasGradedOn(t *testing.T) {
	cases := []struct {
		name  string
		table stats.Table
		want  string
	}{
		{
			name:  "rows and size when the table has been analysed",
			table: stats.Table{Exists: true, Rows: 41_200_000, Bytes: 365 << 30},
			want:  "on t (41.2M rows, 365.0 GB), held for a table rewrite",
		},
		{
			name:  "size alone when it has not",
			table: stats.Table{Exists: true, Bytes: 2 << 30},
			want:  "on t (2.0 GB), held for a table rewrite",
		},
		{
			name:  "neither when the catalog does not have the table",
			table: stats.Table{},
			want:  "on t, held for a table rewrite",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if detail := lintWith(t, snapshotOf(tc.table)).Detail; !strings.Contains(detail, tc.want) {
				t.Errorf("detail = %q, want it to carry %q", detail, tc.want)
			}
		})
	}
}

// TestSizesReadTheWayAPersonSaysThem covers the small end of both scales,
// where an exact figure beats a rounded one.
func TestSizesReadTheWayAPersonSaysThem(t *testing.T) {
	detail := lintWith(t, snapshotOf(stats.Table{Exists: true, Rows: 12, Bytes: 512})).Detail

	if !strings.Contains(detail, "(12 rows, 512 B)") {
		t.Errorf("detail = %q, want the exact figures", detail)
	}
}

// TestEstimateIsReportedForWorkWorthWaitingFor covers the estimate line: it
// scales with the table, states its method, and is left off work that is over
// before anyone looks.
func TestEstimateIsReportedForWorkWorthWaitingFor(t *testing.T) {
	// A gibibyte at ten mebibytes a second is a hundred and two seconds, and
	// the range brackets it: three quarters of it to twice it.
	big := lintWith(t, snapshotOf(stats.Table{Exists: true, Rows: 4_000_000, Bytes: 1 << 30}))

	for _, want := range []string{"estimated 1m17s to 3m25s", "measured throughput"} {
		if !strings.Contains(big.Estimate, want) {
			t.Errorf("estimate = %q, want it to carry %q", big.Estimate, want)
		}
	}

	small := lintWith(t, snapshotOf(stats.Table{Exists: true, Rows: 12, Bytes: 8 << 10}))
	if small.Estimate != "" {
		t.Errorf("a table of 8 kB was estimated at %q", small.Estimate)
	}

	// Without a probe there is no rate to scale by, and no estimate is owed.
	unmeasured := stats.Of(map[lockmodel.Relation]stats.Table{
		{Name: "t"}: {Exists: true, Rows: 4_000_000, Bytes: 1 << 30},
	})

	if estimate := lintWith(t, unmeasured).Estimate; estimate != "" {
		t.Errorf("an unmeasured server produced %q", estimate)
	}
}

// TestGeneratedBackfillsPageByTheRealKey covers what the catalog is worth to
// a fix: offline the key is a guess the comment owns up to, connected it is
// the table's own, and a composite key is left as a guess because the format
// pages by one column.
func TestGeneratedBackfillsPageByTheRealKey(t *testing.T) {
	const addColumn = "-- +mig step: add\n" +
		"ALTER TABLE t ADD COLUMN token uuid NOT NULL DEFAULT gen_random_uuid();\n"

	cases := []struct {
		name  string
		key   []string
		want  string
		avoid string
	}{
		{
			name:  "offline the key is assumed and said to be",
			want:  "key=id is assumed",
			avoid: "primary key\n",
		},
		{
			name: "connected it is the table's own",
			key:  []string{"user_id"},
			want: "key=user_id is the table's primary key",
		},
		{
			name:  "a composite key is not one column to page by",
			key:   []string{"tenant", "user_id"},
			want:  "key=id is assumed",
			avoid: "key=tenant",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fsys := fstest.MapFS{"1_m.sql": &fstest.MapFile{Data: []byte(addColumn)}}

			loaded, err := plan.LoadFS(fsys)
			if err != nil {
				t.Fatalf("load: %v", err)
			}

			var snapshot *stats.Snapshot

			if tc.key != nil {
				snapshot = stats.Of(map[lockmodel.Relation]stats.Table{
					{Name: "t"}: {Exists: true, Rows: 500, Bytes: 1 << 20, PrimaryKey: tc.key},
				})
			}

			result, err := lint.Run(fsys, loaded, 18, snapshot, nil)
			if err != nil {
				t.Fatalf("run: %v", err)
			}

			findings := result.Findings

			if len(findings) != 1 {
				t.Fatalf("found %d findings, want the rewriting default: %+v", len(findings), findings)
			}

			if !strings.Contains(findings[0].Fix, tc.want) {
				t.Errorf("fix does not page by %q:\n%s", tc.want, findings[0].Fix)
			}

			if tc.avoid != "" && strings.Contains(findings[0].Fix, tc.avoid) {
				t.Errorf("fix carries %q:\n%s", tc.avoid, findings[0].Fix)
			}
		})
	}
}

func TestCatalogueIDs(t *testing.T) {
	seen := make(map[string]bool)
	previous := ""

	for _, rule := range rules.All() {
		id := rule.ID()

		if seen[id] {
			t.Errorf("rule id %q appears twice", id)
		}

		if id <= previous {
			t.Errorf("rule id %q out of order after %q", id, previous)
		}

		seen[id] = true
		previous = id
	}

	if len(seen) != 24 {
		t.Errorf("catalog has %d rules, want 24", len(seen))
	}
}

// TestEveryRuleIsDescribed pins the catalog's one description of itself: it
// is what a code-scanning UI shows, and what tells a policy or a suppression
// naming a real rule from one naming a typo.
func TestEveryRuleIsDescribed(t *testing.T) {
	for _, rule := range rules.All() {
		if rules.Describe(rule.ID()) == "" {
			t.Errorf("rule %s has no description", rule.ID())
		}
	}

	// The linter's complaint about a directive it cannot honour is a rule id
	// like any other, and is described alongside them.
	if rules.Describe(rules.L000) == "" {
		t.Error("L000 has no description")
	}

	if rules.Describe("L999") != "" {
		t.Error("an id the catalog does not have came back described")
	}
}

func TestSeverityRendering(t *testing.T) {
	names := map[rules.Severity]string{
		rules.SeverityInfo:  "info",
		rules.SeverityWarn:  "warn",
		rules.SeverityError: "error",
		rules.Severity(0):   "unknown",
	}

	for severity, want := range names {
		if got := severity.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", severity, got, want)
		}

		encoded, err := json.Marshal(severity)
		if err != nil {
			t.Fatalf("marshal %q: %v", want, err)
		}

		if string(encoded) != `"`+want+`"` {
			t.Errorf("marshal %d = %s, want %q", severity, encoded, want)
		}
	}
}
