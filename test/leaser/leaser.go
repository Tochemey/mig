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

// Package leaser drives the lease and ledger packages from a cold start, so
// tests can race, freeze and kill actual processes.
//
// It is a fixture rather than a mock: everything it calls is production code.
// It stands in for the executor, which does not exist yet.
package leaser

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver

	"github.com/tochemey/mig/internal/crash"
	"github.com/tochemey/mig/internal/lease"
	"github.com/tochemey/mig/internal/ledger"
)

// Markers printed on stdout, which tests synchronise on rather than sleeping.
const (
	// MarkerAcquired is followed by the fence token.
	MarkerAcquired = "leaser: acquired fence="

	// MarkerClaimed reports the first fenced ledger write.
	MarkerClaimed = "leaser: claimed"

	// MarkerCommitted reports the final fenced ledger write.
	MarkerCommitted = "leaser: committed"

	// MarkerReleased reports a clean handback.
	MarkerReleased = "leaser: released"

	// MarkerLocked reports that another runner holds the lease.
	MarkerLocked = "leaser: locked"

	// MarkerFenced reports that a write was rejected by the fence.
	MarkerFenced = "leaser: fenced"
)

// Exit codes, distinct so that "another runner held the lease" and "this
// runner was superseded" can be told apart.
const (
	// ExitOK means the run completed and released the lease.
	ExitOK = 0

	// ExitError means something unrelated to leasing went wrong.
	ExitError = 1

	// ExitFenced means a ledger write was rejected: this runner was superseded.
	ExitFenced = 3

	// ExitLocked means the lease was held by another runner.
	ExitLocked = 4
)

// controlConns caps the control pool. Ledger and lease traffic stay on their
// own pool so a saturated work pool cannot starve the heartbeat.
const controlConns = 2

// Config parameterises [Run].
type Config struct {
	// DSN is the only state carried across a restart; everything else the
	// runner must rediscover.
	DSN string

	// MigrationID is the ledger row this run writes.
	MigrationID string

	// TTL is the lease validity window.
	TTL time.Duration

	// Hold is how long to keep the lease after claiming it, standing in for the
	// work the executor will do.
	Hold time.Duration

	// OnLocked selects waiting or failing when another runner holds the lease.
	OnLocked lease.OnLocked

	// WaitTimeout bounds waiting for a held lease.
	WaitTimeout time.Duration
}

// Run performs one leased run and returns the process exit code, reporting
// progress through out.
func Run(ctx context.Context, cfg Config, out func(string)) int {
	db, err := sql.Open("pgx", cfg.DSN)
	if err != nil {
		out(fmt.Sprintf("leaser: open: %v", err))
		return ExitError
	}

	db.SetMaxOpenConns(controlConns)

	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			out(fmt.Sprintf("leaser: close: %v", closeErr))
		}
	}()

	if err := ledger.EnsureSchema(ctx, db); err != nil {
		out(fmt.Sprintf("leaser: ensure schema: %v", err))
		return ExitError
	}

	crash.At(crash.BeforeLeaseAcquire)

	held, err := acquire(ctx, db, cfg)
	if err != nil {
		if errors.Is(err, lease.ErrLocked) {
			out(MarkerLocked)
			return ExitLocked
		}

		out(fmt.Sprintf("leaser: acquire: %v", err))

		return ExitError
	}

	crash.At(crash.AfterLeaseAcquire)
	out(fmt.Sprintf("%s%d", MarkerAcquired, held.Fence().Token))

	return hold(ctx, db, cfg, held, out)
}

// acquire takes the lease with the configured contention policy.
func acquire(ctx context.Context, db *sql.DB, cfg Config) (*lease.Lease, error) {
	return lease.Acquire(ctx, db, lease.Config{
		Owner:       lease.NewOwner(),
		TTL:         cfg.TTL,
		OnLocked:    cfg.OnLocked,
		WaitTimeout: cfg.WaitTimeout,
	})
}

// hold claims the ledger row, waits, then commits it.
func hold(ctx context.Context, db *sql.DB, cfg Config, held *lease.Lease, out func(string)) int {
	fence := held.Fence()

	work, stop := held.Keepalive(ctx)
	defer stop()

	claim := func(ctx context.Context, tx *sql.Tx) error {
		if err := ledger.UpsertMigration(ctx, tx, cfg.MigrationID, cfg.MigrationID); err != nil {
			return err
		}

		return ledger.SetMigrationStatus(ctx, tx, cfg.MigrationID, ledger.StatusRunning)
	}

	if code := write(work, db, fence, claim, MarkerClaimed, out); code != ExitOK {
		return code
	}

	// Interruptible: losing the lease stops the work rather than being logged.
	select {
	case <-work.Done():
	case <-time.After(cfg.Hold):
	}

	// Deliberately on a context the keepalive cannot cancel, standing in for a
	// runner frozen past its expiry that wakes believing it still owns the
	// lease. A working fence rejects this write.
	final := context.WithoutCancel(ctx)

	commit := func(ctx context.Context, tx *sql.Tx) error {
		return ledger.SetMigrationStatus(ctx, tx, cfg.MigrationID, ledger.StatusSucceeded)
	}

	if code := write(final, db, fence, commit, MarkerCommitted, out); code != ExitOK {
		return code
	}

	crash.At(crash.BeforeLeaseRelease)

	if err := held.Release(final); err != nil {
		out(fmt.Sprintf("leaser: release: %v", err))
		return ExitError
	}

	out(MarkerReleased)

	return ExitOK
}

// write performs one fenced ledger write and maps its outcome to an exit code.
func write(ctx context.Context, db *sql.DB, fence ledger.Fence,
	fn func(context.Context, *sql.Tx) error, marker string, out func(string)) int {
	err := ledger.Write(ctx, db, fence, fn)

	switch {
	case err == nil:
		out(marker)
		return ExitOK

	case errors.Is(err, ledger.ErrFenced):
		out(fmt.Sprintf("%s: %v", MarkerFenced, err))
		return ExitFenced

	default:
		out(fmt.Sprintf("leaser: write: %v", err))
		return ExitError
	}
}
