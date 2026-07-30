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

package mig

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"log/slog"
	"time"

	"github.com/tochemey/mig/internal/crash"
	"github.com/tochemey/mig/internal/exec"
	"github.com/tochemey/mig/internal/lease"
	"github.com/tochemey/mig/internal/ledger"
	"github.com/tochemey/mig/internal/plan"
)

var (
	// ErrLocked reports another runner holding the lease.
	ErrLocked = lease.ErrLocked

	// ErrFenced reports a write rejected because the lease has moved on.
	ErrFenced = ledger.ErrFenced

	// ErrChecksumDrift reports a migration edited after it was applied.
	ErrChecksumDrift = exec.ErrChecksumDrift
)

// OnLocked says what to do when another runner holds the lease.
type OnLocked = lease.OnLocked

const (
	// Wait blocks until the lease is free, which is what a deployment wants.
	Wait = lease.Wait

	// Fail returns [ErrLocked] at once, which is what a probe wants.
	Fail = lease.Fail
)

// DefaultTTL is how long a lease stays valid when [Options] does not say.
const DefaultTTL = lease.DefaultTTL

// Summary is what a run did, step by step.
type Summary = exec.Summary

// Options configure a run. The zero value is usable.
type Options struct {
	// Work is the pool batch traffic runs on. It defaults to the pool passed
	// to the call.
	//
	// Giving a backfill its own pool is what keeps it from starving the
	// heartbeat that holds the lease, which is how a lease gets lost under
	// exactly the load it exists to survive.
	Work *sql.DB

	// TTL is how long the lease stays valid without a heartbeat. Zero means
	// [DefaultTTL].
	TTL time.Duration

	// OnLocked says what to do when another runner holds the lease. Zero means
	// [Wait], which is what a deployment wants.
	OnLocked OnLocked

	// AllowDrift downgrades a checksum mismatch on an applied step to a
	// warning.
	AllowDrift bool

	// Version identifies this build in application_name.
	Version string

	// Log receives one record per step transition.
	Log *slog.Logger
}

// Up applies every outstanding step in fsys, in order, under the lease.
//
// The summary is returned whether or not the run succeeded: it says how far the
// run got, which is what the next run has to reconcile against.
func Up(ctx context.Context, db *sql.DB, fsys fs.FS, opts Options) (Summary, error) {
	loaded, err := plan.LoadFS(fsys)
	if err != nil {
		return Summary{}, err
	}

	work := opts.Work
	if work == nil {
		work = db
	}

	var summary Summary

	err = underLease(ctx, db, opts, func(ctx context.Context, fence ledger.Fence) error {
		var runErr error

		summary, runErr = exec.New(db, work, exec.Options{
			Fence:      fence,
			AllowDrift: opts.AllowDrift,
			Version:    opts.Version,
			Log:        opts.Log,
		}).Run(ctx, loaded)

		return runErr
	})

	return summary, err
}

// underLease runs fn holding the lease, and hands it back afterwards.
//
// The lease is released whether or not fn succeeded. A failed run that kept it
// would lock every later run out until the TTL lapsed, which turns one broken
// migration into a window in which nothing can be applied.
func underLease(ctx context.Context, db *sql.DB, opts Options,
	fn func(context.Context, ledger.Fence) error) error {
	if err := ledger.EnsureSchema(ctx, db); err != nil {
		return err
	}

	ttl := opts.TTL
	if ttl == 0 {
		ttl = lease.DefaultTTL
	}

	crash.At(crash.BeforeLeaseAcquire)

	held, err := lease.Acquire(ctx, db, lease.Config{
		Owner:    lease.NewOwner(),
		TTL:      ttl,
		OnLocked: opts.OnLocked,
	})
	if err != nil {
		return err
	}

	crash.At(crash.AfterLeaseAcquire)

	// Losing the lease cancels the work rather than merely being logged.
	running, stop := held.Keepalive(ctx)
	defer stop()

	runErr := fn(running, held.Fence())

	crash.At(crash.BeforeLeaseRelease)

	// A context of its own, so that a run cancelled part way through still
	// hands the lease back instead of leaving it to expire.
	//
	// Bounded by the TTL, because the reason a run ends is sometimes that the
	// database became unreachable. Waiting longer than the lease itself lasts
	// achieves nothing: by then it has expired and a successor may take it.
	// Without the bound the process hangs on a socket nobody will answer.
	releaseCtx, cancelRelease := context.WithTimeout(context.WithoutCancel(ctx), ttl)
	defer cancelRelease()

	releaseErr := held.Release(releaseCtx)

	return errors.Join(runErr, releaseErr)
}
