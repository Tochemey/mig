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

// Package exec applies a plan.
//
// Two properties must survive any refactor of this package.
//
// The catalog is consulted before anything else, unconditionally — not when the
// ledger looks suspicious. Catalog queries are cheap and they are the entire
// safety story: a step whose work is already present is skipped no matter what
// the ledger claims.
//
// A step that cannot commit its ledger row with its work is reconciled rather
// than trusted. The window between the work landing and the record of it is
// real and cannot be closed, so it is recovered from instead.
package exec

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/tochemey/mig/internal/ledger"
	"github.com/tochemey/mig/internal/plan"
	"github.com/tochemey/mig/internal/session"
	"github.com/tochemey/mig/internal/step"
)

var (
	// ErrPostconditionFailed reports a step that ran without error and left the
	// catalog unchanged. Recording it as succeeded would make the next run skip
	// work that never happened.
	ErrPostconditionFailed = errors.New("step reported success but its predicate is still unsatisfied")

	// ErrChecksumDrift reports a migration edited after it was applied.
	ErrChecksumDrift = errors.New("checksum drift: an already-applied step has been edited")
)

// Options configure a run.
type Options struct {
	// Fence guards every ledger write.
	Fence ledger.Fence

	// AllowDrift downgrades a checksum mismatch on an applied step to a warning.
	AllowDrift bool

	// Retry bounds waiting for locks.
	Retry session.Retry

	// Version identifies this build in application_name.
	Version string

	// Log receives one record per step transition.
	Log *slog.Logger
}

// Executor applies migrations.
type Executor struct {
	control *sql.DB
	work    *sql.DB
	opts    Options
}

// New builds an executor over the control and work pools.
//
// Batch traffic is kept off the control pool so that a backfill saturating its
// connections cannot starve the heartbeat, which is how a lease gets lost under
// exactly the load it exists to survive.
func New(control, work *sql.DB, opts Options) *Executor {
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}

	if opts.Retry.Attempts == 0 {
		opts.Retry = session.DefaultRetry()
	}

	return &Executor{control: control, work: work, opts: opts}
}

// Run applies every pending step of every migration, in order.
func (e *Executor) Run(ctx context.Context, p *plan.Plan) (Summary, error) {
	started := time.Now()
	summary := Summary{}

	// Everything is built before anything is applied. A step that cannot be
	// executed — a non-transactional one with no way to recognise its own
	// finished work — has to stop the run while the database is still whole.
	// Discovering it half way through would leave exactly the state this tool
	// exists to avoid: some migrations applied, one refused, and nobody able to
	// say what the database now is.
	built, err := build(p)
	if err != nil {
		return summary, err
	}

	if err := e.record(ctx, p); err != nil {
		return summary, err
	}

	prog := &progress{total: totalSteps(p)}

	for i, migration := range p.Migrations {
		summary.Migrations++

		if err := e.runMigration(ctx, migration, built[i], &summary, prog); err != nil {
			summary.finish(started)
			return summary, err
		}
	}

	summary.finish(started)

	return summary, nil
}

// build turns every step of every migration into its executable form, in the
// same order, and reports the first that cannot be built.
func build(p *plan.Plan) ([][]step.Step, error) {
	built := make([][]step.Step, len(p.Migrations))

	for i, migration := range p.Migrations {
		built[i] = make([]step.Step, len(migration.Steps))

		for j, spec := range migration.Steps {
			work, err := spec.Build()
			if err != nil {
				return nil, fmt.Errorf("migration %q: %w", migration.ID, err)
			}

			built[i][j] = work
		}
	}

	return built, nil
}

// record writes the pending rows for everything in the plan, and checks the
// recorded checksums against the files on disk.
func (e *Executor) record(ctx context.Context, p *plan.Plan) error {
	for _, migration := range p.Migrations {
		if err := e.checkDrift(ctx, migration); err != nil {
			return err
		}

		write := func(ctx context.Context, tx *sql.Tx) error {
			if err := ledger.UpsertMigration(ctx, tx, migration.ID, migration.Name); err != nil {
				return err
			}

			for _, s := range migration.Steps {
				key := ledger.StepKey{MigrationID: migration.ID, Index: s.Index}

				if err := ledger.UpsertStep(ctx, tx, key, s.Name, string(s.Kind), s.Checksum); err != nil {
					return err
				}
			}

			return nil
		}

		if err := ledger.Write(ctx, e.control, e.opts.Fence, write); err != nil {
			return fmt.Errorf("record migration %q: %w", migration.ID, err)
		}
	}

	return nil
}

// checkDrift compares recorded checksums against the files on disk.
//
// Only steps already recorded as succeeded matter: editing a step that has not
// run yet is ordinary work, while editing one that has means the database and
// the repository no longer agree about what was applied.
func (e *Executor) checkDrift(ctx context.Context, migration plan.Migration) error {
	for _, s := range migration.Steps {
		key := ledger.StepKey{MigrationID: migration.ID, Index: s.Index}

		recorded, err := ledger.LoadStep(ctx, e.control, key)
		if errors.Is(err, ledger.ErrNotRecorded) {
			continue
		}

		if err != nil {
			return err
		}

		if recorded.Status != ledger.StatusSucceeded || bytes.Equal(recorded.Checksum, s.Checksum) {
			continue
		}

		if !e.opts.AllowDrift {
			return fmt.Errorf("%w: step %s (%q)", ErrChecksumDrift, key, s.Name)
		}

		e.opts.Log.Warn("checksum drift allowed", "step", key.String(), "name", s.Name)
	}

	return nil
}

// runMigration applies every step of one migration.
func (e *Executor) runMigration(ctx context.Context, migration plan.Migration,
	built []step.Step, summary *Summary, prog *progress) error {
	for i, spec := range migration.Steps {
		if err := e.runStep(ctx, migration, spec, built[i], summary, prog); err != nil {
			return err
		}
	}

	return e.setMigrationStatus(ctx, migration.ID, ledger.StatusSucceeded)
}

// progress numbers the steps of a run, across every migration in the plan, so
// a step can be reported as the n-th of the whole rather than of its file.
type progress struct {
	position int
	total    int
}

// totalSteps counts what the plan will run, for the denominator.
func totalSteps(p *plan.Plan) int {
	total := 0
	for _, migration := range p.Migrations {
		total += len(migration.Steps)
	}

	return total
}

// setMigrationStatus records a migration's outcome.
func (e *Executor) setMigrationStatus(ctx context.Context, id string, status ledger.Status) error {
	write := func(ctx context.Context, tx *sql.Tx) error {
		return ledger.SetMigrationStatus(ctx, tx, id, status)
	}

	if err := ledger.Write(ctx, e.control, e.opts.Fence, write); err != nil {
		return fmt.Errorf("mark migration %q %s: %w", id, status, err)
	}

	return nil
}
