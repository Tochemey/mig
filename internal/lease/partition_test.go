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
	"github.com/tochemey/mig/test/harness"
)

// partitionTTL is short enough to keep the test quick and long enough that the
// heartbeat gets several attempts inside it.
const partitionTTL = 3 * time.Second

// A partition is the one failure the recovery tests cannot produce. Killing a
// process, or terminating its backend, hands the runner an error immediately.
// A lost route hands it nothing: every socket stays open and every read waits
// on an answer that is not coming.
//
// The holder has to give up on its own, and it has to do so while its lease is
// still valid. A runner that keeps working past its expiry is running beside
// whoever took over from it.

// TestPartitionEndsTheWorkBeforeTheLeaseExpires is L4.
func TestPartitionEndsTheWorkBeforeTheLeaseExpires(t *testing.T) {
	proxied, database := newProxiedLedger(t)

	held, err := lease.Acquire(t.Context(), proxied.db, lease.Config{
		Owner:    lease.NewOwner(),
		TTL:      partitionTTL,
		OnLocked: lease.Fail,
	})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	working, stop := held.Keepalive(t.Context())
	defer stop()

	// Everything after this point sees a link that has gone quiet.
	cutAt := time.Now()

	proxied.proxy.Cut()

	select {
	case <-working.Done():
	case <-time.After(2 * partitionTTL):
		t.Fatal("the holder was still working after twice its lease had elapsed")
	}

	// The whole point of the cell: the work stops while the lease is still
	// valid, so nothing overlaps with the runner that takes over next.
	if elapsed := time.Since(cutAt); elapsed >= partitionTTL {
		t.Fatalf("work stopped after %s, which is past the %s lease", elapsed, partitionTTL)
	}

	if cause := context.Cause(working); !errors.Is(cause, lease.ErrLost) {
		t.Fatalf("work ended with %v, want ErrLost", cause)
	}

	// The lease row is left alone: the holder could not reach it to release it,
	// and it must not pretend otherwise. A successor waits for the expiry.
	assertStillHeld(t, database, held.Fence().Owner)
}

// TestPartitionedReleaseIsReported covers handing the lease back over a link
// that is already gone. It cannot succeed, and saying that it did would leave
// the caller believing the lease was free.
func TestPartitionedReleaseIsReported(t *testing.T) {
	proxied, _ := newProxiedLedger(t)

	held, err := lease.Acquire(t.Context(), proxied.db, lease.Config{
		Owner:    lease.NewOwner(),
		TTL:      partitionTTL,
		OnLocked: lease.Fail,
	})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	proxied.proxy.Cut()

	ctx, cancel := context.WithTimeout(context.Background(), partitionTTL)
	defer cancel()

	if err := held.Release(ctx); err == nil {
		t.Fatal("release reported success over a partitioned link")
	}
}

// proxied is a pool whose traffic runs through a proxy the test can cut.
type proxied struct {
	db    *sql.DB
	proxy *harness.Proxy
}

// newProxiedLedger gives the test its own database, with the ledger in place,
// reached through a proxy.
//
// The schema is created over a direct connection, so that only the traffic the
// test cares about crosses the proxy.
func newProxiedLedger(t *testing.T) (proxied, string) {
	t.Helper()

	if shared == nil {
		t.Skip("postgres container not available")
	}

	database := newNamedDatabase(t)

	if err := ledger.EnsureSchema(t.Context(), openOn(t, database)); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	proxy, err := harness.NewProxy(shared.Endpoint())
	if err != nil {
		t.Fatalf("start proxy: %v", err)
	}

	t.Cleanup(func() {
		if err := proxy.Close(); err != nil {
			t.Errorf("close proxy: %v", err)
		}
	})

	dsn, err := proxy.DSN(shared.DSN(database))
	if err != nil {
		t.Fatalf("rewrite dsn: %v", err)
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open through the proxy: %v", err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close proxied pool: %v", err)
		}
	})

	return proxied{db: db, proxy: proxy}, database
}

// assertStillHeld checks the lease row over a direct connection.
func assertStillHeld(t *testing.T, database, owner string) {
	t.Helper()

	var recorded string

	err := openOn(t, database).QueryRowContext(t.Context(),
		`SELECT coalesce(owner, '') FROM mig.lease WHERE id = 1`).Scan(&recorded)
	if err != nil {
		t.Fatalf("read the lease row: %v", err)
	}

	if recorded != owner {
		t.Fatalf("the lease row says %q, want the partitioned holder %q", recorded, owner)
	}
}
