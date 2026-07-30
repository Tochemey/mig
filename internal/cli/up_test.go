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
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tochemey/mig/internal/cli"
	"github.com/tochemey/mig/internal/lease"
)

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

// TestUpShowsItsProgress covers the display on stderr: the step that ran, its
// mark and duration on the first run, and silence on the second, since a
// converged database has nothing to show.
func TestUpShowsItsProgress(t *testing.T) {
	database := newDatabase(t)
	dir := migrationDir(t)
	dsn := shared.DSN(database)

	_, stderr, err := run(t, "up", "--dsn", dsn, "--dir", dir)
	if err != nil {
		t.Fatalf("up: %v", err)
	}

	for _, want := range []string{"[1/1] add_email", "✓"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr %q does not show %q", stderr, want)
		}
	}

	_, stderr, err = run(t, "up", "--dsn", dsn, "--dir", dir)
	if err != nil {
		t.Fatalf("second up: %v", err)
	}

	if stderr != "" {
		t.Fatalf("a converged run still rendered %q", stderr)
	}
}

// TestUpVerboseEmitsJSONLogs covers the switch a log pipeline uses: the same
// records as the display, as structured JSON instead.
func TestUpVerboseEmitsJSONLogs(t *testing.T) {
	database := newDatabase(t)

	_, stderr, err := run(t, "up", "--dsn", shared.DSN(database),
		"--dir", migrationDir(t), "--verbose")
	if err != nil {
		t.Fatalf("up: %v", err)
	}

	for _, want := range []string{`"msg":"step running"`, `"msg":"step done"`, `"status":"succeeded"`} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr %q does not carry %q", stderr, want)
		}
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
