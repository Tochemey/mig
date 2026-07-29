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

	"github.com/tochemey/mig/internal/crash"
)

// Killing the migrator at any of these points must leave a database that the
// next run converges, with no human intervention and no second run doing work.
//
// The interesting one is a kill after the DDL committed and before the ledger
// write: no transaction can cover both, so the ledger is left claiming work
// that finished. Recovery comes from asking the catalog, not from trusting the
// record.
func TestIndexBuildConvergesFromEveryCrashPoint(t *testing.T) {
	cases := []struct {
		name  string
		point string
	}{
		{name: "before the lease is acquired", point: crash.BeforeLeaseAcquire},
		{name: "lease held, nothing recorded", point: crash.AfterLeaseAcquire},
		{name: "recorded as running, no DDL issued", point: crash.BeforeStepRunning},
		{name: "running, about to issue the DDL", point: crash.BeforeApply},
		{name: "DDL committed, ledger not yet written", point: crash.AfterApply},
		{name: "step succeeded, lease not released", point: crash.BeforeLeaseRelease},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			env := newRun(t)

			env.crash(tc.point)
			env.converge()
			env.assertMatchesGolden()
			env.assertSecondRunDoesNothing()
		})
	}
}

// TestIndexBuildConvergesFromAnInterruptedBuild covers the crash with no
// shortcut: killed part-way through the build, the database is left with an
// index that exists and is not valid. Postgres will not resume it, so the next
// run has to recognise it and drop it before building again.
func TestIndexBuildConvergesFromAnInterruptedBuild(t *testing.T) {
	t.Parallel()

	env := newRun(t)

	env.killMidBuild()
	env.requireInvalidIndex()

	env.converge()
	env.assertMatchesGolden()
	env.assertSecondRunDoesNothing()
}

// TestRepairSurvivesBeingKilled covers the case people forget: the recovery
// path is code too, and it can be killed. Repair must be re-entrant, which is
// why it drops with IF EXISTS and re-reads the catalog rather than trusting
// what a previous call believed.
func TestRepairSurvivesBeingKilled(t *testing.T) {
	t.Parallel()

	env := newRun(t)

	env.killMidBuild()
	env.requireInvalidIndex()

	// The next run reaches the repair and dies inside it.
	env.crash(crash.DuringRepair)

	env.converge()
	env.assertMatchesGolden()
	env.assertSecondRunDoesNothing()
}

// TestUninterruptedRunConverges is the control. Without it, a suite where every
// run silently did nothing would still pass.
func TestUninterruptedRunConverges(t *testing.T) {
	t.Parallel()

	env := newRun(t)

	summary := env.run()
	if summary.Applied != 1 {
		t.Fatalf("applied %d steps, want 1", summary.Applied)
	}

	env.assertMatchesGolden()
	env.assertSecondRunDoesNothing()
}
