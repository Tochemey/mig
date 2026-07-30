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
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/tochemey/mig/internal/lease"
	"github.com/tochemey/mig/internal/ledger"
	"github.com/tochemey/mig/test/faultdb"
)

// A lease that reports success without being taken is the worst outcome there
// is: two runners then believe they are alone. Every failure below has to be
// reported rather than treated as contention or as success.

// TestAcquireReportsEveryFailedStatement covers the three statements the claim
// is made of, and both ways its transaction can end.
func TestAcquireReportsEveryFailedStatement(t *testing.T) {
	cases := map[string]faultdb.Fault{
		"reading the lease row": {Op: faultdb.OpQuery, Match: "expires_at"},
		"taking the lease":      {Op: faultdb.OpQuery, Match: "UPDATE mig.lease"},
		"starting the expiry":   {Op: faultdb.OpExec, Match: "mig.lease_expiry"},
		"opening a transaction": {Op: faultdb.OpBegin},
		"committing the claim":  {Op: faultdb.OpCommit},
	}

	for name, fault := range cases {
		t.Run(name, func(t *testing.T) {
			database := newNamedDatabase(t)
			schema(t, openOn(t, database))

			faults := faultdb.NewFaults(fault)

			_, err := lease.Acquire(t.Context(), openFaulted(t, database, faults), lease.Config{
				Owner:    lease.NewOwner(),
				TTL:      time.Minute,
				OnLocked: lease.Fail,
			})

			if err == nil {
				t.Fatalf("the lease was reported as taken with %q broken", name)
			}

			if faults.Fired() == 0 {
				t.Fatalf("no fault was taken, so %q proves nothing", name)
			}
		})
	}
}

// TestAcquireReportsAFailedRollback covers the cleanup after a claim that
// already went wrong. Its own failure must be reported too, rather than
// replacing the reason the claim failed in the first place.
func TestAcquireReportsAFailedRollback(t *testing.T) {
	database := newNamedDatabase(t)
	schema(t, openOn(t, database))

	faults := faultdb.NewFaults(
		faultdb.Fault{Op: faultdb.OpQuery, Match: "UPDATE mig.lease"},
		faultdb.Fault{Op: faultdb.OpRollback},
	)

	_, err := lease.Acquire(t.Context(), openFaulted(t, database, faults), lease.Config{
		Owner:    lease.NewOwner(),
		TTL:      time.Minute,
		OnLocked: lease.Fail,
	})

	if err == nil {
		t.Fatal("the lease was reported as taken")
	}

	if faults.Fired() < 2 {
		t.Fatalf("%d faults were taken, want both the claim and the rollback", faults.Fired())
	}
}

// TestReleaseReportsEveryFailedStatement covers handing the lease back, which
// runs after a migration has already finished and must still be reported.
func TestReleaseReportsEveryFailedStatement(t *testing.T) {
	cases := map[string]faultdb.Fault{
		"clearing the expiry":   {Op: faultdb.OpExec, Match: "mig.lease_expiry"},
		"opening a transaction": {Op: faultdb.OpBegin},
		"committing":            {Op: faultdb.OpCommit},
	}

	for name, fault := range cases {
		t.Run(name, func(t *testing.T) {
			database := newNamedDatabase(t)
			schema(t, openOn(t, database))

			faults := faultdb.NewFaults()
			db := openFaulted(t, database, faults)

			held, err := lease.Acquire(t.Context(), db, lease.Config{
				Owner:    lease.NewOwner(),
				TTL:      time.Minute,
				OnLocked: lease.Fail,
			})
			if err != nil {
				t.Fatalf("acquire: %v", err)
			}

			// Injected only now, so the claim above is made cleanly and it is
			// the release that breaks.
			faults.Add(fault)

			if err := held.Release(t.Context()); err == nil {
				t.Fatalf("the release reported success with %q broken", name)
			}

			if faults.Fired() == 0 {
				t.Fatalf("no fault was taken, so %q proves nothing", name)
			}
		})
	}
}

// schema puts the ledger in place, which is what the lease rows live in.
func schema(t *testing.T, db *sql.DB) {
	t.Helper()

	if err := ledger.EnsureSchema(t.Context(), db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
}

// openFaulted connects through the fault-injecting driver.
//
// Every pool gets its own driver name, because database/sql keeps its drivers
// in a process-wide registry that cannot be replaced.
func openFaulted(t *testing.T, database string, faults *faultdb.Faults) *sql.DB {
	t.Helper()

	db, err := faultdb.Open(fmt.Sprintf("faulted_%s_%s", t.Name(), database),
		shared.DSN(database), faults)
	if err != nil {
		t.Fatalf("open %q with faults: %v", database, err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close pool: %v", err)
		}
	})

	return db
}

// TestAcquireRereadsTheExpiryAfterWaiting is the race the split into two rows
// opened up, driven deterministically rather than waited for.
//
// A runner that blocks on the lease row re-reads, when the lock clears, only
// the rows it locked. With the expiry row left unlocked it would pair the new
// owner with the expiry as it stood before the winner wrote it — a NULL, read
// as "no expiry, the lease has lapsed" — and take a lease already held.
func TestAcquireRereadsTheExpiryAfterWaiting(t *testing.T) {
	database := newNamedDatabase(t)
	first := openOn(t, database)
	schema(t, first)

	// The winner's claim, held open so the second runner has to wait on it.
	tx, err := first.BeginTx(t.Context(), ledger.TxOptions())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	var free bool

	if err := tx.QueryRowContext(t.Context(), `
SELECT l.owner IS NULL OR e.expires_at IS NULL OR e.expires_at < now()
  FROM mig.lease l JOIN mig.lease_expiry e ON e.id = l.id
 WHERE l.id = 1 FOR UPDATE OF l, e`).Scan(&free); err != nil {
		t.Fatalf("claim: %v", err)
	}

	if !free {
		t.Fatal("the lease was not free to begin with")
	}

	if _, err := tx.ExecContext(t.Context(),
		`UPDATE mig.lease SET owner = 'winner', fence = fence + 1 WHERE id = 1`); err != nil {
		t.Fatalf("take: %v", err)
	}

	if _, err := tx.ExecContext(t.Context(),
		`UPDATE mig.lease_expiry SET expires_at = now() + interval '1 hour' WHERE id = 1`); err != nil {
		t.Fatalf("start expiry: %v", err)
	}

	// The loser starts while the winner still holds the row, so it blocks
	// inside the claim and takes the re-read path when the commit lands.
	second := openOn(t, database)
	outcome := make(chan error, 1)

	go func() {
		_, err := lease.Acquire(t.Context(), second, lease.Config{
			Owner:    lease.NewOwner(),
			TTL:      time.Minute,
			OnLocked: lease.Fail,
		})
		outcome <- err
	}()

	waitForBlockedClaim(t, first)

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if err := <-outcome; !errors.Is(err, lease.ErrLocked) {
		t.Fatalf("the second runner got %v, want ErrLocked", err)
	}
}

// waitForBlockedClaim waits until a backend is waiting on a lock, which is the
// only way to know the second runner reached the claim before the commit.
func waitForBlockedClaim(t *testing.T, db *sql.DB) {
	t.Helper()

	const query = `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock'`

	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		var blocked int

		if err := db.QueryRowContext(t.Context(), query).Scan(&blocked); err != nil {
			t.Fatalf("look for a blocked backend: %v", err)
		}

		if blocked > 0 {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("no runner ever blocked on the lease row")
}
