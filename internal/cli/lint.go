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
	"io/fs"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/tochemey/mig/internal/lint/report"
	"github.com/tochemey/mig/pkg/mig"
)

// The lint output formats.
const (
	formatHuman  = "human"
	formatJSON   = "json"
	formatSARIF  = "sarif"
	formatGitHub = "github"
)

// formats is what --format accepts, for the flag's help and the error a
// misspelling gets.
const formats = formatHuman + ", " + formatJSON + ", " + formatSARIF + " or " + formatGitHub

// The flags read back by name, to tell a value the caller set from the
// default that stands in for it.
const (
	flagTargetVersion = "target-version"
	flagPolicy        = "policy"
)

// lintConns is the pool connected mode needs: the catalog reads and the
// calibration probe run one after another on one connection.
const lintConns = 1

// newLintCommand builds the command that checks migrations for lock hazards.
func newLintCommand() *cobra.Command {
	var (
		dir          string
		dsn          string
		format       string
		policyPath   string
		version      int
		fix          bool
		yes          bool
		suppressions bool
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
			opts := lintOptions{
				dir:          dir,
				dsn:          dsn,
				format:       format,
				policy:       policyPath,
				version:      version,
				versionNamed: cmd.Flags().Changed(flagTargetVersion),
				policyNamed:  cmd.Flags().Changed(flagPolicy),
				suppressions: suppressions,
			}

			if fix {
				return runLintFix(cmd.Context(), opts, yes, cmd.OutOrStdout(), cmd.InOrStdin())
			}

			return runLint(cmd.Context(), opts, cmd.OutOrStdout())
		},
	}

	cmd.Flags().StringVar(&dir, "dir", envOr(EnvDir, "migrations"), "directory holding migration files")
	cmd.Flags().StringVar(&dsn, "dsn", os.Getenv(EnvDSN),
		"read table sizes from this database, and grade the findings by them")
	cmd.Flags().StringVar(&format, "format", formatHuman, "output format: "+formats)
	cmd.Flags().StringVar(&policyPath, flagPolicy, mig.PolicyFileName,
		"policy file assigning severities, thresholds and a target version")
	cmd.Flags().IntVar(&version, flagTargetVersion, mig.DefaultTargetVersion,
		"Postgres major version the migrations are written for, ignored with --dsn")
	cmd.Flags().BoolVar(&fix, "fix", false, "rewrite flagged statements as safe steps, after showing the diff")
	cmd.Flags().BoolVar(&yes, "yes", false, "apply fixes without asking, for CI")
	cmd.Flags().BoolVar(&suppressions, "report-suppressions", false,
		"list every lint:ignore directive with its age, so they can be audited")

	cmd.AddCommand(newLintVerifyCommand())

	return cmd
}

// lintOptions is what a lint run needs. The two "named" fields record whether
// the caller set the flag or took its default, which is what decides between
// the flag and the policy file.
type lintOptions struct {
	dir          string
	dsn          string
	format       string
	policy       string
	version      int
	versionNamed bool
	policyNamed  bool
	suppressions bool
}

// runLintFix shows the rewrites as a diff and applies them once confirmed.
//
// It reads the catalog when a database was named, exactly as a report does: a
// generated backfill pages by the table's real primary key rather than by an
// assumption, and the fix is the one output that ends up in the author's file.
func runLintFix(ctx context.Context, opts lintOptions, yes bool,
	stdout io.Writer, stdin io.Reader) error {
	if opts.format != formatHuman {
		return fmt.Errorf("--fix renders a diff and takes no --format")
	}

	if opts.suppressions {
		return fmt.Errorf("--fix rewrites files and reports no suppressions")
	}

	linted, err := lintReport(ctx, opts)
	if err != nil {
		return err
	}

	return applyFixes(opts.dir, linted, yes, stdout, stdin)
}

// runLint checks the directory and renders what it found.
func runLint(ctx context.Context, opts lintOptions, stdout io.Writer) error {
	if opts.suppressions && opts.format != formatHuman {
		return fmt.Errorf("--report-suppressions renders with --format %s", formatHuman)
	}

	linted, err := lintReport(ctx, opts)
	if err != nil {
		return err
	}

	if err := renderFindings(opts, linted, stdout); err != nil {
		return err
	}

	// A reader told to expect estimates is owed the reason there are none.
	if linted.Uncalibrated != "" && opts.format == formatHuman {
		if _, err := fmt.Fprintf(stdout,
			"the calibration probe reported: %s\n", linted.Uncalibrated); err != nil {
			return fmt.Errorf("write report: %w", err)
		}
	}

	if opts.suppressions {
		if err := report.Suppressions(stdout, linted.Suppressions, time.Now()); err != nil {
			return err
		}
	}

	if errors := linted.Errors(); errors > 0 {
		return fmt.Errorf("lint found %d error(s)", errors)
	}

	return nil
}

// renderFindings writes the findings in the format that was asked for.
func renderFindings(opts lintOptions, linted *mig.LintReport, stdout io.Writer) error {
	switch opts.format {
	case formatHuman:
		return report.Human(stdout, linted.Findings, linted.Sources)

	case formatJSON:
		return report.JSON(stdout, linted.Findings)

	case formatSARIF:
		// The migration directory is how a code-scanning UI turns a finding's
		// file into a line of the pull request.
		return report.SARIF(stdout, linted.Findings, linted.Sources, opts.dir, Version)

	case formatGitHub:
		return report.Markdown(stdout, linted.Findings)

	default:
		return fmt.Errorf("unknown format %q: use %s", opts.format, formats)
	}
}

// lintReport runs the offline catalog, or the connected one when a database
// was named.
func lintReport(ctx context.Context, opts lintOptions) (report *mig.LintReport, err error) {
	pol, err := loadPolicy(opts)
	if err != nil {
		return nil, err
	}

	if opts.dsn == "" {
		return mig.Lint(os.DirFS(opts.dir), targetVersion(opts, pol), pol)
	}

	db, err := open(ctx, opts.dsn, lintConns)
	if err != nil {
		return nil, err
	}

	defer func() {
		err = errors.Join(err, closePool(db))
	}()

	return mig.LintConnected(ctx, db, os.DirFS(opts.dir), pol)
}

// loadPolicy reads the policy file.
//
// The conventional one is optional, and a directory without it is linted by
// the catalog's own defaults. One the caller named and that is not there is a
// mistake worth stopping for: a run that silently ignores the policy it was
// pointed at reports the wrong severities.
func loadPolicy(opts lintOptions) (*mig.Policy, error) {
	loaded, err := mig.LoadPolicy(opts.policy, opts.dir)
	if errors.Is(err, fs.ErrNotExist) && !opts.policyNamed {
		return nil, nil
	}

	return loaded, err
}

// targetVersion picks the major to lint against: the one the caller named,
// then the policy's, then the default. Connected mode reads the server's and
// ignores all three.
func targetVersion(opts lintOptions, pol *mig.Policy) int {
	if !opts.versionNamed && pol.TargetVersion() != 0 {
		return pol.TargetVersion()
	}

	return opts.version
}
