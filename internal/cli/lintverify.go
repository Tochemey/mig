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
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tochemey/mig/pkg/mig"
)

// The pools a chaos run needs. They are separate on purpose: a pool the
// workload has exhausted looks exactly like lock contention, and the run
// would report the migration for what the client did to itself.
const (
	chaosWorkConns    = 16
	chaosControlConns = 4

	// adminConns is what creating and dropping one database needs.
	adminConns = 1
)

// scratchPrefix names the database a run builds for itself. It is created and
// dropped by the command, so nothing anybody owns is ever migrated.
const scratchPrefix = "mig_verify_"

// newLintVerifyCommand builds the command that measures a migration under
// load instead of predicting what it will do.
func newLintVerifyCommand() *cobra.Command {
	var (
		dir      string
		dsn      string
		workload string
		budget   string
		keep     bool
	)

	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Run the migrations under a workload and measure what they cost",
		Long: "Apply the migrations to a database of this command's own making, under\n" +
			"traffic from a workload file, and report what it did to that traffic:\n" +
			"latency before and during, what the server spent the time waiting on, and\n" +
			"the longest any statement was blocked.\n\n" +
			"The --dsn names a server, not a database to migrate. A throwaway database\n" +
			"is created on it, used, and dropped, so nothing anybody owns is touched.\n" +
			"Point it at a scratch Postgres: a CI service container, or one started for\n" +
			"the occasion.\n\n" +
			"With --budget the exit code is non-zero when the run exceeds it, which is\n" +
			"what makes this a CI gate rather than a report nobody reads.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLintVerify(cmd.Context(), lintVerifyConfig{
				dir: dir, dsn: dsn, workload: workload, budget: budget, keep: keep,
			}, cmd.OutOrStdout())
		},
	}

	cmd.Flags().StringVar(&dir, "dir", envOr(EnvDir, "migrations"), "directory holding migration files")
	cmd.Flags().StringVar(&dsn, "dsn", os.Getenv(EnvDSN),
		"a server to build the throwaway database on, not a database to migrate")
	cmd.Flags().StringVar(&workload, "workload", "", "workload file describing the traffic to run")
	cmd.Flags().StringVar(&budget, "budget", "",
		"what the run may cost, as p99=50ms,max_block=2s")
	cmd.Flags().BoolVar(&keep, "keep", false,
		"leave the throwaway database behind, to look at what the migration left")

	return cmd
}

// lintVerifyConfig is what the command was asked to do.
type lintVerifyConfig struct {
	dir      string
	dsn      string
	workload string
	budget   string
	keep     bool
}

// runLintVerify builds the throwaway database, measures the run and reports.
func runLintVerify(ctx context.Context, cfg lintVerifyConfig, stdout io.Writer) (err error) {
	if cfg.dsn == "" {
		return fmt.Errorf("no connection string: pass --dsn or set %s", EnvDSN)
	}

	if cfg.workload == "" {
		return errors.New("no workload: pass --workload with the traffic to run")
	}

	source, err := os.ReadFile(cfg.workload)
	if err != nil {
		return fmt.Errorf("read workload: %w", err)
	}

	workload, err := mig.ParseWorkload(source)
	if err != nil {
		return err
	}

	budget, err := mig.ParseBudget(cfg.budget)
	if err != nil {
		return err
	}

	scratch, drop, err := scratchDatabase(ctx, cfg.dsn, cfg.keep)
	if err != nil {
		return err
	}

	defer func() {
		err = errors.Join(err, drop())
	}()

	control, err := open(ctx, scratch, chaosControlConns)
	if err != nil {
		return err
	}

	defer func() {
		err = errors.Join(err, closePool(control))
	}()

	work, err := open(ctx, scratch, chaosWorkConns)
	if err != nil {
		return err
	}

	defer func() {
		err = errors.Join(err, closePool(work))
	}()

	report, runErr := mig.Chaos(ctx, control, work, os.DirFS(cfg.dir), workload, budget)

	if err := mig.RenderChaos(stdout, report); err != nil {
		return errors.Join(runErr, err)
	}

	if runErr != nil {
		return runErr
	}

	if report.Failed() {
		return fmt.Errorf("the run exceeded its budget")
	}

	return nil
}

// scratchDatabase creates a database of this run's own on the named server
// and returns its connection string beside the call that removes it.
func scratchDatabase(ctx context.Context, dsn string, keep bool) (string, func() error, error) {
	admin, err := open(ctx, dsn, adminConns)
	if err != nil {
		return "", nil, err
	}

	// The name only has to be unlikely to collide with another run on the
	// same server, which is not a security property.
	//nolint:gosec // G404: a scratch database name needs no cryptographic randomness.
	name := fmt.Sprintf("%s%d", scratchPrefix, rand.Uint32())

	//nolint:gosec // G201: a database name cannot be bound as a parameter; the name is this command's own.
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		return "", nil, errors.Join(fmt.Errorf("create the throwaway database: %w", err), closePool(admin))
	}

	scratch, err := replaceDatabase(dsn, name)
	if err != nil {
		return "", nil, errors.Join(err, closePool(admin))
	}

	drop := func() error {
		if keep {
			return closePool(admin)
		}

		//nolint:gosec // G201: as above, and the name is this command's own.
		_, err := admin.ExecContext(context.WithoutCancel(ctx), `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`)
		if err != nil {
			err = fmt.Errorf("drop the throwaway database %q: %w", name, err)
		}

		return errors.Join(err, closePool(admin))
	}

	return scratch, drop, nil
}

// replaceDatabase points a connection string at another database on the same
// server.
func replaceDatabase(dsn, name string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse dsn: %w", err)
	}

	if !strings.HasPrefix(parsed.Scheme, "postgres") {
		return "", fmt.Errorf("dsn %q is not a postgres URL: verify builds its own database and "+
			"needs a server to build it on", dsn)
	}

	parsed.Path = "/" + name

	return parsed.String(), nil
}
