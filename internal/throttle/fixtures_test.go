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

package throttle_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
)

// The lag signal is mocked here. What the throttle does with a number is the
// logic worth pinning down. The SQL adapter that reads pg_stat_replication is
// covered with a stub driver.

// fixedLag reports a constant lag.
type fixedLag int64

// Lag reports the constant.
func (l fixedLag) Lag(context.Context) (int64, error) {
	return int64(l), nil
}

// failingLag stands in for a lag query that cannot run.
type failingLag struct{}

// Lag always fails.
func (failingLag) Lag(context.Context) (int64, error) {
	return 0, errors.New("no replication view")
}

// stubSeq keeps driver names unique across tests. database/sql registers
// drivers process-wide and refuses a second Open under the same name.
var stubSeq atomic.Uint64

// stubDriver answers the replication lag query with a fixed result.
type stubDriver struct {
	rows []driver.Value
	err  error
}

// Open returns a connection that serves the stubbed result.
func (d *stubDriver) Open(string) (driver.Conn, error) {
	return &stubConn{rows: d.rows, err: d.err}, nil
}

// stubConn is a single-query connection.
type stubConn struct {
	rows []driver.Value
	err  error
}

// Prepare is unused; QueryContext handles the lag read directly.
func (*stubConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported")
}

// Close is a no-op.
func (*stubConn) Close() error { return nil }

// Begin is unused.
func (*stubConn) Begin() (driver.Tx, error) {
	return nil, errors.New("begin not supported")
}

// QueryContext returns the stubbed lag row, or the stubbed error.
func (c *stubConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	if c.err != nil {
		return nil, c.err
	}

	return &stubRows{values: c.rows}, nil
}

// stubRows yields at most one row of values.
type stubRows struct {
	values []driver.Value
	done   bool
}

// Columns names the single lag column.
func (*stubRows) Columns() []string { return []string{"lag"} }

// Close is a no-op.
func (*stubRows) Close() error { return nil }

// Next advances to the stubbed row once, then ends.
func (r *stubRows) Next(dest []driver.Value) error {
	if r.done || r.values == nil {
		return io.EOF
	}

	r.done = true
	copy(dest, r.values)

	return nil
}

// openStub opens a pool backed by the stub driver.
func openStub(t *testing.T, rows []driver.Value, queryErr error) *sql.DB {
	t.Helper()

	name := fmt.Sprintf("throttle_stub_%d", stubSeq.Add(1))
	sql.Register(name, &stubDriver{rows: rows, err: queryErr})

	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("open stub: %v", err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close stub: %v", err)
		}
	})

	return db
}
