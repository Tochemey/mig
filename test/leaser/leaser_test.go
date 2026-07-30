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

package leaser_test

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tochemey/mig/internal/lease"
	"github.com/tochemey/mig/internal/ledger"
	"github.com/tochemey/mig/test/harness"
	"github.com/tochemey/mig/test/leaser"
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

// The recovery tests drive this fixture as a child process, which is the only
// way to test a kill. Running it in-process covers the failure paths a spawned
// process cannot reach without contriving a crash for each one.

// TestRunCompletes covers the ordinary path: schema, lease, claim, hold,
// commit, release.
func TestRunCompletes(t *testing.T) {
	database := newDatabase(t)

	run := &recording{}

	if code := leaser.Run(t.Context(), config(t, database, "ok"), run.record); code != leaser.ExitOK {
		t.Fatalf("run exited %d: %s", code, run)
	}

	for _, marker := range []string{
		leaser.MarkerAcquired, leaser.MarkerClaimed, leaser.MarkerCommitted, leaser.MarkerReleased,
	} {
		if !run.contains(marker) {
			t.Fatalf("run did not report %q: %s", marker, run)
		}
	}

	if got := status(t, database, "ok"); got != ledger.StatusSucceeded {
		t.Fatalf("migration is %q, want %q", got, ledger.StatusSucceeded)
	}
}

// TestRunRejectsMalformedDSN covers the connection string never being parsed.
func TestRunRejectsMalformedDSN(t *testing.T) {
	cfg := leaser.Config{DSN: "://not-a-dsn", MigrationID: "x", TTL: time.Second}

	run := &recording{}

	if code := leaser.Run(t.Context(), cfg, run.record); code != leaser.ExitError {
		t.Fatalf("run exited %d, want %d: %s", code, leaser.ExitError, run)
	}
}

// TestRunReportsSchemaFailure covers a DSN that parses but leads nowhere, so
// the ledger cannot be created.
func TestRunReportsSchemaFailure(t *testing.T) {
	requireHarness(t)

	cfg := leaser.Config{DSN: shared.DSN("no_such_database"), MigrationID: "x", TTL: time.Second}

	run := &recording{}

	if code := leaser.Run(t.Context(), cfg, run.record); code != leaser.ExitError {
		t.Fatalf("run exited %d, want %d: %s", code, leaser.ExitError, run)
	}

	if !run.contains("ensure schema") {
		t.Fatalf("run did not report the failing stage: %s", run)
	}
}

// TestRunReportsLocked covers --on-locked=fail meeting a held lease.
func TestRunReportsLocked(t *testing.T) {
	database := newDatabase(t)

	holder := hold(t, database)

	t.Cleanup(func() {
		_ = holder.Release(context.Background())
	})

	cfg := config(t, database, "locked-out")
	cfg.OnLocked = lease.Fail

	run := &recording{}

	if code := leaser.Run(t.Context(), cfg, run.record); code != leaser.ExitLocked {
		t.Fatalf("run exited %d, want %d: %s", code, leaser.ExitLocked, run)
	}

	if !run.contains(leaser.MarkerLocked) {
		t.Fatalf("run did not report being locked out: %s", run)
	}
}

// TestRunReportsAbandonedWait covers a wait cut short by cancellation, which is
// a different outcome from finding the lease held.
func TestRunReportsAbandonedWait(t *testing.T) {
	database := newDatabase(t)

	holder := hold(t, database)

	t.Cleanup(func() {
		_ = holder.Release(context.Background())
	})

	cfg := config(t, database, "abandoned")
	cfg.OnLocked = lease.Wait
	cfg.WaitTimeout = time.Minute

	ctx, cancel := context.WithCancel(t.Context())

	time.AfterFunc(300*time.Millisecond, cancel)

	run := &recording{}

	if code := leaser.Run(ctx, cfg, run.record); code != leaser.ExitError {
		t.Fatalf("run exited %d, want %d: %s", code, leaser.ExitError, run)
	}
}

// TestRunReportsClaimFailure covers the first ledger write failing for a reason
// other than the fence.
func TestRunReportsClaimFailure(t *testing.T) {
	database := newDatabase(t)
	db := openDatabase(t, database)

	if err := ledger.EnsureSchema(t.Context(), db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	// A column the ledger does not know to fill makes every insert fail.
	const breakIt = `ALTER TABLE mig.migrations ADD COLUMN required text NOT NULL`

	if _, err := db.ExecContext(t.Context(), breakIt); err != nil {
		t.Fatalf("break the ledger: %v", err)
	}

	run := &recording{}

	if code := leaser.Run(t.Context(), config(t, database, "unwritable"), run.record); code != leaser.ExitError {
		t.Fatalf("run exited %d, want %d: %s", code, leaser.ExitError, run)
	}
}

// TestRunReportsCommitFailure covers the final write failing after the claim
// succeeded, with the row disappearing underneath a run that is mid-flight.
func TestRunReportsCommitFailure(t *testing.T) {
	database := newDatabase(t)

	cfg := config(t, database, "vanishing")
	cfg.Hold = 5 * time.Second

	run := &recording{}

	done := make(chan int, 1)

	go func() {
		done <- leaser.Run(context.Background(), cfg, run.record)
	}()

	run.waitFor(t, leaser.MarkerClaimed)

	db := openDatabase(t, database)

	if _, err := db.ExecContext(t.Context(), `DELETE FROM mig.migrations WHERE id = $1`, "vanishing"); err != nil {
		t.Fatalf("delete the claimed row: %v", err)
	}

	select {
	case code := <-done:
		if code != leaser.ExitError {
			t.Fatalf("run exited %d, want %d: %s", code, leaser.ExitError, run)
		}

	case <-time.After(30 * time.Second):
		t.Fatal("run never finished")
	}
}

// requireHarness skips when TestMain could not reach a docker daemon.
func requireHarness(t *testing.T) *harness.Harness {
	t.Helper()

	if shared == nil {
		t.Skip("postgres container not available")
	}

	return shared
}

// newDatabase gives the test its own database, without the ledger schema.
func newDatabase(t *testing.T) string {
	t.Helper()

	h := requireHarness(t)

	name, err := h.Clone(t.Context(), harness.Template)
	if err != nil {
		t.Fatalf("clone template: %v", err)
	}

	t.Cleanup(func() {
		if err := h.DropDatabase(context.Background(), name); err != nil {
			t.Errorf("drop clone %q: %v", name, err)
		}
	})

	return name
}

// openDatabase connects to a database for direct assertions and setup.
func openDatabase(t *testing.T, database string) *sql.DB {
	t.Helper()

	db, err := shared.Open(t.Context(), database)
	if err != nil {
		t.Fatalf("open %q: %v", database, err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close pool: %v", err)
		}
	})

	return db
}

// config builds a fixture configuration that completes promptly.
func config(t *testing.T, database, id string) leaser.Config {
	t.Helper()

	return leaser.Config{
		DSN:         shared.DSN(database),
		MigrationID: id,
		TTL:         2 * time.Second,
		Hold:        0,
		OnLocked:    lease.Fail,
		WaitTimeout: 5 * time.Second,
	}
}

// hold takes the lease so a run has to contend for it.
func hold(t *testing.T, database string) *lease.Lease {
	t.Helper()

	db := openDatabase(t, database)

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

	return held
}

// status reads a migration's recorded status.
func status(t *testing.T, database, id string) ledger.Status {
	t.Helper()

	migration, err := ledger.LoadMigration(t.Context(), openDatabase(t, database), id)
	if err != nil {
		t.Fatalf("load migration %q: %v", id, err)
	}

	return migration.Status
}

// recording collects a run's progress reports. Tests observe a run still in
// flight, so the lines are written and read from different goroutines.
type recording struct {
	mu    sync.Mutex
	lines []string
}

// record appends a reported line.
func (r *recording) record(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.lines = append(r.lines, line)
}

// contains reports whether any recorded line contains marker.
func (r *recording) contains(marker string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, line := range r.lines {
		if strings.Contains(line, marker) {
			return true
		}
	}

	return false
}

// String renders everything recorded so far, for failure messages.
func (r *recording) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return strings.Join(r.lines, "\n")
}

// waitFor blocks until a run in flight has reported marker.
func (r *recording) waitFor(t *testing.T, marker string) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)

	for {
		if r.contains(marker) {
			return
		}

		if !time.Now().Before(deadline) {
			t.Fatalf("run never reported %q; recorded:\n%s", marker, r)
		}

		time.Sleep(10 * time.Millisecond)
	}
}
