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

// Package faultdb wraps a driver so a chosen statement fails.
//
// The kill matrices cover a process dying between operations. What they cannot
// reach is an operation that fails at the instant it runs — a commit refused, a
// rollback that never lands, a row that will not scan. Those are the paths a
// real outage takes, and they are unreachable without making the driver itself
// misbehave.
//
// A fault matches on a substring of the SQL, so a test names the statement it
// wants to break rather than counting calls.
package faultdb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/stdlib"
)

// names records the driver names already registered.
var names sync.Map

// Op is a driver operation a fault can be attached to.
type Op string

const (
	// OpExec fails a statement that returns no rows.
	OpExec Op = "exec"

	// OpQuery fails a statement that returns rows.
	OpQuery Op = "query"

	// OpBegin fails opening a transaction.
	OpBegin Op = "begin"

	// OpCommit fails a transaction at the one instant that decides whether its
	// work exists.
	OpCommit Op = "commit"

	// OpRollback fails the cleanup after something else already went wrong,
	// which is where a swallowed error hides.
	OpRollback Op = "rollback"
)

// ErrInjected is what a faulted operation returns.
var ErrInjected = errors.New("injected fault")

// Fault is one injected failure.
type Fault struct {
	// Op is the operation to break.
	Op Op

	// Match is a substring of the SQL. An empty Match breaks every call, which
	// is what the operations carrying no SQL need.
	Match string
}

// Faults is a set of injected failures, safe to consult from several
// connections at once.
type Faults struct {
	mu     sync.Mutex
	faults []Fault
	fired  int
}

// NewFaults builds a set.
func NewFaults(faults ...Fault) *Faults {
	return &Faults{faults: faults}
}

// Add attaches a fault after the set is already in use, so a test can let its
// scaffolding run cleanly and break only the call it is aiming at.
func (f *Faults) Add(fault Fault) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.faults = append(f.faults, fault)
}

// Fired reports how many faults have been taken, so a test can tell a path it
// meant to break from one it never reached.
func (f *Faults) Fired() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.fired
}

// check reports the error to return for an operation, or nil to let it through.
func (f *Faults) check(op Op, query string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, fault := range f.faults {
		if fault.Op == op && strings.Contains(query, fault.Match) {
			f.fired++

			return fmt.Errorf("%w: %s", ErrInjected, op)
		}
	}

	return nil
}

// Open registers a fault-injecting driver and connects through it.
//
// The name has to be unique across the process, because database/sql keeps its
// drivers in a global registry with no way to replace or remove one. Reusing a
// name is refused rather than allowed to panic inside database/sql.
func Open(name, dsn string, faults *Faults) (*sql.DB, error) {
	if _, taken := names.LoadOrStore(name, struct{}{}); taken {
		return nil, fmt.Errorf("driver %q is already registered: give each pool its own name", name)
	}

	sql.Register(name, &faultDriver{faults: faults})

	db, err := sql.Open(name, dsn)
	if err != nil {
		return nil, fmt.Errorf("open faulted database: %w", err)
	}

	return db, nil
}

// faultDriver wraps pgx.
type faultDriver struct {
	faults *Faults
}

// Open connects through pgx and wraps the connection.
func (d *faultDriver) Open(dsn string) (driver.Conn, error) {
	conn, err := stdlib.GetDefaultDriver().Open(dsn)
	if err != nil {
		return nil, err
	}

	return &faultConn{conn: conn, faults: d.faults}, nil
}

// faultConn applies the faults to one connection.
type faultConn struct {
	conn   driver.Conn
	faults *Faults
}

// Prepare is required by the interface. Every path used here goes through the
// context-aware methods instead.
func (c *faultConn) Prepare(query string) (driver.Stmt, error) {
	return c.conn.Prepare(query)
}

// Close releases the connection.
func (c *faultConn) Close() error {
	return c.conn.Close()
}

// Begin is required by the interface; BeginTx is what database/sql calls.
func (c *faultConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

// BeginTx opens a transaction unless a fault claims it.
func (c *faultConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if err := c.faults.check(OpBegin, ""); err != nil {
		return nil, err
	}

	tx, err := c.conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}

	return &faultTx{tx: tx, faults: c.faults}, nil
}

// ExecContext runs a statement unless a fault claims it.
func (c *faultConn) ExecContext(ctx context.Context, query string,
	args []driver.NamedValue) (driver.Result, error) {
	if err := c.faults.check(OpExec, query); err != nil {
		return nil, err
	}

	return c.conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

// QueryContext runs a query unless a fault claims it.
func (c *faultConn) QueryContext(ctx context.Context, query string,
	args []driver.NamedValue) (driver.Rows, error) {
	if err := c.faults.check(OpQuery, query); err != nil {
		return nil, err
	}

	return c.conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

// IsValid lets the pool discard a connection a fault has broken, which the
// wrapper would otherwise hide from it.
func (c *faultConn) IsValid() bool {
	valid, ok := c.conn.(driver.Validator)

	return !ok || valid.IsValid()
}

// faultTx applies the faults to a transaction's two endings.
type faultTx struct {
	tx     driver.Tx
	faults *Faults
}

// Commit ends the transaction, reporting a fault if one claims it.
//
// A faulted commit rolls back instead, which is what a commit that fails really
// does: the transaction ends and its work does not exist. Skipping both would
// leave it open holding every lock it took — including the lease row — and the
// next writer would wait on it forever.
func (t *faultTx) Commit() error {
	if err := t.faults.check(OpCommit, ""); err != nil {
		return errors.Join(err, t.tx.Rollback())
	}

	return t.tx.Commit()
}

// Rollback abandons the transaction, reporting a fault if one claims it. The
// real rollback happens either way, for the same reason.
func (t *faultTx) Rollback() error {
	if err := t.faults.check(OpRollback, ""); err != nil {
		return errors.Join(err, t.tx.Rollback())
	}

	return t.tx.Rollback()
}
