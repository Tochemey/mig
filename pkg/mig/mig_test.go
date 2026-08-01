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

package mig_test

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"io/fs"
	"os"
	"testing"
	"time"

	"github.com/tochemey/mig/internal/lease"
	"github.com/tochemey/mig/internal/ledger"
	"github.com/tochemey/mig/test/harness"
)

// seedRows is enough rows for a backfill fixture to take more than one batch.
const seedRows = 5

// embedded is the shape a service ships: migrations compiled into the binary,
// checked at startup, with no directory alongside it.
//
//go:embed testdata/migrations
var embedded embed.FS

// shared is the container for this package, or nil when docker is absent.
var shared *harness.Harness

// TestMain brings up one container for the package.
func TestMain(m *testing.M) {
	os.Exit(harness.Main(m, seedRows, func(_ context.Context, h *harness.Harness) error {
		shared = h
		return nil
	}))
}

// migrations returns the embedded directory rooted where the files are.
func migrations(t *testing.T) fs.FS {
	t.Helper()

	sub, err := fs.Sub(embedded, "testdata/migrations")
	if err != nil {
		t.Fatalf("sub: %v", err)
	}

	return sub
}

// apply runs statements directly, standing in for work done by any means.
func apply(t *testing.T, db *sql.DB, statements ...string) {
	t.Helper()

	for _, statement := range statements {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

// ledgerExists asks whether anything has created the ledger.
func ledgerExists(ctx context.Context, db *sql.DB) (bool, error) {
	var present bool

	err := db.QueryRowContext(ctx, "SELECT to_regclass('mig.steps') IS NOT NULL").Scan(&present)

	return present, err
}

// newDatabase gives the test its own database and a pool onto it.
func newDatabase(t *testing.T) *sql.DB {
	t.Helper()

	_, db := newNamedDatabase(t)

	return db
}

// newNamedDatabase is the same, for a test that needs a second pool onto the
// database it was given.
func newNamedDatabase(t *testing.T) (string, *sql.DB) {
	t.Helper()

	if shared == nil {
		t.Skip("postgres container not available")
	}

	name, err := shared.Clone(t.Context(), harness.Template)
	if err != nil {
		t.Fatalf("clone template: %v", err)
	}

	t.Cleanup(func() {
		if err := shared.DropDatabase(context.Background(), name); err != nil {
			t.Errorf("drop clone %q: %v", name, err)
		}
	})

	return name, openOn(t, name)
}

// openOn connects to a database that already exists.
func openOn(t *testing.T, name string) *sql.DB {
	t.Helper()

	db, err := shared.Open(t.Context(), name)
	if err != nil {
		t.Fatalf("open %q: %v", name, err)
	}

	t.Cleanup(func() {
		// A test that closed the pool itself has already made its point.
		if err := db.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
			t.Errorf("close pool: %v", err)
		}
	})

	return db
}

// holdLease takes the lease so a run has to contend for it, and returns the
// call that hands it back.
func holdLease(t *testing.T, db *sql.DB) func() {
	t.Helper()

	if err := ledger.EnsureSchema(t.Context(), db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	held, err := lease.Acquire(t.Context(), db, lease.Config{
		Owner:    lease.NewOwner(),
		TTL:      time.Minute,
		OnLocked: lease.Fail,
	})
	if err != nil {
		t.Fatalf("hold the lease: %v", err)
	}

	return func() {
		if err := held.Release(context.Background()); err != nil {
			t.Errorf("release the lease: %v", err)
		}
	}
}
