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
	"fmt"
	"time"

	"github.com/tochemey/mig/internal/lint/lockmodel"
)

// The bounds an estimate is reported between. Rewriting a table nobody is
// writing to, with its pages already in memory, is the fast end; production
// traffic and cold pages are the slow one.
const (
	estimateFast = 0.75
	estimateSlow = 2.0
)

// probeRows is the calibration table's size: large enough that the timings
// are not measuring the round trip, small enough that a linter run does not
// become a load test.
const probeRows = 200_000

// The calibration statements. The table is an ordinary one, created inside a
// transaction that is always rolled back: a temporary table would be measured
// unlogged and entirely in cache, and the acceptance run showed that reading
// three times faster than the rewrite it was meant to predict. Nothing
// survives the rollback, and a server that will not take the write says so
// instead of being estimated wrongly.
const (
	probeBegin  = `BEGIN`
	probeCreate = `CREATE TABLE mig_lint_calibration (id bigint, pad text)`
	probeFill   = `INSERT INTO mig_lint_calibration
	               SELECT g, repeat('x', 64) FROM generate_series(1, $1) g`
	probeSize     = `SELECT pg_total_relation_size('mig_lint_calibration')`
	probeRewrite  = `ALTER TABLE mig_lint_calibration ADD COLUMN probe float8 DEFAULT random()`
	probeIndex    = `CREATE INDEX mig_lint_calibration_id ON mig_lint_calibration (id)`
	probeRollback = `ROLLBACK`
)

// Throughput is what one server was measured doing.
//
// Each kind of work is measured twice, per byte and per row, because a
// rewrite pays for both: forming a tuple and evaluating a default cost per
// row, writing the result costs per byte. Which one binds depends on how wide
// the rows are, and a table of narrow rows takes far longer than its size
// alone suggests.
//
// It is not a constant of the hardware either way: it is one sample, and a
// rewrite of a cold 300 GB table under production traffic will be slower. An
// estimate carrying its method and a range is worth more than no estimate,
// and worth less than a rehearsal.
type Throughput struct {
	// Bytes per second.
	Rewrite    float64
	IndexBuild float64

	// Rows per second.
	RewriteRows float64
	IndexRows   float64
}

// Known reports whether a probe ran.
func (t Throughput) Known() bool {
	return t.Rewrite > 0 && t.IndexBuild > 0 && t.RewriteRows > 0 && t.IndexRows > 0
}

// Estimate reports how long the work on a table of this size is likely to
// take, as a range. It reports false for the classes whose cost is not the
// table's size, which is every class but these two.
//
// A table with no row estimate, which is one nobody has analysed, is judged
// on its size alone.
func (t Throughput) Estimate(duration lockmodel.DurationClass, rows, bytes int64) (low, high time.Duration, ok bool) {
	if !t.Known() || bytes <= 0 {
		return 0, 0, false
	}

	var byteRate, rowRate float64

	switch duration {
	case lockmodel.Rewrite:
		byteRate, rowRate = t.Rewrite, t.RewriteRows
	case lockmodel.IndexBuild:
		byteRate, rowRate = t.IndexBuild, t.IndexRows
	case lockmodel.Scan:
		// A validation scan reads what a rewrite reads and writes nothing,
		// so the rewrite's rates are a floor on how fast it can go.
		byteRate, rowRate = t.Rewrite, t.RewriteRows
	default:
		// Catalog work, and anything the model learns to say later: no number
		// is owed for work whose cost is not the size of the table.
		return 0, 0, false
	}

	// Whichever of the two costs binds is the one that decides the wait.
	seconds := float64(bytes) / byteRate
	if perRow := float64(rows) / rowRate; perRow > seconds {
		seconds = perRow
	}

	return time.Duration(seconds * estimateFast * float64(time.Second)),
		time.Duration(seconds * estimateSlow * float64(time.Second)), true
}

// Session is the pinned connection a calibration runs on. The probe is one
// transaction, so every statement has to reach the same backend, which is
// what an *sql.Conn guarantees and a pool does not.
type Session interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Calibrate measures this server by rewriting and indexing a table of its own
// making, inside a transaction it rolls back.
//
// It is ordered rewrite first: an index built beforehand would be rebuilt by
// the rewrite, and the rewrite would be timed doing two jobs.
func Calibrate(ctx context.Context, conn Session) (throughput Throughput, err error) {
	if _, err := conn.ExecContext(ctx, probeBegin); err != nil {
		return Throughput{}, fmt.Errorf("begin calibration: %w", err)
	}

	defer func() {
		if _, backErr := conn.ExecContext(ctx, probeRollback); backErr != nil && err == nil {
			err = fmt.Errorf("roll back calibration: %w", backErr)
			throughput = Throughput{}
		}
	}()

	if _, err := conn.ExecContext(ctx, probeCreate); err != nil {
		return Throughput{}, fmt.Errorf("create calibration table: %w", err)
	}

	if _, err := conn.ExecContext(ctx, probeFill, probeRows); err != nil {
		return Throughput{}, fmt.Errorf("fill calibration table: %w", err)
	}

	var bytes int64

	if err := conn.QueryRowContext(ctx, probeSize).Scan(&bytes); err != nil {
		return Throughput{}, fmt.Errorf("size calibration table: %w", err)
	}

	rewrite, err := timed(ctx, conn, probeRewrite)
	if err != nil {
		return Throughput{}, fmt.Errorf("time a rewrite: %w", err)
	}

	build, err := timed(ctx, conn, probeIndex)
	if err != nil {
		return Throughput{}, fmt.Errorf("time an index build: %w", err)
	}

	return Throughput{
		Rewrite:     rate(bytes, rewrite),
		IndexBuild:  rate(bytes, build),
		RewriteRows: rate(probeRows, rewrite),
		IndexRows:   rate(probeRows, build),
	}, nil
}

// timed runs a statement and reports how long the server took over it.
func timed(ctx context.Context, conn Session, stmt string) (time.Duration, error) {
	started := time.Now()

	if _, err := conn.ExecContext(ctx, stmt); err != nil {
		return 0, err
	}

	return time.Since(started), nil
}

// rate turns one timing into bytes per second. A timing too short to mean
// anything is reported as no measurement rather than as a very fast server.
func rate(bytes int64, elapsed time.Duration) float64 {
	if elapsed < time.Millisecond {
		return 0
	}

	return float64(bytes) / elapsed.Seconds()
}
