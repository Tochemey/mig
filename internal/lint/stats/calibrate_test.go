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

package stats

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/tochemey/mig/internal/lint/lockmodel"
)

// TestCalibrateMeasuresTheServerAndLeavesNothing runs the probe and then
// checks the calibration table is gone: the probe is a transaction it rolls
// back, and a linter that left tables behind on the database it was pointed
// at would deserve its reputation.
func TestCalibrateMeasuresTheServerAndLeavesNothing(t *testing.T) {
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

	throughput, err := Calibrate(t.Context(), conn)
	if err != nil {
		t.Fatalf("calibrate: %v", err)
	}

	if !throughput.Known() {
		t.Fatalf("throughput = %+v, want both rates measured", throughput)
	}

	var present bool

	err = conn.QueryRowContext(t.Context(),
		"SELECT to_regclass('mig_lint_calibration') IS NOT NULL").Scan(&present)
	if err != nil {
		t.Fatalf("look for the calibration table: %v", err)
	}

	if present {
		t.Error("the calibration table outlived the probe")
	}
}

// TestCalibrateFailsOnAClosedConnection covers the path a standby takes,
// where the write is refused and the caller reports the reason instead of
// pretending to an estimate.
func TestCalibrateFailsOnAClosedConnection(t *testing.T) {
	db := newDatabase(t)

	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatalf("pin a connection: %v", err)
	}

	if err := conn.Close(); err != nil {
		t.Fatalf("close connection: %v", err)
	}

	if _, err := Calibrate(t.Context(), conn); err == nil {
		t.Error("calibrate reported success on a closed connection")
	}
}

// brokenSession runs the probe against a real connection with one statement
// broken, which is how each of the probe's failures is reached without a
// broken server.
type brokenSession struct {
	conn   *sql.Conn
	broken string
}

func (s brokenSession) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if query == s.broken {
		return nil, errors.New("statement refused")
	}

	return s.conn.ExecContext(ctx, query, args...)
}

func (s brokenSession) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if query == s.broken {
		// A row carrying the server's own error, which is what a refused
		// query hands back.
		return s.conn.QueryRowContext(ctx, "SELECT no_such_function()")
	}

	return s.conn.QueryRowContext(ctx, query, args...)
}

// TestCalibrateReportsWhicheverStepFails walks the probe's statements and
// breaks each in turn: a server that will not be measured has to say so, not
// hand back a number nobody took.
func TestCalibrateReportsWhicheverStepFails(t *testing.T) {
	steps := map[string]string{
		"begin":    probeBegin,
		"create":   probeCreate,
		"fill":     probeFill,
		"size":     probeSize,
		"rewrite":  probeRewrite,
		"index":    probeIndex,
		"rollback": probeRollback,
	}

	for name, broken := range steps {
		t.Run(name, func(t *testing.T) {
			db := newDatabase(t)

			conn, err := db.Conn(t.Context())
			if err != nil {
				t.Fatalf("pin a connection: %v", err)
			}

			defer func() {
				if err := conn.Close(); err != nil {
					t.Errorf("return the connection: %v", err)
				}
			}()

			throughput, err := Calibrate(t.Context(), brokenSession{conn: conn, broken: broken})
			if err == nil {
				t.Fatal("calibrate reported success")
			}

			if throughput.Known() {
				t.Errorf("a failed probe still reported %+v", throughput)
			}
		})
	}
}

// TestRateRefusesATimingTooShortToMean covers the guard on the measurement
// itself: a statement that returned before the clock moved says nothing about
// how fast the server is.
func TestRateRefusesATimingTooShortToMean(t *testing.T) {
	if got := rate(1<<20, 100*time.Microsecond); got != 0 {
		t.Errorf("rate = %v, want no measurement", got)
	}

	if got := rate(1<<20, time.Second); got != 1<<20 {
		t.Errorf("rate = %v, want a megabyte a second", got)
	}
}

func TestEstimateScalesWithTheTable(t *testing.T) {
	// Round rates, so the arithmetic in the expectations reads, and fast
	// enough per row that the size binds in every case here: the two costs
	// are weighed against each other in the test below.
	throughput := Throughput{
		Rewrite: 10 << 20, IndexBuild: 5 << 20,
		RewriteRows: 1 << 30, IndexRows: 1 << 30,
	}

	cases := []struct {
		name     string
		duration lockmodel.DurationClass
		bytes    int64
		want     time.Duration
		ok       bool
	}{
		{
			name:     "a rewrite is the measured rewrite rate",
			duration: lockmodel.Rewrite, bytes: 100 << 20, want: 10 * time.Second, ok: true,
		},
		{
			name: "an index build is its own rate", duration: lockmodel.IndexBuild,
			bytes: 100 << 20, want: 20 * time.Second, ok: true,
		},
		{
			name: "a scan cannot be slower than a rewrite", duration: lockmodel.Scan,
			bytes: 100 << 20, want: 10 * time.Second, ok: true,
		},
		{name: "catalog work is not estimated", duration: lockmodel.Instant, bytes: 100 << 20},
		{name: "an unknown size is not estimated", duration: lockmodel.Rewrite},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			low, high, ok := throughput.Estimate(tc.duration, 0, tc.bytes)

			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}

			if !ok {
				return
			}

			// The range brackets the point estimate: faster than a busy
			// server, slower than an idle one.
			if low >= tc.want || high <= tc.want {
				t.Errorf("estimate = %s to %s, want a range around %s", low, high, tc.want)
			}
		})
	}
}

// TestEstimateTakesWhicheverCostBinds covers the two halves of the model: a
// table of narrow rows costs more than its size says, because a rewrite pays
// per tuple as well as per byte.
func TestEstimateTakesWhicheverCostBinds(t *testing.T) {
	// One megabyte a second, and one row a second.
	throughput := Throughput{
		Rewrite: 1 << 20, IndexBuild: 1 << 20,
		RewriteRows: 1, IndexRows: 1,
	}

	// Ten megabytes is ten seconds by size; the hundred rows in it are a
	// hundred seconds by row, and the longer of the two is the answer.
	low, high, ok := throughput.Estimate(lockmodel.Rewrite, 100, 10<<20)
	if !ok {
		t.Fatal("no estimate")
	}

	if middle := (low + high) / 2; middle < 60*time.Second {
		t.Errorf("estimate %s ignores the per-row cost", middle)
	}
}

// TestEstimateStaysSilentWithoutAProbe covers the offline and standby paths,
// where no rate was measured and no estimate may be offered.
func TestEstimateStaysSilentWithoutAProbe(t *testing.T) {
	var none Throughput

	if none.Known() {
		t.Error("an unmeasured throughput reported itself known")
	}

	if _, _, ok := none.Estimate(lockmodel.Rewrite, 1_000, 1<<30); ok {
		t.Error("an unmeasured throughput produced an estimate")
	}
}
