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
	"github.com/tochemey/mig/internal/ledger"
)

// A transactional step should prove the opposite property to a concurrent index
// build: because the DDL and the ledger row commit together, the window between
// doing the work and recording it does not exist.

// TestTransactionalStepRollsBackCompletely covers a kill with the DDL applied
// and nothing committed.
//
// This is the test that catches a ledger write which escaped the transaction.
// If the column is ever absent while the ledger says the step ran — or the
// other way round — the two are no longer atomic, and nothing else in the suite
// would notice.
func TestTransactionalStepRollsBackCompletely(t *testing.T) {
	t.Parallel()

	env := newRun(t, columnFixture)

	env.crash(crash.InTransaction)

	if env.hasColumn("email") {
		t.Fatal("the column survived a transaction that never committed")
	}

	recorded := env.recordedStep(0)

	if recorded.Status != ledger.StatusPending {
		t.Fatalf("step is %q, want %q: the ledger write escaped the transaction",
			recorded.Status, ledger.StatusPending)
	}

	if recorded.Attempts != 0 {
		t.Fatalf("step records %d attempts after a full rollback", recorded.Attempts)
	}

	env.converge()
	env.assertMatchesGolden()
	env.assertSecondRunDoesNothing()
}

// TestTransactionalStepSurvivesACommit covers a kill immediately after the
// commit: both the DDL and its ledger row landed, and the next run must skip
// the step rather than reapply it.
func TestTransactionalStepSurvivesACommit(t *testing.T) {
	t.Parallel()

	env := newRun(t, columnFixture)

	env.crash(crash.AfterCommit)

	if !env.hasColumn("email") {
		t.Fatal("the committed column is missing")
	}

	recorded := env.recordedStep(0)

	if recorded.Status != ledger.StatusSucceeded {
		t.Fatalf("step is %q, want %q: the ledger row did not commit with the DDL",
			recorded.Status, ledger.StatusSucceeded)
	}

	// Re-running must skip. ALTER TABLE ADD COLUMN carries no inferable
	// predicate, so the skip can only come from the ledger — which is
	// trustworthy here precisely because its row committed with the DDL.
	env.converge()
	env.assertMatchesGolden()
	env.assertSecondRunDoesNothing()
}

// TestTransactionalStepConvergesFromEveryCrashPoint covers the points around
// the transaction as well as inside it.
func TestTransactionalStepConvergesFromEveryCrashPoint(t *testing.T) {
	cases := []struct {
		name  string
		point string
	}{
		{name: "before the lease is acquired", point: crash.BeforeLeaseAcquire},
		{name: "lease held, nothing recorded", point: crash.AfterLeaseAcquire},
		{name: "about to apply, inside the transaction", point: crash.BeforeApply},
		{name: "DDL applied, nothing committed", point: crash.InTransaction},
		{name: "DDL and ledger row committed", point: crash.AfterCommit},
		{name: "step succeeded, lease not released", point: crash.BeforeLeaseRelease},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			env := newRun(t, columnFixture)

			env.crash(tc.point)
			env.converge()
			env.assertMatchesGolden()
			env.assertSecondRunDoesNothing()
		})
	}
}

// TestTransactionalStepAppliesOnce is the control, and it also pins down the
// count: a transactional step records the attempt in the same transaction as
// the work, so a converged step shows exactly one.
func TestTransactionalStepAppliesOnce(t *testing.T) {
	t.Parallel()

	env := newRun(t, columnFixture)

	summary := env.run()
	if summary.Applied != 1 {
		t.Fatalf("applied %d steps, want 1", summary.Applied)
	}

	if got := env.recordedStep(0).Attempts; got != 1 {
		t.Fatalf("step records %d attempts, want 1", got)
	}

	env.assertMatchesGolden()
	env.assertSecondRunDoesNothing()
}
