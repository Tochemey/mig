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
	"testing"
	"time"

	"github.com/tochemey/mig/internal/lease"
	"github.com/tochemey/mig/internal/ledger"
)

// TestAcquireAppliesDefaults covers the zero configuration, which is what an
// embedding caller gets when it fills in nothing but an owner.
func TestAcquireAppliesDefaults(t *testing.T) {
	db := newLedger(t)

	held, err := lease.Acquire(t.Context(), db, lease.Config{Owner: lease.NewOwner()})
	if err != nil {
		t.Fatalf("acquire with defaults: %v", err)
	}

	if got := held.TTL(); got != lease.DefaultTTL {
		t.Fatalf("TTL defaulted to %s, want %s", got, lease.DefaultTTL)
	}

	// Waiting is the default, so a second acquisition blocks and cancelling is
	// the only way out. Where the cancellation lands, in the idle wait or in the
	// query, is not something the test pins down.
	ctx, cancel := context.WithCancel(t.Context())

	const waited = 500 * time.Millisecond

	time.AfterFunc(waited, cancel)

	start := time.Now()

	_, err = lease.Acquire(ctx, db, lease.Config{Owner: lease.NewOwner()})

	if err == nil || errors.Is(err, lease.ErrLocked) {
		t.Fatalf("second acquire returned %v, want the wait to be cut short by cancellation", err)
	}

	if elapsed := time.Since(start); elapsed < waited {
		t.Fatalf("second acquire gave up after %s without waiting", elapsed)
	}
}

// TestAcquireRequiresAnOwner covers the configuration mistake that leaves the
// ledger unable to say who is holding a migration up.
func TestAcquireRequiresAnOwner(t *testing.T) {
	db := newLedger(t)

	if _, err := lease.Acquire(t.Context(), db, lease.Config{}); !errors.Is(err, lease.ErrNoOwner) {
		t.Fatalf("acquire without an owner returned %v, want ErrNoOwner", err)
	}
}

// TestAcquireGivesUpAtWaitTimeout covers --on-locked=wait reaching its bound.
func TestAcquireGivesUpAtWaitTimeout(t *testing.T) {
	db := newLedger(t)
	ctx := t.Context()

	if _, err := lease.Acquire(ctx, db, config(t, lease.Fail)); err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	waiter := config(t, lease.Wait)
	waiter.WaitTimeout = 300 * time.Millisecond

	_, err := lease.Acquire(ctx, db, waiter)
	if !errors.Is(err, lease.ErrLocked) {
		t.Fatalf("waiting acquire returned %v, want ErrLocked", err)
	}
}

// TestAcquireReportsQueryFailure checks that a broken database is not reported
// as a lease somebody else holds; the two call for opposite responses.
func TestAcquireReportsQueryFailure(t *testing.T) {
	db := newLedger(t)

	if err := db.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}

	_, err := lease.Acquire(t.Context(), db, config(t, lease.Fail))

	if err == nil || errors.Is(err, lease.ErrLocked) {
		t.Fatalf("acquire on a closed database returned %v, want a query error", err)
	}
}

// TestReleaseReportsQueryFailure covers handing the lease back over a database
// that has gone away.
func TestReleaseReportsQueryFailure(t *testing.T) {
	db := newLedger(t)

	held, err := lease.Acquire(t.Context(), db, config(t, lease.Fail))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}

	if err := held.Release(t.Context()); err == nil {
		t.Fatal("release on a closed database returned no error")
	}
}

// TestKeepaliveGivesUpAfterRepeatedFailures checks that a holder which cannot
// reach the database stops working before its lease expires rather than after.
func TestKeepaliveGivesUpAfterRepeatedFailures(t *testing.T) {
	db := newLedger(t)

	cfg := config(t, lease.Fail)
	cfg.TTL = 300 * time.Millisecond

	held, err := lease.Acquire(t.Context(), db, cfg)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	work, stop := held.Keepalive(t.Context())
	defer stop()

	// The database becomes unreachable, as a failover or a severed connection
	// would make it.
	if err := db.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}

	select {
	case <-work.Done():
		if cause := context.Cause(work); !errors.Is(cause, lease.ErrLost) {
			t.Fatalf("work context cancelled with %v, want ErrLost", cause)
		}

	case <-time.After(10 * time.Second):
		t.Fatal("keepalive kept working with an unreachable database")
	}
}

// TestKeepaliveOutlivesALongLedgerWrite is why ownership and expiry are
// separate rows.
//
// A backfill commits its cursor inside the transaction that writes its batch,
// so that transaction holds the lock on the ownership row for as long as the
// batch takes. If the heartbeat wrote that same row it would queue behind the
// batch, and the holder would give up a lease nobody else wanted — under
// exactly the load the lease exists to survive.
func TestKeepaliveOutlivesALongLedgerWrite(t *testing.T) {
	db := newLedger(t)
	ctx := t.Context()

	cfg := config(t, lease.Fail)
	cfg.TTL = 600 * time.Millisecond

	held, err := lease.Acquire(ctx, db, cfg)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	work, stop := held.Keepalive(ctx)
	defer stop()

	// A fenced write that stays open far longer than the whole TTL, as a batch
	// over a large table does.
	const hold = 3 * time.Second

	writeDone := make(chan error, 1)

	go func() {
		writeDone <- ledger.Write(ctx, db, held.Fence(), func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "SELECT pg_sleep($1)", hold.Seconds())
			return err
		})
	}()

	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("long fenced write: %v", err)
		}

	case <-work.Done():
		t.Fatalf("the lease was given up while a batch was in flight: %v", context.Cause(work))

	case <-time.After(hold + 10*time.Second):
		t.Fatal("the long write never finished")
	}

	if err := work.Err(); err != nil {
		t.Fatalf("lease reported lost after the write: %v (cause %v)", err, context.Cause(work))
	}
}

// TestKeepaliveStopsWithItsParent covers the ordinary shutdown path, where
// cancelling the run also winds up the heartbeat.
func TestKeepaliveStopsWithItsParent(t *testing.T) {
	db := newLedger(t)

	cfg := config(t, lease.Fail)
	cfg.TTL = 300 * time.Millisecond

	held, err := lease.Acquire(t.Context(), db, cfg)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())

	work, stop := held.Keepalive(ctx)

	cancel()

	select {
	case <-work.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("keepalive outlived its parent context")
	}

	// stop must remain safe to call after the loop has already exited.
	stop()
}
