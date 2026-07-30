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
	"fmt"
	"io"
	"log/slog"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver
	"github.com/spf13/cobra"

	"github.com/tochemey/mig/internal/session"
	"github.com/tochemey/mig/pkg/mig"
)

const (
	// controlConns caps the control pool, which carries ledger and lease
	// traffic only.
	controlConns = 2

	// workConns caps the pool batches run on. Steps run one at a time, so one
	// connection does the work and the second covers the bounds read.
	workConns = 2
)

// newUpCommand builds the command that applies pending steps.
func newUpCommand() *cobra.Command {
	var cfg config

	cmd := &cobra.Command{
		Use:   "up",
		Short: "Apply pending migrations, converging on the plan",
		Long: "Apply every pending step, in order, under a single lease.\n\n" +
			"A run that is killed at any point can simply be run again: each step is\n" +
			"judged against the catalog rather than against the ledger.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUp(cmd.Context(), cfg, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}

	bindApply(cmd, &cfg)

	return cmd
}

// runUp applies every pending step, converging on the plan.
func runUp(ctx context.Context, cfg config, stdout, stderr io.Writer) (err error) {
	if cfg.dsn == "" {
		return fmt.Errorf("no connection string: pass --dsn or set %s", EnvDSN)
	}

	log := logger(stderr, cfg.verbose)

	control, err := open(ctx, cfg.dsn, controlConns)
	if err != nil {
		return err
	}

	// A pool that will not close cleanly is reported, and turns an otherwise
	// successful run into a failure. Re-running is safe by construction, so
	// failing loudly costs a repeat of a converged run; staying quiet hides a
	// connection that never went away.
	defer func() {
		err = errors.Join(err, closePool(control))
	}()

	// Batches run on their own pool, so that a backfill holding connections
	// cannot starve the heartbeat that keeps its lease alive.
	work, err := open(ctx, cfg.dsn, workConns)
	if err != nil {
		return err
	}

	defer func() {
		err = errors.Join(err, closePool(work))
	}()

	summary, runErr := mig.Up(ctx, control, os.DirFS(cfg.dir), mig.Options{
		Work:       work,
		TTL:        cfg.ttl,
		OnLocked:   mig.OnLocked(cfg.onLocked),
		AllowDrift: cfg.drift,
		Version:    Version,
		Log:        log,
	})

	if errors.Is(runErr, mig.ErrLocked) {
		runErr = exitError{code: ExitLocked, err: runErr}
	}

	// The summary is emitted even when the run failed: it says how far the run
	// got, which is what the next run has to reconcile against.
	return errors.Join(runErr, emit(stdout, summary))
}

// open connects, and refuses a pooler that would discard session state.
func open(ctx context.Context, dsn string, conns int) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	db.SetMaxOpenConns(conns)

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

// closePool closes a pool.
func closePool(db *sql.DB) error {
	if err := db.Close(); err != nil {
		return fmt.Errorf("close pool: %w", err)
	}

	return nil
}

// emit writes the machine-readable summary to stdout.
func emit(stdout io.Writer, summary mig.Summary) error {
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
