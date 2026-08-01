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

package verify

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/tochemey/mig/test/harness"
)

// shared is the container for this package, or nil when docker is absent.
var shared *harness.Harness

// measurable is a workload short enough to run in a test and shaped like the
// real thing: fast traffic, and a reader slow enough to make a queue.
const measurable = `
setup:
  - CREATE TABLE probe (id bigint PRIMARY KEY, name text)
  - INSERT INTO probe SELECT g, 'n' || g FROM generate_series(1, 200) g
keys: 200
baseline: 400ms
settle: 400ms
queries:
  - name: point_read
    sql: SELECT name FROM probe WHERE id = $1
    key: true
    rate: 100
slow_read:
  sql: SELECT count(*) FROM probe WHERE id <= 2 AND pg_sleep(0.2) IS NULL
  every: 300ms
`

// TestMain brings up one container for the package.
func TestMain(m *testing.M) {
	os.Exit(harness.Main(m, 0, func(_ context.Context, h *harness.Harness) error {
		shared = h
		return nil
	}))
}

// TestRunMeasuresBothWindowsAndTheServer covers the whole shape: traffic
// before, traffic during, and what the server was waiting on throughout.
func TestRunMeasuresBothWindowsAndTheServer(t *testing.T) {
	control, work := pools(t)

	config, err := ParseConfig([]byte(measurable))
	if err != nil {
		t.Fatalf("workload: %v", err)
	}

	blocked := make(chan struct{})

	report, err := Run(t.Context(), control, work, Options{
		Config:     config,
		Migrations: fstest.MapFS{},
		Apply: func(ctx context.Context, db *sql.DB, _ fs.FS) error {
			// An ACCESS EXCLUSIVE lock held while the traffic runs, which is
			// what a migration does to a table and what the sampler is here
			// to see.
			close(blocked)

			_, err := db.ExecContext(ctx,
				"BEGIN; LOCK TABLE probe IN ACCESS EXCLUSIVE MODE; SELECT pg_sleep(0.4); COMMIT")

			return err
		},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if !report.Applied {
		t.Fatalf("the applier failed: %s", report.ApplyError)
	}

	if report.Baseline.Count == 0 || report.During.Count == 0 {
		t.Fatalf("nothing was measured: baseline %d, during %d",
			report.Baseline.Count, report.During.Count)
	}

	// The point of the whole exercise: holding the table hurt the traffic
	// that wanted it.
	if report.During.Max <= report.Baseline.Max {
		t.Errorf("during max %s is no worse than baseline max %s",
			report.During.Max, report.Baseline.Max)
	}

	if report.Longest.Relation != "probe" {
		t.Errorf("longest block was on %q, want probe", report.Longest.Relation)
	}

	if len(report.Waits) == 0 {
		t.Error("the sampler attributed nothing")
	}
}

// TestRunReportsAnApplierThatFailed covers the verdict a failed migration is:
// the windows are still measured and the report says what happened.
func TestRunReportsAnApplierThatFailed(t *testing.T) {
	control, work := pools(t)

	config, err := ParseConfig([]byte(measurable))
	if err != nil {
		t.Fatalf("workload: %v", err)
	}

	report, err := Run(t.Context(), control, work, Options{
		Config:     config,
		Migrations: fstest.MapFS{},
		Apply: func(context.Context, *sql.DB, fs.FS) error {
			return errors.New("it would not go through")
		},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if report.Applied || !report.Failed() {
		t.Errorf("a migration that never applied passed: %+v", report)
	}

	if report.ApplyError == "" {
		t.Error("the report does not say why it failed")
	}
}

// TestRunRefusesWithoutAnApplier covers the caller that forgot the executor.
func TestRunRefusesWithoutAnApplier(t *testing.T) {
	control, work := pools(t)

	config, err := ParseConfig([]byte(measurable))
	if err != nil {
		t.Fatalf("workload: %v", err)
	}

	if _, err := Run(t.Context(), control, work, Options{Config: config}); err == nil {
		t.Error("a run with no applier reported success")
	}
}

// TestRunReportsASetupThatCannotRun covers the workload whose schema does not
// build, which is a mistake in the file rather than a finding.
func TestRunReportsASetupThatCannotRun(t *testing.T) {
	control, work := pools(t)

	_, err := Run(t.Context(), control, work, Options{
		Config: Config{Setup: []string{"CREATE TABLE"}},
		Apply:  func(context.Context, *sql.DB, fs.FS) error { return nil },
	})

	if err == nil || !strings.Contains(err.Error(), "workload setup") {
		t.Errorf("err = %v, want the setup that would not run", err)
	}
}

// TestRunStopsWhenItsCallerDoes covers the windows being interrupted: a run
// whose context ends mid-measurement reports why rather than a report built
// on half a window.
func TestRunStopsWhenItsCallerDoes(t *testing.T) {
	config, err := ParseConfig([]byte(measurable))
	if err != nil {
		t.Fatalf("workload: %v", err)
	}

	cases := map[string]time.Duration{
		"during the baseline": 0,
		"during the settle":   config.Baseline + 100*time.Millisecond,
	}

	for name, after := range cases {
		t.Run(name, func(t *testing.T) {
			// Its own database: the setup builds the same table each time.
			control, work := pools(t)

			ctx, cancel := context.WithTimeout(t.Context(), after+50*time.Millisecond)
			defer cancel()

			_, err := Run(ctx, control, work, Options{
				Config:     config,
				Migrations: fstest.MapFS{},
				Apply:      func(context.Context, *sql.DB, fs.FS) error { return nil },
			})

			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("err = %v, want the deadline", err)
			}
		})
	}
}

// pools gives one test its own database and the two pools a run needs.
func pools(t *testing.T) (control, work *sql.DB) {
	t.Helper()

	if shared == nil {
		t.Skip("postgres container not available")
	}

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

// TestSamplerSurvivesAClosedPool covers the reading that cannot be taken: the
// sampler counts it rather than stopping, and the count reaches the report so
// a quiet attribution cannot pass for a quiet server.
func TestSamplerSurvivesAClosedPool(t *testing.T) {
	control, _ := pools(t)

	if err := control.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	sampler := Watch(t.Context(), control)

	time.Sleep(3 * sampleEvery)

	waits, blocked, longest, failures := sampler.Stop()

	if failures == 0 {
		t.Error("readings against a closed pool were counted as successes")
	}

	if len(waits) != 0 || blocked != 0 || longest.Relation != "" {
		t.Errorf("a sampler that read nothing reported %v, %d, %+v", waits, blocked, longest)
	}
}
