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

// The connected linter's acceptance: an estimate taken from a calibration
// probe has to land within twice the truth on a table of ten million rows.
//
// A number nobody checked is worse than no number, because it will be quoted
// in a change review. This is the check.
//
// It is gated because it is a timing measurement, and a timing measurement
// shares nothing well. Run inside a whole-suite run it competes with every
// other package's containers, the probe and the rewrite it predicts see
// different machines, and the comparison measures the load rather than the
// model. CI runs it as a step of its own, on every change.
package estimate_test

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/tochemey/mig/internal/lint/lockmodel"
	"github.com/tochemey/mig/internal/lint/stats"
	"github.com/tochemey/mig/test/harness"
)

// EnvEstimate turns the acceptance on. Without it the package skips, so a
// whole-suite run does not fail on a number it was in no position to measure.
const EnvEstimate = "MIG_ESTIMATE"

// acceptanceRows is the table the design names: ten million.
const acceptanceRows = 10_000_000

// tolerance is the factor the estimate has to land within, either way.
const tolerance = 2.0

// shared is the container for this package, or nil when docker is absent.
var shared *harness.Harness

// TestMain brings up one container for the package.
func TestMain(m *testing.M) {
	os.Exit(harness.Main(m, 0, func(_ context.Context, h *harness.Harness) error {
		shared = h
		return nil
	}))
}

// TestEstimateLandsWithinTwiceTheTruth calibrates against a table of its own
// making, estimates the rewrite of a ten-million-row table, then rewrites it
// and compares.
func TestEstimateLandsWithinTwiceTheTruth(t *testing.T) {
	if os.Getenv(EnvEstimate) == "" {
		t.Skipf("set %s to run the estimate acceptance, on a machine it has to itself", EnvEstimate)
	}

	if shared == nil {
		t.Skip("postgres container not available")
	}

	db := newDatabase(t)

	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatalf("pin a connection: %v", err)
	}

	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("return connection: %v", err)
		}
	}()

	exec(t, db, "CREATE TABLE big (id bigint, n int)")
	exec(t, db, "INSERT INTO big SELECT g, g FROM generate_series(1, $1) g", acceptanceRows)
	exec(t, db, "ANALYZE big")

	snapshot, err := stats.Collect(t.Context(), db, []lockmodel.Relation{{Name: "big"}})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}

	// Calibrated last, immediately before the statement it predicts. Seeding
	// ten million rows takes long enough that a machine busy with the rest of
	// the suite can be a different machine by the end of it, and what is under
	// test is the model, not the load either side of it.
	throughput, err := stats.Calibrate(t.Context(), conn)
	if err != nil {
		t.Fatalf("calibrate: %v", err)
	}

	table := snapshot.Table(lockmodel.Relation{Name: "big"})

	low, high, ok := throughput.Estimate(lockmodel.Rewrite, table.Rows, table.Bytes)
	if !ok {
		t.Fatalf("no estimate for a table of %d bytes at %+v", table.Bytes, throughput)
	}

	// The statement the estimate was about: a volatile default writes every
	// row out again, which is the rewrite the model predicts.
	started := time.Now()

	exec(t, db, "ALTER TABLE big ADD COLUMN filled float8 DEFAULT random()")

	measured := time.Since(started)

	middle := (low + high) / 2

	t.Logf("%d rows, %d bytes: estimated %s to %s, measured %s",
		table.Rows, table.Bytes, low, high, measured)

	if float64(measured) > tolerance*float64(middle) || float64(middle) > tolerance*float64(measured) {
		t.Errorf("estimate %s (range %s to %s) is more than %gx from the measured %s",
			middle, low, high, tolerance, measured)
	}
}

// newDatabase gives the test its own clone of the template.
func newDatabase(t *testing.T) *sql.DB {
	t.Helper()

	name, err := shared.Clone(t.Context(), harness.Template)
	if err != nil {
		t.Fatalf("clone template: %v", err)
	}

	db, err := shared.Open(t.Context(), name)
	if err != nil {
		t.Fatalf("open clone: %v", err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close pool: %v", err)
		}

		if err := shared.DropDatabase(context.Background(), name); err != nil {
			t.Errorf("drop clone %q: %v", name, err)
		}
	})

	return db
}

// exec runs a statement or fails the test.
func exec(t *testing.T, db *sql.DB, stmt string, args ...any) {
	t.Helper()

	if _, err := db.ExecContext(t.Context(), stmt, args...); err != nil {
		t.Fatalf("exec %q: %v", stmt, err)
	}
}
