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
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/tochemey/mig/pkg/mig"
)

// newPlanCommand builds the command that shows what a run would check.
func newPlanCommand() *cobra.Command {
	var dir string

	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Show the steps and the predicate each will be judged by",
		Long: "Read the migration directory and print, for every step, its kind and the\n" +
			"condition the catalog must satisfy for the step to count as done.\n\n" +
			"It touches no database and writes nothing.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPlan(dir, cmd.OutOrStdout())
		},
	}

	cmd.Flags().StringVar(&dir, "dir", envOr(EnvDir, "migrations"), "directory holding migration files")

	return cmd
}

// runPlan prints every step and the predicate it will be judged by.
//
// A step that cannot be reconciled is shown rather than hidden, and reported
// through the exit code, so the refusal a run would make is visible before
// anyone reaches for a database.
func runPlan(dir string, stdout io.Writer) error {
	planned, err := mig.Plan(os.DirFS(dir))
	if err != nil {
		return err
	}

	out := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)

	refused := 0
	migration := ""

	for _, step := range planned {
		if step.Migration != migration {
			migration = step.Migration

			if _, err := fmt.Fprintln(out, migration); err != nil {
				return fmt.Errorf("write plan: %w", err)
			}
		}

		if !step.Reconcilable {
			refused++
		}

		if _, err := fmt.Fprintf(out, "  %d\t%s\t%s\t%s\n",
			step.Index, step.Name, step.Kind, step.Describe); err != nil {
			return fmt.Errorf("write plan: %w", err)
		}
	}

	if err := out.Flush(); err != nil {
		return fmt.Errorf("write plan: %w", err)
	}

	if refused > 0 {
		return fmt.Errorf(
			"%d step(s) cannot be reconciled; add a satisfied: annotation to each", refused)
	}

	return nil
}
