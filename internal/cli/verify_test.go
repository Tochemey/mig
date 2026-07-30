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
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tochemey/mig/internal/cli"
	"github.com/tochemey/mig/pkg/mig"
)

// The read-only commands. None of them takes a lease, and none writes anything
// a later run has to reconcile.

// TestVerifyDistinguishesPendingFromFailure covers the exit codes a scheduler
// branches on: nothing outstanding, work outstanding, and the check itself
// failing are three different things.
func TestVerifyDistinguishesPendingFromFailure(t *testing.T) {
	database := newDatabase(t)
	dir := migrationDir(t)
	dsn := shared.DSN(database)

	stdout, _, err := run(t, "verify", "--dsn", dsn, "--dir", dir)
	if !errors.Is(err, mig.ErrPending) {
		t.Fatalf("verify returned %v, want ErrPending", err)
	}

	if code := cli.ExitCode(err); code != cli.ExitPending {
		t.Fatalf("exit code is %d, want %d", code, cli.ExitPending)
	}

	if !strings.Contains(stdout, "add_email") {
		t.Fatalf("stdout %q does not name the outstanding step", stdout)
	}

	if _, _, err := run(t, "up", "--dsn", dsn, "--dir", dir); err != nil {
		t.Fatalf("up: %v", err)
	}

	stdout, _, err = run(t, "verify", "--dsn", dsn, "--dir", dir)
	if err != nil {
		t.Fatalf("verify after up: %v", err)
	}

	if !strings.Contains(stdout, "up to date") {
		t.Fatalf("stdout is %q", stdout)
	}
}

// TestVerifyRejectsBadInvocations covers the ways the check can be
// misconfigured. Each has to fail as an error, never as "up to date".
func TestVerifyRejectsBadInvocations(t *testing.T) {
	dir := migrationDir(t)

	cases := map[string][]string{
		"no dsn":            {"verify", "--dir", dir},
		"missing directory": {"verify", "--dsn", "postgres://x/y", "--dir", filepath.Join(dir, "absent")},
		"unexpected argument": {
			"verify", "--dsn", "postgres://x/y", "--dir", dir, "extra",
		},
	}

	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := run(t, args...)
			if err == nil {
				t.Fatalf("%v was accepted", args)
			}

			if errors.Is(err, mig.ErrPending) {
				t.Fatalf("%v was reported as pending work", args)
			}
		})
	}
}

// TestVerifyRejectsAnUnreachableDatabase covers a connection string that parses
// and leads nowhere.
func TestVerifyRejectsAnUnreachableDatabase(t *testing.T) {
	requireHarness(t)

	_, _, err := run(t, "verify", "--dsn", shared.DSN("no_such_database"), "--dir", migrationDir(t))
	if err == nil {
		t.Fatal("verify accepted an unreachable database")
	}
}
