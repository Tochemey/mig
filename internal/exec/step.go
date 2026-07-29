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

package exec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/tochemey/mig/internal/crash"
	"github.com/tochemey/mig/internal/ledger"
	"github.com/tochemey/mig/internal/plan"
	"github.com/tochemey/mig/internal/session"
	"github.com/tochemey/mig/internal/step"
)

// runStep converges one step.
//
// The order is the product. The catalog is asked first and always; only then
// does the ledger get consulted, and only to decide whether a repair is needed
// before the work is attempted again.
func (e *Executor) runStep(ctx context.Context, migration plan.Migration,
	spec plan.Step, summary *Summary) (err error) {
	key := ledger.StepKey{MigrationID: migration.ID, Index: spec.Index}
	started := time.Now()

	result := StepResult{Name: spec.Name, Kind: string(spec.Kind)}

	work, err := spec.Build()
	if err != nil {
		return err
	}

	conn, err := e.connect(ctx, migration, spec)
	if err != nil {
		return err
	}

	// A connection that will not go back to the pool is reported: it may still
	// be holding the locks the next step needs.
	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("release connection for step %s: %w", key, closeErr))
		}
	}()

	// The catalog first, unconditionally.
	done, err := work.Satisfied(ctx, conn)
	if err != nil {
		return e.fail(ctx, key, fmt.Errorf("check whether step %q is satisfied: %w", spec.Name, err))
	}

	if done {
		result.Status = string(ledger.StatusSucceeded)
		summary.skip(result, started)

		return e.succeed(ctx, key)
	}

	repaired, err := e.reconcile(ctx, key, work, conn)
	if err != nil {
		return err
	}

	result.Repaired = repaired

	if repaired {
		summary.Repaired++

		// Clearing a partial state can complete the work outright: dropping an
		// invalid index left by an interrupted DROP INDEX CONCURRENTLY is the
		// whole of that step.
		done, err := work.Satisfied(ctx, conn)
		if err != nil {
			return e.fail(ctx, key, fmt.Errorf("check step %q after repair: %w", spec.Name, err))
		}

		if done {
			result.Status = string(ledger.StatusSucceeded)
			summary.skip(result, started)

			return e.succeed(ctx, key)
		}
	}

	attempts, err := e.beginAttempt(ctx, key)
	if err != nil {
		return err
	}

	result.Attempts = attempts

	if err := e.apply(ctx, key, work, conn); err != nil {
		return err
	}

	result.Status = string(ledger.StatusSucceeded)
	summary.apply(result, started)

	return e.succeed(ctx, key)
}

// connect pins a connection and configures it for the step.
func (e *Executor) connect(ctx context.Context, migration plan.Migration, spec plan.Step) (*sql.Conn, error) {
	conn, err := e.control.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("pin connection for step %q: %w", spec.Name, err)
	}

	cfg := session.Config{
		Application: fmt.Sprintf("mig/%s %s/%s", e.opts.Version, migration.ID, spec.Name),
		LockTimeout: session.DefaultLockTimeout,
	}

	if spec.NoLockTimeout {
		cfg.LockTimeout = 0
	}

	// A concurrent index build runs for as long as it runs; a statement timeout
	// would kill it part-way and leave an invalid index behind every time.
	if spec.Kind != step.KindDDLNoTx {
		cfg.StatementTimeout = session.DefaultStatementTimeout
	}

	if err := session.Prepare(ctx, conn, cfg); err != nil {
		prepared := fmt.Errorf("prepare session for step %q: %w", spec.Name, err)

		if closeErr := conn.Close(); closeErr != nil {
			return nil, errors.Join(prepared, fmt.Errorf("release connection: %w", closeErr))
		}

		return nil, prepared
	}

	return conn, nil
}

// reconcile repairs a step that an earlier run may have left part-way through.
//
// The trigger is the existence of any prior attempt, not the ledger's opinion
// of what happened. A crash between recording the attempt and issuing the DDL
// leaves a row that says running with nothing done, and a crash during the DDL
// leaves work half done under the same row.
func (e *Executor) reconcile(ctx context.Context, key ledger.StepKey, work step.Step, conn *sql.Conn) (bool, error) {
	recorded, err := ledger.LoadStep(ctx, e.control, key)
	if errors.Is(err, ledger.ErrNotRecorded) {
		return false, nil
	}

	if err != nil {
		return false, err
	}

	if !recorded.Attempted() {
		return false, nil
	}

	e.opts.Log.Info("repairing partial step", "step", key.String(), "attempts", recorded.Attempts)

	if err := work.Repair(ctx, conn); err != nil {
		return false, e.fail(ctx, key, fmt.Errorf("repair step %s: %w", key, err))
	}

	return true, nil
}

// beginAttempt records that work is about to start, and returns the attempt
// count. It commits before the work so a crash during the work leaves evidence.
func (e *Executor) beginAttempt(ctx context.Context, key ledger.StepKey) (int, error) {
	crash.At(crash.BeforeStepRunning)

	var attempts int

	write := func(ctx context.Context, tx *sql.Tx) error {
		count, err := ledger.IncrementAttempts(ctx, tx, key)
		if err != nil {
			return err
		}

		attempts = count

		return ledger.SetStepStatus(ctx, tx, key, ledger.StatusRunning, "")
	}

	if err := ledger.Write(ctx, e.control, e.opts.Fence, write); err != nil {
		return 0, fmt.Errorf("mark step %s running: %w", key, err)
	}

	return attempts, nil
}

// apply runs the step and verifies that it did what it claimed.
func (e *Executor) apply(ctx context.Context, key ledger.StepKey, work step.Step, conn *sql.Conn) error {
	notx, ok := work.(step.NoTxStep)
	if !ok {
		return fmt.Errorf("step %s: %w", key, plan.ErrKindUnsupported)
	}

	crash.At(crash.BeforeApply)

	run := func(ctx context.Context) error {
		return notx.Apply(ctx, conn)
	}

	if err := session.WithLockRetry(ctx, e.opts.Retry, run); err != nil {
		return e.fail(ctx, key, err)
	}

	crash.At(crash.AfterApply)

	// The postcondition. A step that reports success without changing the
	// catalog would otherwise be recorded as done, and the next run would skip
	// work that never happened.
	done, err := work.Satisfied(ctx, conn)
	if err != nil {
		return e.fail(ctx, key, fmt.Errorf("check step %s after apply: %w", key, err))
	}

	if !done {
		return e.fail(ctx, key, fmt.Errorf("step %s: %w", key, ErrPostconditionFailed))
	}

	return nil
}

// succeed records a step as done.
func (e *Executor) succeed(ctx context.Context, key ledger.StepKey) error {
	write := func(ctx context.Context, tx *sql.Tx) error {
		return ledger.SetStepStatus(ctx, tx, key, ledger.StatusSucceeded, "")
	}

	if err := ledger.Write(ctx, e.control, e.opts.Fence, write); err != nil {
		return fmt.Errorf("mark step %s succeeded: %w", key, err)
	}

	return nil
}

// fail records why a step stopped and returns the original error.
//
// The recorded status is for a human to read. It is never consulted to decide
// whether work is required, so a failure to record it must not mask the failure
// that caused it.
func (e *Executor) fail(ctx context.Context, key ledger.StepKey, cause error) error {
	write := func(ctx context.Context, tx *sql.Tx) error {
		return ledger.SetStepStatus(ctx, tx, key, ledger.StatusFailed, cause.Error())
	}

	if err := ledger.Write(ctx, e.control, e.opts.Fence, write); err != nil {
		return errors.Join(cause, fmt.Errorf("record failure of step %s: %w", key, err))
	}

	return cause
}
