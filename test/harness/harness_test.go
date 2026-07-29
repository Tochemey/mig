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

package harness_test

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tochemey/mig/test/dummy"
	"github.com/tochemey/mig/test/harness"
)

const (
	// templateRows matches the fast CI tier.
	templateRows = 100_000

	// dummyPkg is the child these tests spawn.
	dummyPkg = "github.com/tochemey/mig/test/dummy/cmd/dummy"
)

var (
	// shared is the container for this package, or nil when docker is absent.
	shared *harness.Harness

	// dummyBin is the compiled dummy child, built once in TestMain.
	dummyBin string
)

// TestMain brings up one container, seeds the template and builds the child
// binary. Tests degrade to a skip when no docker daemon is reachable.
func TestMain(m *testing.M) {
	os.Exit(harness.Main(m, templateRows, func(ctx context.Context, h *harness.Harness) error {
		bin, err := harness.Build(ctx, h.BinDir(), dummyPkg)
		if err != nil {
			return err
		}

		shared, dummyBin = h, bin

		return nil
	}))
}

// TestCloneFromTemplate covers the isolation mechanism: every test gets its own
// database, copied rather than re-seeded.
func TestCloneFromTemplate(t *testing.T) {
	h := requireHarness(t)
	ctx := t.Context()

	name := cloneDatabase(t, h)

	db, err := h.Open(ctx, name)
	if err != nil {
		t.Fatalf("open clone: %v", err)
	}

	t.Cleanup(func() {
		_ = db.Close()
	})

	var rows int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM users").Scan(&rows); err != nil {
		t.Fatalf("count fixture rows: %v", err)
	}

	if rows != templateRows {
		t.Fatalf("clone has %d rows, want %d", rows, templateRows)
	}
}

// TestClonesAreIsolated checks that a migration applied by one test is not
// visible to the next.
func TestClonesAreIsolated(t *testing.T) {
	h := requireHarness(t)
	ctx := t.Context()

	first, second := cloneDatabase(t, h), cloneDatabase(t, h)

	one, err := h.Open(ctx, first)
	if err != nil {
		t.Fatalf("open first clone: %v", err)
	}

	t.Cleanup(func() {
		_ = one.Close()
	})

	if _, err := one.ExecContext(ctx, "ALTER TABLE users ADD COLUMN email text"); err != nil {
		t.Fatalf("mutate first clone: %v", err)
	}

	two, err := h.Open(ctx, second)
	if err != nil {
		t.Fatalf("open second clone: %v", err)
	}

	t.Cleanup(func() {
		_ = two.Close()
	})

	var exists bool

	const query = `SELECT exists(
        SELECT 1 FROM information_schema.columns
         WHERE table_name = 'users' AND column_name = 'email')`

	if err := two.QueryRowContext(ctx, query).Scan(&exists); err != nil {
		t.Fatalf("inspect second clone: %v", err)
	}

	if exists {
		t.Fatal("second clone sees a column added to the first; clones are not isolated")
	}
}

// TestKilledChildBackendDrains spawns a child, kills it, and confirms no server
// backend remains. Until this holds, every recovery test races a backend that
// is still doing the work.
func TestKilledChildBackendDrains(t *testing.T) {
	h := requireHarness(t)
	ctx := t.Context()

	name := cloneDatabase(t, h)
	proc := startDummy(t, h, name)

	waitForDummyBackend(t, h, name)

	if err := proc.Kill(); err != nil {
		t.Fatalf("kill dummy: %v", err)
	}

	if _, err := proc.Wait(10 * time.Second); err != nil {
		t.Fatalf("wait for dummy to exit: %v", err)
	}

	// The client is gone, but the backend need not be.
	if err := h.WaitBackendsGone(ctx, name, 60*time.Second); err != nil {
		t.Fatalf("backend did not drain: %v", err)
	}

	backends, err := h.Backends(ctx, name)
	if err != nil {
		t.Fatalf("list backends: %v", err)
	}

	if len(backends) != 0 {
		t.Fatalf("expected no backends after drain, got %d: %v", len(backends), backends)
	}
}

// TestWaitBackendsGoneFailsWhileChildLives keeps the drain wait honest. An
// implementation that always returned nil would satisfy every other test here
// and silently disarm the suite.
func TestWaitBackendsGoneFailsWhileChildLives(t *testing.T) {
	h := requireHarness(t)
	ctx := t.Context()

	name := cloneDatabase(t, h)

	startDummy(t, h, name)
	waitForDummyBackend(t, h, name)

	err := h.WaitBackendsGone(ctx, name, 500*time.Millisecond)
	if err == nil {
		t.Fatal("WaitBackendsGone succeeded while a backend was still attached")
	}

	// The error must name the survivor, or a real failure is undiagnosable.
	if !strings.Contains(err.Error(), dummy.AppName) {
		t.Fatalf("error does not identify the surviving backend: %v", err)
	}
}

// TestFreezeAndThaw covers the signal control used to drive a runner frozen
// past its lease expiry and then woken.
func TestFreezeAndThaw(t *testing.T) {
	h := requireHarness(t)

	name := cloneDatabase(t, h)
	proc := startDummy(t, h, name)

	if err := proc.Freeze(); err != nil {
		t.Fatalf("freeze dummy: %v", err)
	}

	stopped := func(state string) bool {
		return strings.HasPrefix(state, "T")
	}

	waitForProcessState(t, proc.PID(), stopped, "stopped")

	if err := proc.Thaw(); err != nil {
		t.Fatalf("thaw dummy: %v", err)
	}

	running := func(state string) bool {
		return !strings.HasPrefix(state, "T")
	}

	waitForProcessState(t, proc.PID(), running, "running")
}

// requireHarness skips when TestMain could not reach a docker daemon.
func requireHarness(t *testing.T) *harness.Harness {
	t.Helper()

	if shared == nil {
		t.Skip("postgres container not available")
	}

	return shared
}

// cloneDatabase gives the test its own database and drops it afterwards.
func cloneDatabase(t *testing.T, h *harness.Harness) string {
	t.Helper()

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

// startDummy spawns the child against a database and waits for it to announce
// that its connection is established.
func startDummy(t *testing.T, h *harness.Harness, database string, args ...string) *harness.Process {
	t.Helper()

	proc, err := harness.Start(dummyBin, append([]string{"--dsn", h.DSN(database)}, args...), nil)
	if err != nil {
		t.Fatalf("start dummy: %v", err)
	}

	t.Cleanup(proc.Cleanup)

	if err := proc.WaitOutput(dummy.ReadyMarker, 30*time.Second); err != nil {
		t.Fatalf("dummy never became ready: %v", err)
	}

	return proc
}

// waitForDummyBackend blocks until the dummy's backend is executing its sleep.
// Killing the child before then leaves an idle backend, which exits at once and
// does not exercise the race the drain wait exists for.
func waitForDummyBackend(t *testing.T, h *harness.Harness, database string) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)

	for {
		backends, err := h.Backends(t.Context(), database)
		if err != nil {
			t.Fatalf("list backends: %v", err)
		}

		for _, b := range backends {
			if b.Application == dummy.AppName && b.State == "active" && strings.Contains(b.Query, "pg_sleep") {
				return
			}
		}

		if !time.Now().Before(deadline) {
			t.Fatalf("dummy backend never became active in %q: %v", database, backends)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// waitForProcessState blocks until the OS-level state of a process satisfies
// want, describing it as desc on failure. Signal delivery is asynchronous, so
// the state is polled. "T" is the stopped state on both Linux and macOS.
func waitForProcessState(t *testing.T, pid int, want func(string) bool, desc string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for {
		//nolint:gosec // G204: the pid comes from a process this test spawned.
		out, err := exec.Command("ps", "-o", "state=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			t.Fatalf("ps for pid %d: %v", pid, err)
		}

		state := strings.TrimSpace(string(out))
		if want(state) {
			return
		}

		if !time.Now().Before(deadline) {
			t.Fatalf("process %d is in state %q, want %s", pid, state, desc)
		}

		time.Sleep(10 * time.Millisecond)
	}
}
