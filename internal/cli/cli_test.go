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

package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tochemey/mig/internal/cli"
	"github.com/tochemey/mig/internal/exec"
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

// migration is a single transactional step, which needs no concurrency to
// exercise the command end to end.
const migration = `-- +mig step: add_email
ALTER TABLE users ADD COLUMN email text;
`

// TestUpAppliesAndConverges covers the command doing its job, and the stdout
// contract the recovery tests read: a second run reports no work.
func TestUpAppliesAndConverges(t *testing.T) {
	database := newDatabase(t)
	dir := migrationDir(t)

	stdout, stderr, err := run(t, "up", "--dsn", shared.DSN(database), "--dir", dir)
	if err != nil {
		t.Fatalf("up: %v\nstderr: %s", err, stderr)
	}

	first := decode(t, stdout)
	if first.Applied != 1 {
		t.Fatalf("first run applied %d steps, want 1: %+v", first.Applied, first)
	}

	stdout, stderr, err = run(t, "up", "--dsn", shared.DSN(database), "--dir", dir)
	if err != nil {
		t.Fatalf("second up: %v\nstderr: %s", err, stderr)
	}

	if second := decode(t, stdout); second.Applied != 0 {
		t.Fatalf("second run applied %d steps, want 0: %+v", second.Applied, second)
	}
}

// TestUpReportsBeingLocked covers --on-locked=fail meeting a held lease. The
// exit code has to distinguish it from a failure, since a scheduler treats the
// two differently.
func TestUpReportsBeingLocked(t *testing.T) {
	database := newDatabase(t)
	dir := migrationDir(t)

	holder := holdLease(t, database)

	t.Cleanup(func() {
		if err := holder.Release(context.Background()); err != nil {
			t.Errorf("release lease: %v", err)
		}
	})

	_, _, err := run(t, "up", "--dsn", shared.DSN(database), "--dir", dir, "--on-locked", "fail")

	if !errors.Is(err, lease.ErrLocked) {
		t.Fatalf("up returned %v, want ErrLocked", err)
	}

	if code := cli.ExitCode(err); code != cli.ExitLocked {
		t.Fatalf("exit code is %d, want %d", code, cli.ExitLocked)
	}
}

// TestUpRejectsBadInvocations covers the ways a run can be misconfigured. Each
// must fail before touching the database.
func TestUpRejectsBadInvocations(t *testing.T) {
	dir := migrationDir(t)

	cases := map[string][]string{
		"no dsn":            {"up", "--dir", dir},
		"missing directory": {"up", "--dsn", "postgres://x/y", "--dir", filepath.Join(dir, "absent")},
		"unknown flag":      {"up", "--teleport"},
		"unexpected argument": {
			"up", "--dsn", "postgres://x/y", "--dir", dir, "extra",
		},
		"unknown command": {"teleport"},
	}

	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := run(t, args...); err == nil {
				t.Fatalf("%v was accepted", args)
			}
		})
	}
}

// TestUpRejectsAnUnreachableDatabase covers a connection string that parses but
// leads nowhere.
func TestUpRejectsAnUnreachableDatabase(t *testing.T) {
	requireHarness(t)

	dir := migrationDir(t)

	_, _, err := run(t, "up", "--dsn", shared.DSN("no_such_database"), "--dir", dir)
	if err == nil {
		t.Fatal("up accepted an unreachable database")
	}
}

// TestExitCode covers the mapping the process exits with.
func TestExitCode(t *testing.T) {
	if got := cli.ExitCode(nil); got != cli.ExitOK {
		t.Fatalf("nil maps to %d, want %d", got, cli.ExitOK)
	}

	if got := cli.ExitCode(errors.New("boom")); got != cli.ExitError {
		t.Fatalf("an ordinary error maps to %d, want %d", got, cli.ExitError)
	}
}

// TestVersionIsReported covers the flag an operator uses to work out which
// build is applying their migrations.
func TestVersionIsReported(t *testing.T) {
	stdout, _, err := run(t, "--version")
	if err != nil {
		t.Fatalf("version: %v", err)
	}

	if !strings.Contains(stdout, cli.Version) {
		t.Fatalf("version output is %q, want it to mention %q", stdout, cli.Version)
	}
}

// run executes the command tree and captures its streams.
func run(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()

	var out, errOut bytes.Buffer

	root := cli.New()
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)

	err = root.ExecuteContext(t.Context())

	return out.String(), errOut.String(), err
}

// decode reads the machine-readable summary from a run's stdout.
func decode(t *testing.T, stdout string) exec.Summary {
	t.Helper()

	lines := strings.Split(strings.TrimSpace(stdout), "\n")

	var summary exec.Summary

	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &summary); err != nil {
		t.Fatalf("decode summary %q: %v", stdout, err)
	}

	return summary
}

// requireHarness skips when TestMain could not reach a docker daemon.
func requireHarness(t *testing.T) *harness.Harness {
	t.Helper()

	if shared == nil {
		t.Skip("postgres container not available")
	}

	return shared
}

// newDatabase gives the test its own database holding the fixture.
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

// migrationDir writes the fixture migration and returns its directory.
func migrationDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	path := filepath.Join(dir, "20240817120000_add_email.sql")
	if err := os.WriteFile(path, []byte(migration), 0o600); err != nil {
		t.Fatalf("write migration: %v", err)
	}

	return dir
}

// holdLease takes the lease so a run has to contend for it.
func holdLease(t *testing.T, database string) *lease.Lease {
	t.Helper()

	db, err := shared.Open(t.Context(), database)
	if err != nil {
		t.Fatalf("open %q: %v", database, err)
	}

	t.Cleanup(func() {
		_ = db.Close()
	})

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
