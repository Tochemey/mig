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
	"os"

	"github.com/spf13/cobra"

	"github.com/tochemey/mig/internal/lint/report"
	"github.com/tochemey/mig/pkg/mig"
)

// The lint output formats.
const (
	formatHuman = "human"
	formatJSON  = "json"
)

// lintConns is the pool connected mode needs: the catalog reads and the
// calibration probe run one after another on one connection.
const lintConns = 1

// newLintCommand builds the command that checks migrations for lock hazards.
func newLintCommand() *cobra.Command {
	var (
		dir     string
		dsn     string
		format  string
		version int
		fix     bool
		yes     bool
	)

	cmd := &cobra.Command{
		Use:   "lint",
		Short: "Report the locks each statement takes and the ones that will hurt",
		Long: "Check every migration against the lock hazard rules: statements that block\n" +
			"reads or writes for as long as the table is large, transaction misuse, and\n" +
			"validation work that could be deferred.\n\n" +
			"Without --dsn it touches no database, and a hazard whose cost is the size\n" +
			"of the table stays a warning, because nothing here knows that size. With\n" +
			"--dsn it reads the catalog: the target version is the server's, severity\n" +
			"follows the size of the table each finding names, and the findings carry\n" +
			"an estimate of how long the work will hold its lock. Point it at a\n" +
			"production-shaped replica, not at production.\n\n" +
			"Nothing is written either way. The exit code is non-zero only when a\n" +
			"finding is an error.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if fix {
				return runLintFix(cmd.Context(), dir, dsn, format, version, yes,
					cmd.OutOrStdout(), cmd.InOrStdin())
			}

			return runLint(cmd.Context(), dir, dsn, format, version, cmd.OutOrStdout())
		},
	}

	cmd.Flags().StringVar(&dir, "dir", envOr(EnvDir, "migrations"), "directory holding migration files")
	cmd.Flags().StringVar(&dsn, "dsn", os.Getenv(EnvDSN),
		"read table sizes from this database, and grade the findings by them")
	cmd.Flags().StringVar(&format, "format", formatHuman, "output format: human or json")
	cmd.Flags().IntVar(&version, "target-version", mig.DefaultTargetVersion,
		"Postgres major version the migrations are written for, ignored with --dsn")
	cmd.Flags().BoolVar(&fix, "fix", false, "rewrite flagged statements as safe steps, after showing the diff")
	cmd.Flags().BoolVar(&yes, "yes", false, "apply fixes without asking, for CI")

	cmd.AddCommand(newLintVerifyCommand())

	return cmd
}

// runLintFix shows the rewrites as a diff and applies them once confirmed.
//
// It reads the catalog when a database was named, exactly as a report does: a
// generated backfill pages by the table's real primary key rather than by an
// assumption, and the fix is the one output that ends up in the author's file.
func runLintFix(ctx context.Context, dir, dsn, format string, version int, yes bool,
	stdout io.Writer, stdin io.Reader) error {
	if format != formatHuman {
		return fmt.Errorf("--fix renders a diff and takes no --format")
	}

	linted, err := lintReport(ctx, dir, dsn, version)
	if err != nil {
		return err
	}

	return applyFixes(dir, linted, yes, stdout, stdin)
}

// runLint checks the directory and renders what it found.
func runLint(ctx context.Context, dir, dsn, format string, version int, stdout io.Writer) error {
	linted, err := lintReport(ctx, dir, dsn, version)
	if err != nil {
		return err
	}

	var rendered error

	switch format {
	case formatHuman:
		rendered = report.Human(stdout, linted.Findings, linted.Sources)
	case formatJSON:
		rendered = report.JSON(stdout, linted.Findings)
	default:
		return fmt.Errorf("unknown format %q: use human or json", format)
	}

	if rendered != nil {
		return rendered
	}

	// A reader told to expect estimates is owed the reason there are none.
	if linted.Uncalibrated != "" && format == formatHuman {
		if _, err := fmt.Fprintf(stdout,
			"the calibration probe reported: %s\n", linted.Uncalibrated); err != nil {
			return fmt.Errorf("write report: %w", err)
		}
	}

	if errors := linted.Errors(); errors > 0 {
		return fmt.Errorf("lint found %d error(s)", errors)
	}

	return nil
}

// lintReport runs the offline catalog, or the connected one when a database
// was named.
func lintReport(ctx context.Context, dir, dsn string, version int) (report *mig.LintReport, err error) {
	if dsn == "" {
		return mig.Lint(os.DirFS(dir), version)
	}

	db, err := open(ctx, dsn, lintConns)
	if err != nil {
		return nil, err
	}

	defer func() {
		err = errors.Join(err, closePool(db))
	}()

	return mig.LintConnected(ctx, db, os.DirFS(dir))
}
