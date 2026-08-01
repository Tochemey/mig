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

package verify

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"time"
)

// Applier runs the migration being measured. It is the executor in every real
// run; a test supplies its own to measure something it controls.
type Applier func(ctx context.Context, db *sql.DB, migrations fs.FS) error

// Options is one verification.
type Options struct {
	// Config is the workload.
	Config Config

	// Migrations is the directory holding the migration under test.
	Migrations fs.FS

	// Budget is what the run may cost. An empty budget measures and reports
	// without failing anything.
	Budget Budget

	// Apply runs the migration. Nil means the executor.
	Apply Applier
}

// Report is what one verification measured.
type Report struct {
	// Baseline is the workload with nothing happening to the database;
	// During covers the migration and the settling window after it.
	Baseline Window
	During   Window

	// Migration is how long the migration itself took, and Applied says
	// whether it got through at all.
	//
	// A migration the executor could not apply under this traffic is a
	// verdict rather than an error: refusing to queue behind a long read is
	// what the lock timeout is for.
	Migration  time.Duration
	Applied    bool
	ApplyError string

	// Waits is what the server was waiting on, loudest first, and Blocked
	// how many readings caught a backend waiting on a relation lock.
	Waits   []Wait
	Blocked int
	Longest Block

	// Unsampled counts readings the sampler could not take, so a quiet
	// attribution cannot pass for a quiet server.
	Unsampled int

	// Violations are the budget terms the run broke.
	Violations []Violation
}

// Failed reports whether the run broke its budget or could not be applied at
// all, which is what the command exits non-zero on.
func (r Report) Failed() bool {
	return len(r.Violations) > 0 || !r.Applied
}

// Run measures a migration against a workload.
//
// The two pools are not interchangeable and must not be the same one: a pool
// exhausted by the workload looks exactly like lock contention, and the
// harness would report the migration for something the client did to itself.
// work carries the traffic; control seeds the schema and applies the
// migration.
func Run(ctx context.Context, control, work *sql.DB, opts Options) (Report, error) {
	apply := opts.Apply
	if apply == nil {
		return Report{}, errors.New("verify: no applier: the caller supplies the executor")
	}

	if err := setup(ctx, control, opts.Config.Setup); err != nil {
		return Report{}, err
	}

	sampler := Watch(ctx, control)
	workload := Start(ctx, work, opts.Config)

	// The traffic stops before this returns on every path, including the
	// cancelled ones. Stopping twice is a no-op; not stopping at all leaves
	// queries in flight against pools the caller is free to close.
	defer workload.Stop()
	defer sampler.Stop()

	// The baseline: what this workload costs when nothing is happening to
	// the database. Everything after it is measured against this.
	if err := wait(ctx, opts.Config.Baseline); err != nil {
		return Report{}, err
	}

	report := Report{Baseline: workload.Take()}

	started := time.Now()
	applyErr := apply(ctx, control, opts.Migrations)
	report.Migration = time.Since(started)
	report.Applied = applyErr == nil

	if applyErr != nil {
		report.ApplyError = applyErr.Error()
	}

	// The settling window runs whether or not the migration failed: a
	// migration that fell over half way still left the database in whatever
	// state it left it in, and the traffic is still living with it.
	if err := wait(ctx, opts.Config.Settle); err != nil {
		return Report{}, err
	}

	report.During = workload.Stop()
	report.Waits, report.Blocked, report.Longest, report.Unsampled = sampler.Stop()
	report.Violations = opts.Budget.Exceeded(report.During, report.Longest.For)

	return report, nil
}

// setup builds the schema and rows the migration will run against.
func setup(ctx context.Context, db *sql.DB, statements []string) error {
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("workload setup %q: %w", statement, err)
		}
	}

	return nil
}

// wait sleeps for a window, or returns what stopped it.
func wait(ctx context.Context, window time.Duration) error {
	timer := time.NewTimer(window)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
