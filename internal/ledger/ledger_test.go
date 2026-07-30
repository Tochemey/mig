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

package ledger_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/tochemey/mig/internal/ledger"
	"github.com/tochemey/mig/test/harness"
)

// shared is the container for this package, or nil when docker is absent.
var shared *harness.Harness

// TestMain brings up one container for the package. The fixture table is
// irrelevant here, so the template is seeded empty.
func TestMain(m *testing.M) {
	os.Exit(harness.Main(m, 0, func(_ context.Context, h *harness.Harness) error {
		shared = h
		return nil
	}))
}

// heldFence is the fence a test takes so its writes are allowed through.
const heldFence int64 = 1

// TestEnsureSchemaIsIdempotent covers the first thing every run does: the
// schema is created on demand and re-creating it must be a no-op.
func TestEnsureSchemaIsIdempotent(t *testing.T) {
	db := newDatabase(t)
	ctx := t.Context()

	for range 3 {
		if err := ledger.EnsureSchema(ctx, db); err != nil {
			t.Fatalf("ensure schema: %v", err)
		}
	}

	var leases int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM mig.lease").Scan(&leases); err != nil {
		t.Fatalf("count lease rows: %v", err)
	}

	if leases != 1 {
		t.Fatalf("lease table has %d rows, want exactly 1", leases)
	}
}

// TestEnsureSchemaIsRaceFree covers the case L1 creates: several runners
// starting at once. CREATE ... IF NOT EXISTS is not enough on its own — two
// creators can still collide on the catalog's unique index.
func TestEnsureSchemaIsRaceFree(t *testing.T) {
	db := newDatabase(t)
	ctx := t.Context()

	const runners = 8

	errs := make([]error, runners)

	var wg sync.WaitGroup

	for i := range runners {
		wg.Add(1)

		go func() {
			defer wg.Done()

			errs[i] = ledger.EnsureSchema(ctx, db)
		}()
	}

	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent runner %d: %v", i, err)
		}
	}
}

// TestWriteAppliesWithHeldFence is the happy path: the holder of the lease can
// write.
func TestWriteAppliesWithHeldFence(t *testing.T) {
	db := newLedger(t)
	ctx := t.Context()
	fence := takeLease(t, db, heldFence)

	err := ledger.Write(ctx, db, fence, func(ctx context.Context, tx *sql.Tx) error {
		return ledger.UpsertMigration(ctx, tx, "20240817120000_add_email", "add_email")
	})
	if err != nil {
		t.Fatalf("fenced write: %v", err)
	}

	migration, err := ledger.LoadMigration(ctx, db, "20240817120000_add_email")
	if err != nil {
		t.Fatalf("load migration: %v", err)
	}

	if migration.Status != ledger.StatusPending {
		t.Fatalf("status is %q, want %q", migration.Status, ledger.StatusPending)
	}
}

// TestWriteRejectsStaleFence is the mechanism the whole design rests on: a
// runner superseded by a newer acquisition must not be able to write.
func TestWriteRejectsStaleFence(t *testing.T) {
	db := newLedger(t)
	ctx := t.Context()

	stale := takeLease(t, db, 1)

	// A second acquisition, as a successor would make.
	takeLease(t, db, 2)

	err := ledger.Write(ctx, db, stale, func(ctx context.Context, tx *sql.Tx) error {
		return ledger.UpsertMigration(ctx, tx, "clobbered", "clobbered")
	})

	if !errors.Is(err, ledger.ErrFenced) {
		t.Fatalf("stale write returned %v, want ErrFenced", err)
	}

	if _, err := ledger.LoadMigration(ctx, db, "clobbered"); !errors.Is(err, ledger.ErrNotRecorded) {
		t.Fatalf("stale runner's write landed: %v", err)
	}
}

// TestWriteRejectsWrongOwner covers the other half of the fence: the right
// token under the wrong name is still not us.
func TestWriteRejectsWrongOwner(t *testing.T) {
	db := newLedger(t)
	ctx := t.Context()

	takeLease(t, db, heldFence)

	imposter := ledger.Fence{Owner: "someone-else", Token: heldFence}

	err := ledger.Write(ctx, db, imposter, func(ctx context.Context, tx *sql.Tx) error {
		return ledger.UpsertMigration(ctx, tx, "clobbered", "clobbered")
	})

	if !errors.Is(err, ledger.ErrFenced) {
		t.Fatalf("imposter write returned %v, want ErrFenced", err)
	}
}

// TestWriteRollsBackOnError keeps a failed write from leaving a partial row
// behind: the executor records failures itself, in a later transaction.
func TestWriteRollsBackOnError(t *testing.T) {
	db := newLedger(t)
	ctx := t.Context()
	fence := takeLease(t, db, heldFence)

	boom := errors.New("boom")

	err := ledger.Write(ctx, db, fence, func(ctx context.Context, tx *sql.Tx) error {
		if err := ledger.UpsertMigration(ctx, tx, "half-written", "half-written"); err != nil {
			return err
		}

		return boom
	})

	if !errors.Is(err, boom) {
		t.Fatalf("write returned %v, want boom", err)
	}

	if _, err := ledger.LoadMigration(ctx, db, "half-written"); !errors.Is(err, ledger.ErrNotRecorded) {
		t.Fatalf("rolled-back row is still present: %v", err)
	}
}

// TestUpsertMigrationPreservesStatus covers re-running against an already
// recorded migration: the executor upserts pending rows on every run, and doing
// so must not reset what earlier runs recorded.
func TestUpsertMigrationPreservesStatus(t *testing.T) {
	db := newLedger(t)
	ctx := t.Context()
	fence := takeLease(t, db, heldFence)

	const id = "20240817120000_add_email"

	writeOrFail(t, db, fence, func(ctx context.Context, tx *sql.Tx) error {
		if err := ledger.UpsertMigration(ctx, tx, id, "add_email"); err != nil {
			return err
		}

		return ledger.SetMigrationStatus(ctx, tx, id, ledger.StatusSucceeded)
	})

	writeOrFail(t, db, fence, func(ctx context.Context, tx *sql.Tx) error {
		return ledger.UpsertMigration(ctx, tx, id, "add_email")
	})

	migration, err := ledger.LoadMigration(ctx, db, id)
	if err != nil {
		t.Fatalf("load migration: %v", err)
	}

	if migration.Status != ledger.StatusSucceeded {
		t.Fatalf("re-upsert reset status to %q", migration.Status)
	}
}

// TestSetMigrationStatusUnknown reports rather than silently succeeding on a
// migration nobody recorded.
func TestSetMigrationStatusUnknown(t *testing.T) {
	db := newLedger(t)
	fence := takeLease(t, db, heldFence)

	err := ledger.Write(t.Context(), db, fence, func(ctx context.Context, tx *sql.Tx) error {
		return ledger.SetMigrationStatus(ctx, tx, "never-recorded", ledger.StatusSucceeded)
	})

	if !errors.Is(err, ledger.ErrNotRecorded) {
		t.Fatalf("status write returned %v, want ErrNotRecorded", err)
	}
}

// TestLoadMigrationMissing distinguishes "not recorded" from a query failure.
func TestLoadMigrationMissing(t *testing.T) {
	db := newLedger(t)

	if _, err := ledger.LoadMigration(t.Context(), db, "absent"); !errors.Is(err, ledger.ErrNotRecorded) {
		t.Fatalf("load returned %v, want ErrNotRecorded", err)
	}
}

// newDatabase gives the test its own database, without the ledger schema.
func newDatabase(t *testing.T) *sql.DB {
	t.Helper()

	if shared == nil {
		t.Skip("postgres container not available")
	}

	name, err := shared.Clone(t.Context(), harness.Template)
	if err != nil {
		t.Fatalf("clone template: %v", err)
	}

	db, err := shared.Open(t.Context(), name)
	if err != nil {
		t.Fatalf("open clone: %v", err)
	}

	t.Cleanup(func() {
		_ = db.Close()

		if err := shared.DropDatabase(context.Background(), name); err != nil {
			t.Errorf("drop clone %q: %v", name, err)
		}
	})

	return db
}

// newLedger gives the test its own database with the ledger schema in place.
func newLedger(t *testing.T) *sql.DB {
	t.Helper()

	db := newDatabase(t)

	if err := ledger.EnsureSchema(t.Context(), db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	return db
}

// takeLease claims the lease row directly, standing in for package lease, which
// must not be imported here: lease depends on ledger, and testing a dependency
// through its dependent hides which one is broken.
func takeLease(t *testing.T, db *sql.DB, token int64) ledger.Fence {
	t.Helper()

	owner := "owner-" + t.Name()

	if _, err := db.ExecContext(t.Context(),
		`UPDATE mig.lease SET owner = $1, fence = $2 WHERE id = 1`, owner, token); err != nil {
		t.Fatalf("take lease at fence %d: %v", token, err)
	}

	if _, err := db.ExecContext(t.Context(),
		`UPDATE mig.lease_expiry SET expires_at = now() + interval '1 hour' WHERE id = 1`); err != nil {
		t.Fatalf("start lease expiry: %v", err)
	}

	return ledger.Fence{Owner: owner, Token: token}
}

// writeOrFail performs a fenced write that the test expects to succeed.
func writeOrFail(t *testing.T, db *sql.DB, fence ledger.Fence, fn func(context.Context, *sql.Tx) error) {
	t.Helper()

	if err := ledger.Write(t.Context(), db, fence, fn); err != nil {
		t.Fatalf("fenced write: %v", err)
	}
}
