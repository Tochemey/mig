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

package lease_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/tochemey/mig/internal/lease"
	"github.com/tochemey/mig/internal/ledger"
	"github.com/tochemey/mig/test/harness"
)

// shared is the container for this package, or nil when docker is absent.
var shared *harness.Harness

// TestMain brings up one container for the package.
func TestMain(m *testing.M) {
	os.Exit(harness.Main(m, 0, func(_ context.Context, h *harness.Harness) error {
		shared = h
		return nil
	}))
}

// TestAcquireIncrementsFence checks that tokens are monotonic, which is what
// makes a stale holder recognisable.
func TestAcquireIncrementsFence(t *testing.T) {
	db := newLedger(t)
	ctx := t.Context()

	for want := int64(1); want <= 3; want++ {
		held, err := lease.Acquire(ctx, db, config(t, lease.Fail))
		if err != nil {
			t.Fatalf("acquire %d: %v", want, err)
		}

		if got := held.Fence().Token; got != want {
			t.Fatalf("fence is %d, want %d", got, want)
		}

		if err := held.Release(ctx); err != nil {
			t.Fatalf("release %d: %v", want, err)
		}
	}
}

// TestAcquireFailsWhenHeld covers --on-locked=fail.
func TestAcquireFailsWhenHeld(t *testing.T) {
	db := newLedger(t)
	ctx := t.Context()

	first, err := lease.Acquire(ctx, db, config(t, lease.Fail))
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	t.Cleanup(func() {
		if err := first.Release(context.Background()); err != nil && !errors.Is(err, ledger.ErrFenced) {
			t.Errorf("release the held lease: %v", err)
		}
	})

	if _, err := lease.Acquire(ctx, db, config(t, lease.Fail)); !errors.Is(err, lease.ErrLocked) {
		t.Fatalf("second acquire returned %v, want ErrLocked", err)
	}
}

// TestAcquireWaitsForRelease covers --on-locked=wait: the second runner blocks
// rather than proceeding alongside the first.
func TestAcquireWaitsForRelease(t *testing.T) {
	db := newLedger(t)
	ctx := t.Context()

	first, err := lease.Acquire(ctx, db, config(t, lease.Fail))
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	acquired := make(chan *lease.Lease, 1)
	failed := make(chan error, 1)

	go func() {
		second, err := lease.Acquire(ctx, db, config(t, lease.Wait))
		if err != nil {
			failed <- err
			return
		}

		acquired <- second
	}()

	// The waiter must still be waiting while the lease is held.
	select {
	case second := <-acquired:
		t.Fatalf("second runner acquired fence %d while the lease was held", second.Fence().Token)
	case err := <-failed:
		t.Fatalf("second acquire: %v", err)
	case <-time.After(500 * time.Millisecond):
	}

	if err := first.Release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}

	select {
	case second := <-acquired:
		if got := second.Fence().Token; got != 2 {
			t.Fatalf("second runner has fence %d, want 2", got)
		}

	case err := <-failed:
		t.Fatalf("second acquire after release: %v", err)

	case <-time.After(10 * time.Second):
		t.Fatal("second runner never acquired the released lease")
	}
}

// TestAcquireStealsExpiredLease checks that a runner which died without
// releasing does not lock everyone else out.
func TestAcquireStealsExpiredLease(t *testing.T) {
	db := newLedger(t)
	ctx := t.Context()

	cfg := config(t, lease.Fail)
	cfg.TTL = 300 * time.Millisecond

	dead, err := lease.Acquire(ctx, db, cfg)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	// No keepalive: this stands in for a process that was killed.
	waitForExpiry(t, db)

	successor, err := lease.Acquire(ctx, db, config(t, lease.Fail))
	if err != nil {
		t.Fatalf("steal expired lease: %v", err)
	}

	if got := successor.Fence().Token; got != 2 {
		t.Fatalf("successor has fence %d, want 2", got)
	}

	// And the dead runner is now fenced out of the ledger.
	err = ledger.Write(ctx, db, dead.Fence(), func(ctx context.Context, tx *sql.Tx) error {
		return ledger.UpsertMigration(ctx, tx, "clobbered", "clobbered")
	})

	if !errors.Is(err, ledger.ErrFenced) {
		t.Fatalf("expired runner's write returned %v, want ErrFenced", err)
	}
}

// TestReleaseIsFenced checks that a superseded runner cannot hand away a lease
// its successor is using.
func TestReleaseIsFenced(t *testing.T) {
	db := newLedger(t)
	ctx := t.Context()

	cfg := config(t, lease.Fail)
	cfg.TTL = 300 * time.Millisecond

	stale, err := lease.Acquire(ctx, db, cfg)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	waitForExpiry(t, db)

	successor, err := lease.Acquire(ctx, db, config(t, lease.Fail))
	if err != nil {
		t.Fatalf("successor acquire: %v", err)
	}

	if err := stale.Release(ctx); !errors.Is(err, ledger.ErrFenced) {
		t.Fatalf("stale release returned %v, want ErrFenced", err)
	}

	// The successor still holds it.
	if err := successor.Release(ctx); err != nil {
		t.Fatalf("successor release: %v", err)
	}
}

// TestKeepaliveExtendsLease checks that a lease under renewal outlives several
// TTLs and cannot be stolen.
func TestKeepaliveExtendsLease(t *testing.T) {
	db := newLedger(t)
	ctx := t.Context()

	cfg := config(t, lease.Fail)
	cfg.TTL = 300 * time.Millisecond

	held, err := lease.Acquire(ctx, db, cfg)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	work, stop := held.Keepalive(ctx)
	defer stop()

	time.Sleep(3 * cfg.TTL)

	if err := work.Err(); err != nil {
		t.Fatalf("renewed lease was reported lost: %v (cause %v)", err, context.Cause(work))
	}

	if _, err := lease.Acquire(ctx, db, config(t, lease.Fail)); !errors.Is(err, lease.ErrLocked) {
		t.Fatalf("a renewed lease was stolen: %v", err)
	}
}

// TestKeepaliveCancelsWorkWhenStolen checks that once the lease is taken, the
// previous holder's work context is cancelled.
func TestKeepaliveCancelsWorkWhenStolen(t *testing.T) {
	db := newLedger(t)
	ctx := t.Context()

	cfg := config(t, lease.Fail)
	cfg.TTL = 300 * time.Millisecond

	held, err := lease.Acquire(ctx, db, cfg)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	work, stop := held.Keepalive(ctx)
	defer stop()

	// Steal it out from under the keepalive, as an expiry takeover would.
	stealLease(t, db)

	select {
	case <-work.Done():
		if cause := context.Cause(work); !errors.Is(cause, lease.ErrLost) {
			t.Fatalf("work context cancelled with %v, want ErrLost", cause)
		}

	case <-time.After(5 * time.Second):
		t.Fatal("keepalive did not notice the lease was stolen")
	}
}

// TestNewOwnerIsUnique checks that two runners cannot look like the same one,
// which would let each pass the other's fence check.
func TestNewOwnerIsUnique(t *testing.T) {
	seen := make(map[string]bool)

	for range 100 {
		owner := lease.NewOwner()

		if seen[owner] {
			t.Fatalf("owner %q generated twice", owner)
		}

		seen[owner] = true
	}
}

// TestConcurrentAcquireAdmitsOne checks that when many runners start together,
// exactly one gets in.
func TestConcurrentAcquireAdmitsOne(t *testing.T) {
	db := newLedger(t)
	ctx := t.Context()

	const runners = 8

	var (
		mu       sync.Mutex
		admitted []*lease.Lease
		locked   int
	)

	var wg sync.WaitGroup

	for range runners {
		wg.Add(1)

		go func() {
			defer wg.Done()

			held, err := lease.Acquire(ctx, db, config(t, lease.Fail))

			mu.Lock()
			defer mu.Unlock()

			switch {
			case err == nil:
				admitted = append(admitted, held)
			case errors.Is(err, lease.ErrLocked):
				locked++
			default:
				t.Errorf("acquire: %v", err)
			}
		}()
	}

	wg.Wait()

	if len(admitted) != 1 {
		t.Fatalf("%d runners acquired the lease, want exactly 1", len(admitted))
	}

	if locked != runners-1 {
		t.Fatalf("%d runners were locked out, want %d", locked, runners-1)
	}

	// One acquisition means exactly one fence increment.
	if got := admitted[0].Fence().Token; got != 1 {
		t.Fatalf("fence advanced to %d after one acquisition", got)
	}
}

// config builds a lease configuration for a distinct runner.
func config(t *testing.T, onLocked lease.OnLocked) lease.Config {
	t.Helper()

	return lease.Config{
		Owner:       lease.NewOwner(),
		TTL:         5 * time.Second,
		OnLocked:    onLocked,
		WaitTimeout: 10 * time.Second,
	}
}

// waitForExpiry blocks until the server considers the lease lapsed, which is
// what acquisition judges by.
func waitForExpiry(t *testing.T, db *sql.DB) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for {
		var expired bool

		const query = `SELECT coalesce(expires_at < now(), true) FROM mig.lease_expiry WHERE id = 1`

		if err := db.QueryRowContext(t.Context(), query).Scan(&expired); err != nil {
			t.Fatalf("check lease expiry: %v", err)
		}

		if expired {
			return
		}

		if !time.Now().Before(deadline) {
			t.Fatal("lease never expired")
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// stealLease takes the lease directly, as a successor acquisition would.
func stealLease(t *testing.T, db *sql.DB) {
	t.Helper()

	statements := []string{
		`UPDATE mig.lease SET owner = 'thief', fence = fence + 1 WHERE id = 1`,
		`UPDATE mig.lease_expiry SET expires_at = now() + interval '1 hour', heartbeat_at = now() WHERE id = 1`,
	}

	for _, stmt := range statements {
		if _, err := db.ExecContext(t.Context(), stmt); err != nil {
			t.Fatalf("steal lease: %v", err)
		}
	}
}

// newLedger gives the test its own database with the ledger schema in place.
func newLedger(t *testing.T) *sql.DB {
	t.Helper()

	db := openOn(t, newNamedDatabase(t))

	if err := ledger.EnsureSchema(t.Context(), db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	return db
}

// newNamedDatabase gives the test its own database, for a case that needs to
// open more than one pool onto it.
func newNamedDatabase(t *testing.T) string {
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

	return name
}

// openOn connects to a database that already exists.
func openOn(t *testing.T, name string) *sql.DB {
	t.Helper()

	db, err := shared.Open(t.Context(), name)
	if err != nil {
		t.Fatalf("open %q: %v", name, err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close pool: %v", err)
		}
	})

	return db
}
