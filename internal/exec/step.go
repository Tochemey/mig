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
	"strconv"
	"time"

	"github.com/tochemey/mig/internal/crash"
	"github.com/tochemey/mig/internal/ledger"
	"github.com/tochemey/mig/internal/plan"
	"github.com/tochemey/mig/internal/session"
	"github.com/tochemey/mig/internal/step"
	"github.com/tochemey/mig/internal/throttle"
)

// stepRun is one step's execution: what to run, where it runs, and what will be
// reported about it.
type stepRun struct {
	spec    plan.Step
	work    step.Step
	key     ledger.StepKey
	conn    *sql.Conn
	result  StepResult
	started time.Time

	// skipped separates "the catalog already showed the work" from "this run
	// performed it", which the summary encodes in its counters and the closing
	// record has to name directly.
	skipped bool
}

// runStep converges one step.
//
// The order is the product. The catalog is asked first and always; only then
// does the ledger get consulted, and only to decide whether a repair is needed
// before the work is attempted again.
func (e *Executor) runStep(ctx context.Context, migration plan.Migration, spec plan.Step,
	work step.Step, summary *Summary, prog *progress) (err error) {
	run := &stepRun{
		spec:    spec,
		work:    work,
		key:     ledger.StepKey{MigrationID: migration.ID, Index: spec.Index},
		result:  StepResult{Name: spec.Name, Kind: string(spec.Kind)},
		started: time.Now(),
	}

	// Every step is numbered whether or not it runs, so the display counts
	// [2/3] by what the plan holds rather than by what this run worked on.
	prog.position++
	position := prog.position

	// One record per step whatever path it took, so the display and the JSON
	// logs both account for everything the summary will list.
	defer func() {
		e.opts.Log.Info("step done",
			"migration", migration.ID, "step", spec.Name, "kind", string(spec.Kind),
			"position", position, "total", prog.total,
			"status", doneStatus(run, err), "repaired", run.result.Repaired,
			"attempts", run.result.Attempts,
			"duration_ms", time.Since(run.started).Milliseconds())
	}()

	conn, err := e.connect(ctx, migration, spec)
	if err != nil {
		return err
	}

	run.conn = conn

	// A connection that will not go back to the pool is reported: it may still
	// be holding the locks the next step needs.
	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("release connection for step %s: %w", run.key, closeErr))
		}
	}()

	// The catalog first, unconditionally.
	done, err := work.Satisfied(ctx, conn)
	if err != nil {
		return e.fail(ctx, run.key, fmt.Errorf("check whether step %s is satisfied: %w", run.key, err))
	}

	if done {
		return e.skip(ctx, run, summary)
	}

	// Announced only once the catalog has said there is work, so a converged
	// database renders as silence rather than as a list of steps not run.
	e.opts.Log.Info("step running",
		"migration", migration.ID, "step", spec.Name, "kind", string(spec.Kind),
		"position", position, "total", prog.total)

	switch applier := work.(type) {
	case step.TxStep:
		return e.runTxStep(ctx, run, applier, summary)

	case step.NoTxStep:
		return e.runNoTxStep(ctx, run, applier, summary)

	case step.ResumableStep:
		return e.runResumableStep(ctx, run, applier, summary)

	default:
		return fmt.Errorf("step %s: %w", run.key, plan.ErrKindUnsupported)
	}
}

// doneStatus names a step's outcome for its closing record.
func doneStatus(run *stepRun, err error) string {
	switch {
	case err != nil:
		return "failed"

	case run.skipped:
		return "skipped"

	default:
		return run.result.Status
	}
}

// skip records a step whose work the catalog already shows as done.
func (e *Executor) skip(ctx context.Context, run *stepRun, summary *Summary) error {
	run.skipped = true
	run.result.Status = string(ledger.StatusSucceeded)
	summary.skip(run.result, run.started)

	return e.succeed(ctx, run.key)
}

// runTxStep applies a step whose DDL and ledger row commit together.
//
// Nothing is written before the transaction, so a crash part-way through leaves
// the ledger exactly as it was. That is the property the whole shape exists for,
// and it is why this path needs neither a repair nor a postcondition check.
func (e *Executor) runTxStep(ctx context.Context, run *stepRun,
	work step.TxStep, summary *Summary) error {
	// The one place the ledger is allowed to decide. A transactional step's row
	// commits with its DDL, so "succeeded" cannot describe work that did not
	// happen — which is exactly what makes trusting it safe here and nowhere
	// else. Without it, a step whose SQL carries no inferable predicate would
	// re-run on every invocation.
	recorded, err := ledger.LoadStep(ctx, e.control, run.key)

	switch {
	case errors.Is(err, ledger.ErrNotRecorded):
	case err != nil:
		return err
	case recorded.Status == ledger.StatusSucceeded:
		run.skipped = true
		run.result.Status = string(ledger.StatusSucceeded)
		run.result.Attempts = recorded.Attempts
		summary.skip(run.result, run.started)

		return nil
	}

	attempt := func(ctx context.Context) error {
		return e.commitTxStep(ctx, run, work)
	}

	if err := session.WithLockRetry(ctx, e.opts.Retry, attempt); err != nil {
		return e.fail(ctx, run.key, err)
	}

	crash.At(crash.AfterCommit)

	run.result.Status = string(ledger.StatusSucceeded)
	summary.apply(run.result, run.started)

	return nil
}

// commitTxStep runs one attempt at a transactional step.
//
// The order inside the transaction is the contract: the fence is asserted
// first, the DDL runs, and the ledger row is written last, all before a single
// commit. A refactor that moves the ledger write outside this function reopens
// the window that transactional steps exist to avoid.
func (e *Executor) commitTxStep(ctx context.Context, run *stepRun, work step.TxStep) (err error) {
	tx, err := run.conn.BeginTx(ctx, ledger.TxOptions())
	if err != nil {
		return fmt.Errorf("begin transaction for step %s: %w", run.key, err)
	}

	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("roll back step %s: %w", run.key, rollbackErr))
		}
	}()

	if err := ledger.Guard(ctx, tx, e.opts.Fence); err != nil {
		return err
	}

	crash.At(crash.BeforeApply)

	if err := work.ApplyTx(ctx, tx); err != nil {
		return err
	}

	crash.At(crash.InTransaction)

	attempts, err := ledger.IncrementAttempts(ctx, tx, run.key)
	if err != nil {
		return err
	}

	if err := ledger.SetStepStatus(ctx, tx, run.key, ledger.StatusSucceeded, ""); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit step %s: %w", run.key, err)
	}

	// Only once the commit has landed, since a rolled-back attempt leaves no
	// trace and reporting it would overstate what the ledger holds.
	run.result.Attempts = attempts

	return nil
}

// runNoTxStep applies a step that cannot share a transaction with its ledger
// row, and so must be reconciled instead of trusted.
func (e *Executor) runNoTxStep(ctx context.Context, run *stepRun,
	work step.NoTxStep, summary *Summary) error {
	repaired, err := e.reconcile(ctx, run.key, work, run.conn)
	if err != nil {
		return err
	}

	run.result.Repaired = repaired

	if repaired {
		summary.Repaired++

		// Clearing a partial state can complete the work outright: dropping an
		// invalid index left by an interrupted DROP INDEX CONCURRENTLY is the
		// whole of that step.
		done, err := work.Satisfied(ctx, run.conn)
		if err != nil {
			return e.fail(ctx, run.key, fmt.Errorf("check step %s after repair: %w", run.key, err))
		}

		if done {
			return e.skip(ctx, run, summary)
		}
	}

	attempts, err := e.beginAttempt(ctx, run.key)
	if err != nil {
		return err
	}

	run.result.Attempts = attempts

	if err := e.apply(ctx, run.key, work, run.conn); err != nil {
		return err
	}

	run.result.Status = string(ledger.StatusSucceeded)
	summary.apply(run.result, run.started)

	return e.succeed(ctx, run.key)
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

	e.opts.Log.Info("repairing partial step",
		"step", key.String(), "kind", string(work.Meta().Kind), "attempts", recorded.Attempts)

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

// apply runs a non-transactional step and verifies that it did what it claimed.
func (e *Executor) apply(ctx context.Context, key ledger.StepKey, work step.NoTxStep, conn *sql.Conn) error {
	crash.At(crash.BeforeApply)

	run := func(ctx context.Context) error {
		return work.Apply(ctx, conn)
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

// runResumableStep applies a step that records its progress as it goes.
//
// It is handed a way to open a fenced transaction on the work pool and a way to
// write its checkpoint into that same transaction, so a batch cannot commit
// without the cursor covering it.
func (e *Executor) runResumableStep(ctx context.Context, run *stepRun,
	work step.ResumableStep, summary *Summary) error {
	attempts, err := e.beginAttempt(ctx, run.key)
	if err != nil {
		return err
	}

	run.result.Attempts = attempts

	env := step.Env{
		Begin: func(ctx context.Context) (*sql.Tx, error) {
			return e.beginFenced(ctx)
		},
		Checkpoint: func(ctx context.Context, tx *sql.Tx, state []byte) error {
			return ledger.SetCheckpoint(ctx, tx, run.key, state)
		},
		Load: func(ctx context.Context) ([]byte, error) {
			return ledger.LoadCheckpoint(ctx, e.control, run.key)
		},
		Read: func(ctx context.Context, query string, args ...any) *sql.Row {
			return e.work.QueryRowContext(ctx, query, args...)
		},
		Retry: func(ctx context.Context, batch func(context.Context) error) error {
			return session.WithLockRetry(ctx, e.opts.Retry, batch)
		},
		Throttle: throttle.New(throttle.Config{
			Batch:       run.spec.Backfill.Batch,
			MaxLagBytes: run.spec.Backfill.MaxLagBytes,
			Lag:         throttle.Replication(e.control),
		}),
		Report: func(cursor step.Cursor) {
			e.opts.Log.Info("backfill progress",
				"step", run.key.String(), "cursor", cursor.Position, "rows", cursor.Rows)
		},
		Resume: func(cursor step.Cursor) {
			e.opts.Log.Info("backfill resuming",
				"step", run.key.String(), "cursor", cursor.Position, "rows", cursor.Rows)
		},
	}

	if err := work.Run(ctx, env); err != nil {
		return e.fail(ctx, run.key, err)
	}

	// The postcondition. A backfill that walked its whole key range and left
	// rows behind has not done its job, and recording it as done would hide
	// them.
	done, err := work.Satisfied(ctx, run.conn)
	if err != nil {
		return e.fail(ctx, run.key, fmt.Errorf("check step %s after backfill: %w", run.key, err))
	}

	if !done {
		return e.fail(ctx, run.key, fmt.Errorf("step %s: %w", run.key, ErrPostconditionFailed))
	}

	run.result.Status = string(ledger.StatusSucceeded)
	summary.apply(run.result, run.started)

	return e.succeed(ctx, run.key)
}

// setLocalLockTimeout bounds lock waits for one transaction.
//
// Batches run on the work pool rather than on a pinned connection, so the
// timeout is set on the transaction instead of the session. Without it a batch
// blocked on a row lock waits forever, and because Postgres orders lock
// requests, every later query wanting that table queues behind it.
const setLocalLockTimeout = "SELECT set_config('lock_timeout', $1, true)"

// beginFenced opens a transaction on the work pool with the lease fence already
// asserted, so a batch cannot be written by a runner that has been superseded.
func (e *Executor) beginFenced(ctx context.Context) (*sql.Tx, error) {
	tx, err := e.work.BeginTx(ctx, ledger.TxOptions())
	if err != nil {
		return nil, fmt.Errorf("begin batch: %w", err)
	}

	if err := e.prepareBatch(ctx, tx); err != nil {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			return nil, errors.Join(err, fmt.Errorf("roll back batch: %w", rollbackErr))
		}

		return nil, err
	}

	return tx, nil
}

// prepareBatch bounds the transaction's lock waits and asserts the fence.
//
// The timeout comes first so that it covers the fence check too: asserting the
// fence takes the lease row, and an acquisition in flight would otherwise block
// the batch indefinitely.
func (e *Executor) prepareBatch(ctx context.Context, tx *sql.Tx) error {
	timeout := strconv.FormatInt(session.DefaultLockTimeout.Milliseconds(), 10)

	if _, err := tx.ExecContext(ctx, setLocalLockTimeout, timeout); err != nil {
		return fmt.Errorf("set lock timeout for batch: %w", err)
	}

	return ledger.Guard(ctx, tx, e.opts.Fence)
}
