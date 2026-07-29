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

// Package kill_test proves the recovery invariant: for any point at which the
// migrator dies, re-running it converges on the same state as an uninterrupted
// run, without human intervention.
package kill_test

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tochemey/mig/internal/ledger"
	"github.com/tochemey/mig/test/harness"
	"github.com/tochemey/mig/test/leaser"
)

const (
	leaserPkg = "github.com/tochemey/mig/test/leaser/cmd/leaser"
	migPkg    = "github.com/tochemey/mig/cmd/mig"
)

var (
	// shared is the container for this package, or nil when docker is absent.
	shared *harness.Harness

	// leaserBin exercises the lease on its own, without a plan to apply.
	leaserBin string

	// migBin is the migrator itself.
	migBin string

	// goldenStates holds, per migration, the result of an uninterrupted run.
	goldenStates = map[string]harness.State{}
	goldenMu     sync.Mutex
)

// TestMain brings up one container and builds the binaries these tests spawn.
func TestMain(m *testing.M) {
	os.Exit(harness.Main(m, templateRows, func(ctx context.Context, h *harness.Harness) error {
		leaser, err := harness.Build(ctx, h.BinDir(), leaserPkg)
		if err != nil {
			return err
		}

		mig, err := harness.Build(ctx, h.BinDir(), migPkg)
		if err != nil {
			return err
		}

		shared, leaserBin, migBin = h, leaser, mig

		return nil
	}))
}

// templateRows is enough rows that a concurrent index build takes long enough
// to be killed part-way through, and few enough to keep the suite quick.
const templateRows = 100_000

// TestL1ExactlyOneRunnerApplies covers --on-locked=fail: several runners start
// together, exactly one gets in, and the others exit cleanly.
func TestL1ExactlyOneRunnerApplies(t *testing.T) {
	database := newDatabase(t)

	const runners = 4

	procs := make([]*harness.Process, runners)

	for i := range procs {
		procs[i] = start(t, database,
			"--id", "l1-"+string(rune('a'+i)),
			"--ttl", "30s",
			"--hold", "2s",
			"--on-locked", "fail",
		)
	}

	admitted, locked := 0, 0

	for i, proc := range procs {
		code, err := proc.Wait(60 * time.Second)
		if err != nil {
			t.Fatalf("runner %d: %v", i, err)
		}

		switch code {
		case leaser.ExitOK:
			admitted++
		case leaser.ExitLocked:
			locked++
		default:
			t.Fatalf("runner %d exited %d\nstdout: %s\nstderr: %s", i, code, proc.Stdout(), proc.Stderr())
		}
	}

	if admitted != 1 {
		t.Fatalf("%d runners applied, want exactly 1", admitted)
	}

	if locked != runners-1 {
		t.Fatalf("%d runners were locked out, want %d", locked, runners-1)
	}

	// One acquisition means one fence increment. A second would mean a runner
	// got in behind the first.
	if got := fence(t, database); got != 1 {
		t.Fatalf("fence is %d after one admitted runner, want 1", got)
	}
}

// TestL1WaitingRunnerFollowsRelease covers --on-locked=wait: the second runner
// runs after the first, never beside it.
func TestL1WaitingRunnerFollowsRelease(t *testing.T) {
	database := newDatabase(t)

	first := start(t, database, "--id", "l1w-a", "--ttl", "30s", "--hold", "2s", "--on-locked", "wait")

	// Start the second only once the first holds the lease, so the test measures
	// waiting rather than a lucky ordering.
	waitFor(t, first, leaser.MarkerClaimed)

	second := start(t, database, "--id", "l1w-b", "--ttl", "30s", "--hold", "0s", "--on-locked", "wait")

	for name, proc := range map[string]*harness.Process{"first": first, "second": second} {
		code, err := proc.Wait(60 * time.Second)
		if err != nil {
			t.Fatalf("%s runner: %v", name, err)
		}

		if code != leaser.ExitOK {
			t.Fatalf("%s runner exited %d\nstdout: %s\nstderr: %s", name, code, proc.Stdout(), proc.Stderr())
		}
	}

	if got := fence(t, database); got != 2 {
		t.Fatalf("fence is %d after two sequential runners, want 2", got)
	}

	// Judged by the server's clock: the second runner cannot have started before
	// the first finished.
	firstEnd := finishedAt(t, database, "l1w-a")
	secondStart := startedAt(t, database, "l1w-b")

	if secondStart.Before(firstEnd) {
		t.Fatalf("runners overlapped: second started %s, first finished %s", secondStart, firstEnd)
	}
}

// TestL2FrozenRunnerIsFencedOut validates the fencing token.
//
// A runner frozen past its lease expiry wakes believing it still owns the
// lease, and a write that lands then clobbers the runner that replaced it.
// SIGSTOP and SIGCONT make that sequence deterministic.
func TestL2FrozenRunnerIsFencedOut(t *testing.T) {
	database := newDatabase(t)

	frozen := start(t, database, "--id", "l2-a", "--ttl", "1s", "--hold", "5m", "--on-locked", "fail")
	waitFor(t, frozen, leaser.MarkerClaimed)

	if err := frozen.Freeze(); err != nil {
		t.Fatalf("freeze first runner: %v", err)
	}

	waitForLeaseExpiry(t, database)

	successor := start(t, database, "--id", "l2-b", "--ttl", "30s", "--hold", "0s", "--on-locked", "fail")

	code, err := successor.Wait(60 * time.Second)
	if err != nil {
		t.Fatalf("successor: %v", err)
	}

	if code != leaser.ExitOK {
		t.Fatalf("successor exited %d\nstdout: %s\nstderr: %s", code, successor.Stdout(), successor.Stderr())
	}

	// The first runner wakes up still believing the lease is its own.
	if err := frozen.Thaw(); err != nil {
		t.Fatalf("thaw first runner: %v", err)
	}

	code, err = frozen.Wait(60 * time.Second)
	if err != nil {
		t.Fatalf("frozen runner: %v", err)
	}

	if code != leaser.ExitFenced {
		t.Fatalf("frozen runner exited %d, want %d (fenced)\nstdout: %s\nstderr: %s",
			code, leaser.ExitFenced, frozen.Stdout(), frozen.Stderr())
	}

	if !strings.Contains(frozen.Stdout(), leaser.MarkerFenced) {
		t.Fatalf("frozen runner did not report being fenced\nstdout: %s", frozen.Stdout())
	}

	if strings.Contains(frozen.Stdout(), leaser.MarkerCommitted) {
		t.Fatalf("frozen runner committed after losing the lease\nstdout: %s", frozen.Stdout())
	}

	// Its row is still where the freeze left it, so nothing it did after waking
	// reached the database.
	if got := status(t, database, "l2-a"); got != ledger.StatusRunning {
		t.Fatalf("frozen runner's row is %q, want %q — its post-thaw write landed", got, ledger.StatusRunning)
	}

	if got := status(t, database, "l2-b"); got != ledger.StatusSucceeded {
		t.Fatalf("successor's row is %q, want %q", got, ledger.StatusSucceeded)
	}
}

// TestL3SuccessorResumesAfterKill checks that a runner killed mid-work does not
// lock the database out: the next takes over once the lease lapses.
func TestL3SuccessorResumesAfterKill(t *testing.T) {
	database := newDatabase(t)

	killed := start(t, database, "--id", "l3-a", "--ttl", "1s", "--hold", "5m", "--on-locked", "fail")
	waitFor(t, killed, leaser.MarkerClaimed)

	if err := killed.Kill(); err != nil {
		t.Fatalf("kill first runner: %v", err)
	}

	if _, err := killed.Wait(30 * time.Second); err != nil {
		t.Fatalf("wait for first runner: %v", err)
	}

	if err := shared.WaitBackendsGone(t.Context(), database, 60*time.Second); err != nil {
		t.Fatalf("backends of the killed runner did not drain: %v", err)
	}

	successor := start(t, database, "--id", "l3-b", "--ttl", "30s", "--hold", "0s", "--on-locked", "wait")

	code, err := successor.Wait(60 * time.Second)
	if err != nil {
		t.Fatalf("successor: %v", err)
	}

	if code != leaser.ExitOK {
		t.Fatalf("successor exited %d\nstdout: %s\nstderr: %s", code, successor.Stdout(), successor.Stderr())
	}

	// The dead runner left its row mid-flight, which is diagnostic rather than a
	// blocker.
	if got := status(t, database, "l3-a"); got != ledger.StatusRunning {
		t.Fatalf("killed runner's row is %q, want %q", got, ledger.StatusRunning)
	}
}

// newDatabase gives the test its own database without the ledger schema, since
// creating it is part of a cold start.
func newDatabase(t *testing.T) string {
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

// start spawns a runner against a database and registers its cleanup.
func start(t *testing.T, database string, args ...string) *harness.Process {
	t.Helper()

	proc, err := harness.Start(leaserBin, append([]string{"--dsn", shared.DSN(database)}, args...), nil)
	if err != nil {
		t.Fatalf("start runner: %v", err)
	}

	t.Cleanup(proc.Cleanup)

	return proc
}

// waitFor blocks until a runner prints marker.
func waitFor(t *testing.T, proc *harness.Process, marker string) {
	t.Helper()

	if err := proc.WaitOutput(marker, 60*time.Second); err != nil {
		t.Fatalf("waiting for %q: %v", marker, err)
	}
}

// openLedger connects to a database for assertions about the ledger.
func openLedger(t *testing.T, database string) *sql.DB {
	t.Helper()

	db, err := shared.Open(t.Context(), database)
	if err != nil {
		t.Fatalf("open %q: %v", database, err)
	}

	t.Cleanup(func() {
		_ = db.Close()
	})

	return db
}

// fence reads the current fence token.
func fence(t *testing.T, database string) int64 {
	t.Helper()

	var token int64
	if err := openLedger(t, database).QueryRowContext(t.Context(),
		"SELECT fence FROM mig.lease WHERE id = 1").Scan(&token); err != nil {
		t.Fatalf("read fence: %v", err)
	}

	return token
}

// status reads a migration's recorded status.
func status(t *testing.T, database, id string) ledger.Status {
	t.Helper()

	migration, err := ledger.LoadMigration(t.Context(), openLedger(t, database), id)
	if err != nil {
		t.Fatalf("load migration %q: %v", id, err)
	}

	return migration.Status
}

// startedAt reads when a migration was claimed, by the server's clock.
func startedAt(t *testing.T, database, id string) time.Time {
	t.Helper()

	return timestamp(t, database, id, "started_at")
}

// finishedAt reads when a migration completed, by the server's clock.
func finishedAt(t *testing.T, database, id string) time.Time {
	t.Helper()

	return timestamp(t, database, id, "finished_at")
}

// timestamp reads one timestamp column of a migration row.
func timestamp(t *testing.T, database, id, column string) time.Time {
	t.Helper()

	var at time.Time

	//nolint:gosec // G201: the column name is a literal from the callers above.
	query := "SELECT " + column + " FROM mig.migrations WHERE id = $1"

	if err := openLedger(t, database).QueryRowContext(t.Context(), query, id).Scan(&at); err != nil {
		t.Fatalf("read %s of %q: %v", column, id, err)
	}

	return at
}

// waitForLeaseExpiry blocks until the server considers the lease lapsed, which
// is what acquisition judges by.
func waitForLeaseExpiry(t *testing.T, database string) {
	t.Helper()

	db := openLedger(t, database)
	deadline := time.Now().Add(30 * time.Second)

	for {
		var expired bool

		const query = `SELECT coalesce(expires_at < now(), true) FROM mig.lease WHERE id = 1`

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
