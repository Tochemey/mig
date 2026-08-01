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

// The chaos harness's acceptance: it has to catch a migration that blocks
// traffic and pass one that does not. Both halves are the test, because a
// harness that fails everything and one that fails nothing are equally
// useless.
package chaos_test

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/tochemey/mig/pkg/mig"
	"github.com/tochemey/mig/test/harness"
)

// workload is the traffic: fast reads and writes, and the long reader that
// makes lock queueing visible. The reader sleeps per row over three rows, so
// it holds ACCESS SHARE for about a third of a second and then lets go, which
// is a slow query rather than a stuck one. Without that reader an ACCESS EXCLUSIVE
// statement is granted at once and blocks nobody, which is exactly how a
// chaos harness comes to report all clear on migrations that take a site
// down.
const workload = `
setup:
  - CREATE TABLE accounts (id bigint PRIMARY KEY, name text NOT NULL)
  - INSERT INTO accounts SELECT g, 'u' || g FROM generate_series(1, 20000) g
  - ANALYZE accounts
keys: 20000
baseline: 1s
settle: 1500ms
queries:
  - name: point_read
    sql: SELECT name FROM accounts WHERE id = $1
    key: true
    rate: 200
  - name: point_write
    sql: UPDATE accounts SET name = name WHERE id = $1
    key: true
    rate: 50
slow_read:
  sql: SELECT count(*) FROM accounts WHERE id <= 3 AND pg_sleep(0.3) IS NULL
  every: 500ms
`

// knownBad rewrites the table under ACCESS EXCLUSIVE with the lock timeout
// turned off, which is the shape L025 flags without running anything: it
// queues behind whatever the slow reader is holding, and because Postgres
// grants locks in order, every reader arriving after it waits for both.
const knownBad = `-- +mig step: add_token
-- +mig no_lock_timeout
ALTER TABLE accounts ADD COLUMN token uuid NOT NULL DEFAULT gen_random_uuid();
`

// knownGood builds an index without blocking writes, which is what the
// linter's own advice produces.
const knownGood = `-- +mig step: index_name
-- +mig notx
CREATE INDEX CONCURRENTLY idx_accounts_name ON accounts (name);
`

// budget is what the run may cost, in both of the terms the design pairs.
//
// max_block is what catches this one. A migration that blocks for a third of
// a second inside a window measured in seconds hurts a small share of the
// queries, so the client-side p99 can sit under a budget the server-side
// attribution blows through: the blocked queries are in the worst five
// percent, not the worst one. Measuring from both sides is the point.
const budget = "p99=100ms,max_block=100ms"

// shared is the container for this package, or nil when docker is absent.
var shared *harness.Harness

// TestMain brings up one container for the package.
func TestMain(m *testing.M) {
	os.Exit(harness.Main(m, 0, func(_ context.Context, h *harness.Harness) error {
		shared = h
		return nil
	}))
}

// TestChaosCatchesWhatItMustAndPassesWhatItMust is V5's acceptance.
func TestChaosCatchesWhatItMustAndPassesWhatItMust(t *testing.T) {
	if shared == nil {
		t.Skip("postgres container not available")
	}

	cases := []struct {
		name      string
		migration string
		wantFail  bool
	}{
		{name: "a rewrite behind a long read is caught", migration: knownBad, wantFail: true},
		{name: "a concurrent index build passes", migration: knownGood},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := chaos(t, tc.migration)

			if report.Failed() != tc.wantFail {
				t.Errorf("failed = %v, want %v\n%s", report.Failed(), tc.wantFail, render(t, report))
			}

			// Whatever the verdict, the harness has to have measured
			// something: a run that recorded nothing agrees with every
			// expectation and means nothing.
			if report.Baseline.Count == 0 || report.During.Count == 0 {
				t.Errorf("nothing was measured: baseline %d, during %d",
					report.Baseline.Count, report.During.Count)
			}

			t.Log("\n" + render(t, report))
		})
	}
}

// TestChaosRefusesAWorkloadWithoutASlowReader covers the design's review
// checklist: a workload with no long-running reader cannot reproduce the
// hazard the harness exists for, so it is refused rather than run.
func TestChaosRefusesAWorkloadWithoutASlowReader(t *testing.T) {
	without := strings.Split(workload, "slow_read:")[0]

	if _, err := mig.ParseWorkload([]byte(without)); err == nil {
		t.Fatal("a workload with no slow reader was accepted")
	}
}

// chaos runs one migration under the workload and returns what it measured.
func chaos(t *testing.T, migration string) mig.ChaosReport {
	t.Helper()

	config, err := mig.ParseWorkload([]byte(workload))
	if err != nil {
		t.Fatalf("workload: %v", err)
	}

	budget, err := mig.ParseBudget(budget)
	if err != nil {
		t.Fatalf("budget: %v", err)
	}

	control, work := pools(t)

	fsys := fstest.MapFS{"20240817120000_case.sql": &fstest.MapFile{Data: []byte(migration)}}

	report, err := mig.Chaos(t.Context(), control, work, fsys, config, budget)
	if err != nil {
		t.Fatalf("chaos: %v", err)
	}

	return report
}

// pools gives the run its own database and the two pools it needs: the
// workload's and the migration's, which must not be the same one.
func pools(t *testing.T) (control, work *sql.DB) {
	t.Helper()

	name, err := shared.Clone(t.Context(), harness.Template)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	t.Cleanup(func() {
		if err := shared.DropDatabase(context.Background(), name); err != nil {
			t.Errorf("drop %q: %v", name, err)
		}
	})

	for _, pool := range []**sql.DB{&control, &work} {
		db, err := shared.Open(t.Context(), name)
		if err != nil {
			t.Fatalf("open: %v", err)
		}

		t.Cleanup(func() {
			if err := db.Close(); err != nil {
				t.Errorf("close pool: %v", err)
			}
		})

		*pool = db
	}

	return control, work
}

// render is the report as a person would read it.
func render(t *testing.T, report mig.ChaosReport) string {
	t.Helper()

	out := &strings.Builder{}

	if err := mig.RenderChaos(out, report); err != nil {
		t.Fatalf("render: %v", err)
	}

	return out.String()
}
