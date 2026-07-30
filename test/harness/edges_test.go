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
	"strings"
	"testing"
	"time"

	"github.com/tochemey/mig/test/dummy"
	"github.com/tochemey/mig/test/harness"
)

// The harness is the instrument the other tests are measured with, so a silent
// fault in it disarms a test rather than failing one. Its own failure paths are
// exercised here.

// TestLifecycleAgainstOlderPostgres covers [harness.WithImage], used to run
// against an older major version.
func TestLifecycleAgainstOlderPostgres(t *testing.T) {
	requireHarness(t)

	ctx := t.Context()

	older, err := harness.New(ctx, harness.WithImage("postgres:16-alpine"))
	if err != nil {
		t.Fatalf("start older postgres: %v", err)
	}

	var version string
	if err := older.Admin().QueryRowContext(ctx, "SHOW server_version").Scan(&version); err != nil {
		_ = older.Close(context.Background())
		t.Fatalf("read server version: %v", err)
	}

	if !strings.HasPrefix(version, "16") {
		_ = older.Close(context.Background())
		t.Fatalf("server version is %q, want 16.x", version)
	}

	if err := older.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestNewRejectsUnknownImage covers startup failing, which must surface rather
// than leave a package quietly skipping.
func TestNewRejectsUnknownImage(t *testing.T) {
	requireHarness(t)

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	h, err := harness.New(ctx, harness.WithImage("mig-no-such-image:0"))
	if err == nil {
		_ = h.Close(context.Background())
		t.Fatal("New accepted an image that does not exist")
	}
}

// TestOpenRejectsUnknownDatabase covers connecting to a database that is not
// there, which is how a mistyped clone name shows up.
func TestOpenRejectsUnknownDatabase(t *testing.T) {
	h := requireHarness(t)

	db, err := h.Open(t.Context(), "no_such_database")
	if err == nil {
		if err := db.Close(); err != nil {
			t.Errorf("close pool: %v", err)
		}
		t.Fatal("Open accepted a database that does not exist")
	}
}

// TestCloneRejectsUnknownTemplate covers cloning from a template that was never
// seeded.
func TestCloneRejectsUnknownTemplate(t *testing.T) {
	h := requireHarness(t)

	if _, err := h.Clone(t.Context(), "no_such_template"); err == nil {
		t.Fatal("Clone accepted a template that does not exist")
	}
}

// TestDropDatabaseReportsFailure covers a drop that cannot be carried out: the
// server refuses to drop the database the harness is connected through.
func TestDropDatabaseReportsFailure(t *testing.T) {
	h := requireHarness(t)

	if err := h.DropDatabase(t.Context(), "postgres"); err == nil {
		t.Fatal("DropDatabase accepted the maintenance database")
	}
}

// TestSeedTemplateReportsFailure covers seeding failing before it starts.
func TestSeedTemplateReportsFailure(t *testing.T) {
	h := requireHarness(t)

	if err := h.SeedTemplate(t.Context(), "postgres", 0); err == nil {
		t.Fatal("SeedTemplate accepted the maintenance database")
	}
}

// TestWaitBackendsGoneHonoursCancellation covers the wait being abandoned by
// its caller rather than by its own timeout.
func TestWaitBackendsGoneHonoursCancellation(t *testing.T) {
	h := requireHarness(t)

	name := cloneDatabase(t, h)

	startDummy(t, h, name)
	waitForDummyBackend(t, h, name)

	ctx, cancel := context.WithCancel(t.Context())

	time.AfterFunc(200*time.Millisecond, cancel)

	if err := h.WaitBackendsGone(ctx, name, time.Minute); err == nil {
		t.Fatal("WaitBackendsGone ignored its cancelled context")
	}
}

// TestBuildReportsFailure covers a child that cannot be compiled, which would
// otherwise surface later as a confusing spawn failure.
func TestBuildReportsFailure(t *testing.T) {
	h := requireHarness(t)

	if _, err := harness.Build(t.Context(), h.BinDir(), "github.com/tochemey/mig/test/no-such-package"); err == nil {
		t.Fatal("Build accepted a package that does not exist")
	}
}

// TestStartReportsFailure covers spawning a binary that is not there.
func TestStartReportsFailure(t *testing.T) {
	requireHarness(t)

	proc, err := harness.Start("/nonexistent/mig-no-such-binary", nil, nil)
	if err == nil {
		proc.Cleanup()
		t.Fatal("Start accepted a binary that does not exist")
	}
}

// TestChildExitsCleanly covers a child that finishes on its own, reporting a
// zero exit code and leaving no backend behind.
func TestChildExitsCleanly(t *testing.T) {
	h := requireHarness(t)

	name := cloneDatabase(t, h)
	proc := startDummy(t, h, name, "--hold", "100ms")

	code, err := proc.Wait(30 * time.Second)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}

	if code != 0 {
		t.Fatalf("child exited %d\nstdout: %s\nstderr: %s", code, proc.Stdout(), proc.Stderr())
	}

	if !proc.Exited() {
		t.Fatal("child reports itself still running after exiting")
	}

	if stderr := proc.Stderr(); stderr != "" {
		t.Fatalf("child wrote to stderr: %s", stderr)
	}

	// Cleanup must be safe on a process that has already finished, since tests
	// register it unconditionally.
	proc.Cleanup()
}

// TestChildReportsItsOwnFailure covers a child that exits non-zero. Exit codes
// distinguish outcomes, so a failing child must report through its status and
// not only its output.
func TestChildReportsItsOwnFailure(t *testing.T) {
	h := requireHarness(t)

	proc, err := harness.Start(dummyBin, []string{"--dsn", h.DSN("no_such_database")}, nil)
	if err != nil {
		t.Fatalf("start dummy: %v", err)
	}

	t.Cleanup(proc.Cleanup)

	code, err := proc.Wait(30 * time.Second)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}

	if code != 1 {
		t.Fatalf("child exited %d, want 1\nstdout: %s\nstderr: %s", code, proc.Stdout(), proc.Stderr())
	}

	if !strings.Contains(proc.Stderr(), "dummy:") {
		t.Fatalf("child did not explain its failure: %s", proc.Stderr())
	}
}

// TestCloseIsReportedOnceTerminated covers tearing down a harness that is
// already gone.
func TestCloseIsReportedOnceTerminated(t *testing.T) {
	requireHarness(t)

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	throwaway, err := harness.New(ctx)
	if err != nil {
		t.Fatalf("start throwaway harness: %v", err)
	}

	if err := throwaway.Close(context.Background()); err != nil {
		t.Fatalf("first close: %v", err)
	}

	if err := throwaway.Close(context.Background()); err == nil {
		t.Fatal("second close reported success for a container that was already terminated")
	}
}

// TestSignalReportsFailure covers signalling a process that is gone, which must
// be reported rather than silently ignored.
func TestSignalReportsFailure(t *testing.T) {
	h := requireHarness(t)

	name := cloneDatabase(t, h)
	proc := startDummy(t, h, name, "--hold", "100ms")

	if _, err := proc.Wait(30 * time.Second); err != nil {
		t.Fatalf("wait: %v", err)
	}

	if err := proc.Kill(); err == nil {
		t.Fatal("Kill accepted a process that had already exited")
	}
}

// TestWaitTimesOut covers a child that outlives the patience of its test.
func TestWaitTimesOut(t *testing.T) {
	h := requireHarness(t)

	name := cloneDatabase(t, h)
	proc := startDummy(t, h, name)

	if _, err := proc.Wait(200 * time.Millisecond); err == nil {
		t.Fatal("Wait returned before the child exited")
	}
}

// TestWaitOutputTimesOut covers waiting for a marker the child never prints.
func TestWaitOutputTimesOut(t *testing.T) {
	h := requireHarness(t)

	name := cloneDatabase(t, h)
	proc := startDummy(t, h, name)

	if err := proc.WaitOutput("never printed", 200*time.Millisecond); err == nil {
		t.Fatal("WaitOutput returned for a marker that was never printed")
	}
}

// TestWaitOutputDetectsEarlyExit covers the failure that would otherwise wait
// out the whole timeout: a child that died before reaching the marker.
func TestWaitOutputDetectsEarlyExit(t *testing.T) {
	h := requireHarness(t)

	name := cloneDatabase(t, h)
	proc := startDummy(t, h, name, "--hold", "50ms")

	if _, err := proc.Wait(30 * time.Second); err != nil {
		t.Fatalf("wait: %v", err)
	}

	err := proc.WaitOutput("never printed", 30*time.Second)
	if err == nil {
		t.Fatal("WaitOutput returned for a marker the exited child never printed")
	}

	if !strings.Contains(err.Error(), "exited before printing") {
		t.Fatalf("error does not explain that the child exited: %v", err)
	}
}

// TestBackendString covers the rendering used in every drain failure message.
func TestBackendString(t *testing.T) {
	backend := harness.Backend{
		PID:         42,
		State:       "active",
		Query:       "SELECT\n  pg_sleep(600)",
		Application: dummy.AppName,
	}

	got := backend.String()

	// The query is collapsed onto one line.
	if !strings.Contains(got, `query="SELECT pg_sleep(600)"`) {
		t.Fatalf("backend renders as %q", got)
	}

	if !strings.Contains(got, "pid=42") {
		t.Fatalf("backend renders as %q", got)
	}
}
