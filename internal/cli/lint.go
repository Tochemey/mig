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

// newLintCommand builds the command that checks migrations for lock hazards.
func newLintCommand() *cobra.Command {
	var (
		dir     string
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
			"It touches no database and writes nothing. The exit code is non-zero only\n" +
			"when a finding is an error.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if fix {
				return runLintFix(dir, format, version, yes, cmd.OutOrStdout(), cmd.InOrStdin())
			}

			return runLint(dir, format, version, cmd.OutOrStdout())
		},
	}

	cmd.Flags().StringVar(&dir, "dir", envOr(EnvDir, "migrations"), "directory holding migration files")
	cmd.Flags().StringVar(&format, "format", formatHuman, "output format: human or json")
	cmd.Flags().IntVar(&version, "target-version", mig.DefaultTargetVersion,
		"Postgres major version the migrations are written for")
	cmd.Flags().BoolVar(&fix, "fix", false, "rewrite flagged statements as safe steps, after showing the diff")
	cmd.Flags().BoolVar(&yes, "yes", false, "apply fixes without asking, for CI")

	return cmd
}

// runLintFix shows the rewrites as a diff and applies them once confirmed.
func runLintFix(dir, format string, version int, yes bool, stdout io.Writer, stdin io.Reader) error {
	if format != formatHuman {
		return fmt.Errorf("--fix renders a diff and takes no --format")
	}

	linted, err := mig.Lint(os.DirFS(dir), version)
	if err != nil {
		return err
	}

	return applyFixes(dir, linted, yes, stdout, stdin)
}

// runLint checks the directory and renders what it found.
func runLint(dir, format string, version int, stdout io.Writer) error {
	linted, err := mig.Lint(os.DirFS(dir), version)
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

	if errors := linted.Errors(); errors > 0 {
		return fmt.Errorf("lint found %d error(s)", errors)
	}

	return nil
}
