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
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tochemey/mig/pkg/mig"
)

// newImportCommand builds the command that adopts another tool's history.
func newImportCommand() *cobra.Command {
	var (
		cfg  config
		from string
	)

	cmd := &cobra.Command{
		Use:   "import",
		Short: "Adopt a history written by goose or golang-migrate",
		Long: "Read the other tool's history table and record everything it applied as\n" +
			"already succeeded, so those migrations are not run again.\n\n" +
			"No file is rewritten and no schema is touched. Migrations the other tool\n" +
			"does not record are left to be reconciled against the catalog on the next\n" +
			"run, which is safe whether or not they in fact ran.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runImport(cmd.Context(), cfg, mig.Source(from), cmd.OutOrStdout())
		},
	}

	bindDatabase(cmd, &cfg)

	cmd.Flags().DurationVar(&cfg.ttl, "lease-ttl", envDuration(EnvLeaseTTL, mig.DefaultTTL),
		"lease validity window")
	cmd.Flags().StringVar(&from, "from", "", "tool to adopt from: "+sourceList())

	return cmd
}

// sourceList renders the accepted sources for the flag's help and its errors.
func sourceList() string {
	names := make([]string, 0, len(mig.Sources()))
	for _, source := range mig.Sources() {
		names = append(names, string(source))
	}

	return strings.Join(names, "|")
}

// runImport adopts the other tool's history under the lease.
func runImport(ctx context.Context, cfg config, source mig.Source, stdout io.Writer) (err error) {
	if cfg.dsn == "" {
		return fmt.Errorf("no connection string: pass --dsn or set %s", EnvDSN)
	}

	if !slices.Contains(mig.Sources(), source) {
		return fmt.Errorf("%w: %q: pass --from %s", mig.ErrUnknownSource, source, sourceList())
	}

	db, err := open(ctx, cfg.dsn, controlConns)
	if err != nil {
		return err
	}

	defer func() {
		err = errors.Join(err, closePool(db))
	}()

	report, err := mig.Import(ctx, db, os.DirFS(cfg.dir), source, mig.Options{
		TTL:      cfg.ttl,
		OnLocked: mig.Fail,
	})
	if err != nil {
		if errors.Is(err, mig.ErrLocked) {
			return exitError{code: ExitLocked, err: err}
		}

		return err
	}

	return writeReport(stdout, report)
}

// writeReport says exactly what was adopted and what was not.
func writeReport(stdout io.Writer, report mig.ImportReport) error {
	lines := []string{fmt.Sprintf("adopted %d migration(s) from %s", len(report.Adopted), report.Source)}

	for _, id := range report.Adopted {
		lines = append(lines, "  adopted:  "+id)
	}

	for _, id := range report.Recheck {
		lines = append(lines, "  recheck:  "+id)
	}

	for _, version := range report.Unknown {
		lines = append(lines, fmt.Sprintf("  no file:  %d (applied there, absent here)", version))
	}

	if report.Dirty {
		lines = append(lines, fmt.Sprintf(
			"  dirty:    %d left to reconcile against the catalog", report.DirtyVersion))
	}

	if _, err := fmt.Fprintln(stdout, strings.Join(lines, "\n")); err != nil {
		return fmt.Errorf("write report: %w", err)
	}

	return nil
}
