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

package crash_test

import (
	"errors"
	"os"
	"os/exec"
	"regexp"
	"syscall"
	"testing"

	"github.com/tochemey/mig/internal/crash"
)

// envChild marks the re-executed copy of this test that plays the victim.
const envChild = "MIG_CRASH_TEST_CHILD"

// TestAtKillsArmedPoint checks that an armed point ends the process outright.
// It re-executes the test binary because a kill cannot be observed from inside
// the process it kills.
func TestAtKillsArmedPoint(t *testing.T) {
	if os.Getenv(envChild) == "1" {
		crash.At(crash.BeforeLeaseAcquire)

		// Reached only if At declined to kill; the parent reads the exit code.
		os.Exit(0)
	}

	err := reexec(t, crash.BeforeLeaseAcquire)

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("child exited with %v, want a fatal signal", err)
	}

	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("unexpected wait status type %T", exitErr.Sys())
	}

	if !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("child exited with %v, want SIGKILL", exitErr)
	}
}

// TestAtIgnoresOtherPoints checks that arming one point leaves the others
// inert, so a test can crash at a chosen site.
func TestAtIgnoresOtherPoints(t *testing.T) {
	if os.Getenv(envChild) == "1" {
		crash.At(crash.AfterLeaseAcquire)
		crash.At(crash.BeforeLeaseRelease)
		os.Exit(0)
	}

	if err := reexec(t, crash.BeforeLeaseAcquire); err != nil {
		t.Fatalf("child died at an unarmed point: %v", err)
	}
}

// TestAtIgnoresUnarmedProcess covers the shipping configuration, where the
// environment variable is absent and every call is inert.
func TestAtIgnoresUnarmedProcess(t *testing.T) {
	if os.Getenv(envChild) == "1" {
		crash.At(crash.BeforeLeaseAcquire)
		os.Exit(0)
	}

	if err := reexec(t, ""); err != nil {
		t.Fatalf("child died with no crash point armed: %v", err)
	}
}

// TestEveryPointFires checks each named point individually. A constant that is
// never armed anywhere is a crash the matrices believe they are exercising and
// are not.
func TestEveryPointFires(t *testing.T) {
	for _, point := range crash.Points() {
		t.Run(point, func(t *testing.T) {
			if os.Getenv(envChild) == "1" {
				crash.At(os.Getenv(crash.EnvPoint))
				os.Exit(0)
			}

			err := reexec(t, point)

			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("child exited with %v, want a fatal signal", err)
			}

			status, ok := exitErr.Sys().(syscall.WaitStatus)
			if !ok {
				t.Fatalf("unexpected wait status type %T", exitErr.Sys())
			}

			if !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatalf("child exited with %v, want SIGKILL", exitErr)
			}
		})
	}
}

// TestPointsAreDistinct keeps two sites from sharing a name, which would arm
// both at once and make the matrix row that used it prove nothing.
func TestPointsAreDistinct(t *testing.T) {
	seen := make(map[string]bool)

	for _, point := range crash.Points() {
		if point == "" {
			t.Fatal("a crash point has an empty name, which would arm on an unset variable")
		}

		if seen[point] {
			t.Fatalf("crash point %q is declared twice", point)
		}

		seen[point] = true
	}
}

// reexec runs this test again as a child with point armed, returning the exit
// error. An empty point leaves the process unarmed.
func reexec(t *testing.T, point string) error {
	t.Helper()

	//nolint:gosec // G204: the arguments are this test binary and its own name.
	cmd := exec.Command(os.Args[0], "-test.run=^"+regexp.QuoteMeta(t.Name())+"$")
	cmd.Env = append(os.Environ(), envChild+"=1")

	if point != "" {
		cmd.Env = append(cmd.Env, crash.EnvPoint+"="+point)
	}

	return cmd.Run()
}
