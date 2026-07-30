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
	"database/sql"
	"encoding/json"
	"errors"
	"io"
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

// TestExitErrorReportsTheUnderlyingFailure covers the message an operator sees
// for a coded failure, which must be the cause rather than the code.
func TestExitErrorReportsTheUnderlyingFailure(t *testing.T) {
	requireHarness(t)

	holder := holdLease(t, newDatabase(t))

	t.Cleanup(func() {
		if err := holder.Release(context.Background()); err != nil {
			t.Errorf("release lease: %v", err)
		}
	})

	_, _, err := run(t, "up", "--dsn", shared.DSN("no_such_database"), "--dir", migrationDir(t))
	if err == nil {
		t.Fatal("up accepted an unreachable database")
	}

	if err.Error() == "" {
		t.Fatal("the failure renders as nothing")
	}
}

// TestExitCodeUnwrapsACodedFailure covers the mapping through an error that has
// been wrapped on its way up.
func TestExitCodeUnwrapsACodedFailure(t *testing.T) {
	database := newDatabase(t)

	holder := holdLease(t, database)

	t.Cleanup(func() {
		if err := holder.Release(context.Background()); err != nil {
			t.Errorf("release lease: %v", err)
		}
	})

	_, _, err := run(t, "up", "--dsn", shared.DSN(database),
		"--dir", migrationDir(t), "--on-locked", "fail")

	if got := cli.ExitCode(err); got != cli.ExitLocked {
		t.Fatalf("exit code is %d, want %d", got, cli.ExitLocked)
	}

	// The message has to name the contention, not just carry a code.
	if !errors.Is(err, lease.ErrLocked) || err.Error() == "" {
		t.Fatalf("error is %v", err)
	}
}

// The flag defaults come from the environment, so that a job spec sets them
// once rather than repeating them on every invocation.

// TestEnvironmentSuppliesTheDSN covers the variable standing in for --dsn.
func TestEnvironmentSuppliesTheDSN(t *testing.T) {
	database := newDatabase(t)

	t.Setenv(cli.EnvDSN, shared.DSN(database))
	t.Setenv(cli.EnvDir, migrationDir(t))

	stdout, _, err := run(t, "up")
	if err != nil {
		t.Fatalf("up: %v", err)
	}

	if applied := decode(t, stdout).Applied; applied != 1 {
		t.Fatalf("the run applied %d steps, want 1", applied)
	}
}

// TestEnvironmentSuppliesTheLeaseTTL covers the duration variable, including a
// value that does not parse. A stray value must not silently shorten a lease
// into one that lapses mid-migration.
func TestEnvironmentSuppliesTheLeaseTTL(t *testing.T) {
	for name, value := range map[string]string{
		"parsed":     "45s",
		"unparsable": "half an hour",
	} {
		t.Run(name, func(t *testing.T) {
			database := newDatabase(t)

			t.Setenv(cli.EnvLeaseTTL, value)

			if _, _, err := run(t, "up", "--dsn", shared.DSN(database), "--dir", migrationDir(t)); err != nil {
				t.Fatalf("up with %s=%q: %v", cli.EnvLeaseTTL, value, err)
			}
		})
	}
}

// errWrite is what a closed pipe returns, which is what stdout becomes when the
// reader on the other end goes away.
var errWrite = errors.New("write failed")

// brokenWriter fails every write, standing in for a closed stdout.
type brokenWriter struct{}

// Write always fails.
func (brokenWriter) Write([]byte) (int, error) {
	return 0, errWrite
}

// TestOutputFailuresAreReported covers every command's write to stdout. A
// report that could not be written must fail the command: a caller reading an
// empty stdout and a zero exit code would conclude there was nothing to say.
func TestOutputFailuresAreReported(t *testing.T) {
	database := newDatabase(t)
	dir := migrationDir(t)
	dsn := shared.DSN(database)

	// Ordered: each command's output is exercised against the state the one
	// before it left behind.
	cases := []struct {
		name string
		args []string
	}{
		{"verify pending", []string{"verify", "--dsn", dsn, "--dir", dir}},
		{"status empty", []string{"status", "--dsn", dsn}},
		{"status json", []string{"status", "--dsn", dsn, "--json"}},
		{"fingerprint", []string{"fingerprint", "--dsn", dsn}},
		{"fingerprint describe", []string{"fingerprint", "--dsn", dsn, "--describe"}},
		{"plan", []string{"plan", "--dir", dir}},
		{"up", []string{"up", "--dsn", dsn, "--dir", dir}},
		{"status recorded", []string{"status", "--dsn", dsn}},
		{"status recorded json", []string{"status", "--dsn", dsn, "--json"}},
		{"verify satisfied", []string{"verify", "--dsn", dsn, "--dir", dir}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runTo(t, brokenWriter{}, tc.args...)
			if !errors.Is(err, errWrite) {
				t.Fatalf("%v returned %v, want the write failure", tc.args, err)
			}
		})
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
		if err := db.Close(); err != nil {
			t.Errorf("close pool: %v", err)
		}
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

// runTo executes the command tree writing to a caller-supplied stdout.
func runTo(t *testing.T, stdout io.Writer, args ...string) error {
	t.Helper()

	root := cli.New()
	root.SetOut(stdout)
	root.SetErr(io.Discard)
	root.SetArgs(args)

	return root.ExecuteContext(t.Context())
}

// openDatabase connects for the direct setup an adoption test needs.
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

// mustExec runs a setup statement and fails the test if it does not.
func mustExec(t *testing.T, db *sql.DB, query string) {
	t.Helper()

	if _, err := db.ExecContext(t.Context(), query); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// write puts a migration file in a directory.
func write(t *testing.T, dir, name, body string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatalf("write %q: %v", name, err)
	}
}
