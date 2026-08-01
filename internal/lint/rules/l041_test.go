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
	"testing"
	"testing/fstest"

	"github.com/tochemey/mig/internal/lint"
	"github.com/tochemey/mig/internal/lint/lockmodel"
	"github.com/tochemey/mig/internal/lint/rules"
	"github.com/tochemey/mig/internal/lint/stats"
	"github.com/tochemey/mig/internal/plan"
)

// TestL041 runs the rule's fixture, which is silent: the rule needs a size
// only the catalog knows, and the offline pass has none.
func TestL041(t *testing.T) { golden(t, "l041") }

// TestL041FiresOnlyOnASizeTheCatalogGave covers the half the fixture cannot
// show: the rule is about bloat on a large table, and large is not something
// the offline pass knows.
func TestL041FiresOnlyOnASizeTheCatalogGave(t *testing.T) {
	const migration = "-- +mig step: prune\nDELETE FROM sessions WHERE expires_at < now();\n"

	sessions := lockmodel.Relation{Name: "sessions"}

	cases := []struct {
		name     string
		snapshot *stats.Snapshot
		want     bool
	}{
		{name: "offline it stays silent"},
		{
			name: "a small table is not worth the words",
			snapshot: stats.Of(map[lockmodel.Relation]stats.Table{
				sessions: {Exists: true, Rows: 500, Bytes: 1 << 20},
			}),
		},
		{
			name: "a large table is",
			snapshot: stats.Of(map[lockmodel.Relation]stats.Table{
				sessions: {Exists: true, Rows: 40_000_000, Bytes: 8 << 30},
			}),
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fsys := fstest.MapFS{"1_m.sql": &fstest.MapFile{Data: []byte(migration)}}

			loaded, err := plan.LoadFS(fsys)
			if err != nil {
				t.Fatalf("load: %v", err)
			}

			result, err := lint.Run(fsys, loaded, 18, tc.snapshot, nil)
			if err != nil {
				t.Fatalf("run: %v", err)
			}

			findings := result.Findings
			fired := false

			for _, finding := range findings {
				if finding.RuleID == rules.L041 {
					fired = true
				}
			}

			if fired != tc.want {
				t.Errorf("fired = %v, want %v: %+v", fired, tc.want, findings)
			}
		})
	}
}
