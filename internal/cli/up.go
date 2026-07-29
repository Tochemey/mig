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

package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver

	"github.com/tochemey/mig/internal/crash"
	"github.com/tochemey/mig/internal/exec"
	"github.com/tochemey/mig/internal/lease"
	"github.com/tochemey/mig/internal/ledger"
	"github.com/tochemey/mig/internal/plan"
	"github.com/tochemey/mig/internal/session"
)

// controlConns caps the control pool. Ledger and lease traffic stay on their
// own pool so a saturated work pool cannot starve the heartbeat.
const controlConns = 2

// up applies every pending step, converging on the plan.
func up(ctx context.Context, args []string, stdout, stderr io.Writer) (code int, err error) {
	var cfg config

	flags := flag.NewFlagSet("up", flag.ContinueOnError)
	flags.SetOutput(stderr)
	bind(flags, &cfg)

	if err := flags.Parse(args); err != nil {
		return ExitError, err
	}

	if cfg.dsn == "" {
		return ExitError, fmt.Errorf("no connection string: pass --dsn or set %s", EnvDSN)
	}

	log := logger(stderr, cfg.verbose)

	loaded, err := plan.Load(cfg.dir)
	if err != nil {
		return ExitError, err
	}

	control, err := open(ctx, cfg.dsn)
	if err != nil {
		return ExitError, err
	}

	// A pool that will not close cleanly is reported, and turns an otherwise
	// successful run into a failure. Re-running is safe by construction, so
	// failing loudly costs a repeat of a converged run; staying quiet hides a
	// connection that never went away.
	defer func() {
		if closeErr := closePool(control); closeErr != nil {
			err = errors.Join(err, closeErr)

			if code == ExitOK {
				code = ExitError
			}
		}
	}()

	return apply(ctx, control, cfg, loaded, log, stdout)
}

// open connects, and refuses a pooler that would discard session state.
func open(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	db.SetMaxOpenConns(controlConns)

	if err := probe(ctx, db); err != nil {
		return nil, errors.Join(err, closePool(db))
	}

	return db, nil
}

// probe checks the connection works and is not behind a transaction pooler.
func probe(ctx context.Context, db *sql.DB) (err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("return probe connection: %w", closeErr))
		}
	}()

	return session.DetectPooling(ctx, conn)
}

// closePool closes the control pool.
func closePool(db *sql.DB) error {
	if err := db.Close(); err != nil {
		return fmt.Errorf("close control pool: %w", err)
	}

	return nil
}

// apply takes the lease and runs the plan under it.
func apply(ctx context.Context, control *sql.DB, cfg config,
	loaded *plan.Plan, log *slog.Logger, stdout io.Writer) (int, error) {
	if err := ledger.EnsureSchema(ctx, control); err != nil {
		return ExitError, err
	}

	crash.At(crash.BeforeLeaseAcquire)

	held, err := lease.Acquire(ctx, control, lease.Config{
		Owner:    lease.NewOwner(),
		TTL:      cfg.ttl,
		OnLocked: lease.OnLocked(cfg.onLocked),
	})
	if err != nil {
		if errors.Is(err, lease.ErrLocked) {
			log.Info("another runner holds the lease")
			return ExitLocked, nil
		}

		return ExitError, err
	}

	crash.At(crash.AfterLeaseAcquire)

	// Losing the lease cancels the work rather than merely being logged.
	work, stop := held.Keepalive(ctx)
	defer stop()

	executor := exec.New(control, exec.Options{
		Fence:      held.Fence(),
		AllowDrift: cfg.drift,
		Version:    Version,
		Log:        log,
	})

	summary, runErr := executor.Run(work, loaded)

	// The summary is emitted even when the run failed: it says how far the run
	// got, which is what the next run has to reconcile against.
	if err := emit(stdout, summary); err != nil {
		return ExitError, err
	}

	if runErr != nil {
		return ExitError, runErr
	}

	crash.At(crash.BeforeLeaseRelease)

	if err := held.Release(context.WithoutCancel(ctx)); err != nil {
		return ExitError, err
	}

	return ExitOK, nil
}

// emit writes the machine-readable summary to stdout.
func emit(stdout io.Writer, summary exec.Summary) error {
	encoded, err := json.Marshal(summary)
	if err != nil {
		return fmt.Errorf("encode summary: %w", err)
	}

	if _, err := fmt.Fprintln(stdout, string(encoded)); err != nil {
		return fmt.Errorf("write summary: %w", err)
	}

	return nil
}

// logger builds the structured logger. Logs go to stderr so that stdout
// carries only the summary.
func logger(stderr io.Writer, verbose bool) *slog.Logger {
	level := slog.LevelWarn
	if verbose {
		level = slog.LevelInfo
	}

	return slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: level}))
}
