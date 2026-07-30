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

package kill_test

import (
	"testing"
	"time"
)

// leaseTTL is the lease every runner in this file holds. Each assertion is
// measured against it, and it matches the one --lease-ttl the tests pass.
const leaseTTL = 3 * time.Second

// A partition is the failure the rest of this package cannot produce. Every
// other cell kills the runner, and a killed runner stops by definition. Here
// the process stays healthy and only its route to Postgres disappears, so
// stopping is something it has to decide to do.

// TestL4PartitionedRunnerStopsAndExits covers a runner losing its connection
// while it holds the lease.
//
// It has to give up while the lease is still valid, and then it has to exit.
// A process that hangs on a socket nobody will answer is no better than one
// that keeps working: the job never finishes and nothing says why.
func TestL4PartitionedRunnerStopsAndExits(t *testing.T) {
	r := newRun(t, indexFixture)

	_, cut := r.partition()

	proc := r.start("")

	// The build has to be under way, so the partition interrupts work rather
	// than arriving before any started.
	r.waitForIndexBuild()

	cutAt := time.Now()

	cut()

	// The runner gives up at two thirds of its lease, then has the rest of it
	// to hand the lease back or to give up on that too.
	code, err := proc.Wait(4 * leaseTTL)
	if err != nil {
		t.Fatalf("the runner never exited after the partition: %v", err)
	}

	elapsed := time.Since(cutAt)

	if code == 0 {
		t.Fatalf("the runner reported success after %s with no database", elapsed)
	}

	if elapsed > 3*leaseTTL {
		t.Fatalf("the runner took %s to exit, long past its %s lease", elapsed, leaseTTL)
	}
}

// TestL4PartitionedRunLeavesADatabaseThatConverges covers what the next run
// finds. A partition interrupts a concurrent build in the same way a kill
// does, and the recovery has to be the same: one further run, applied and
// repaired, matching a run that was never interrupted.
func TestL4PartitionedRunLeavesADatabaseThatConverges(t *testing.T) {
	r := newRun(t, indexFixture)

	proxy, cut := r.partition()

	// The build is pinned open rather than raced, exactly as the kill tests
	// pin it. A build over this fixture takes a few hundred milliseconds, so a
	// partition timed against it would land after it, sometimes.
	release := r.blockIndexBuild()

	proc := r.start("")

	r.waitForIndexBuild()

	cut()

	if _, err := proc.Wait(4 * leaseTTL); err != nil {
		t.Fatalf("the runner never exited after the partition: %v", err)
	}

	// Released before draining, since the blocker is itself a backend.
	release()

	r.endPartition(proxy)

	// The interrupted build is the state the repair exists to clear. Without
	// it the test would pass on a partition that arrived too early to matter.
	r.drain()
	r.requireInvalidIndex()

	// The lease the runner could not release is still recorded against it, so
	// the next run waits it out rather than being locked out for good.
	r.converge()
	r.assertMatchesGolden()
	r.assertSecondRunDoesNothing()
}
