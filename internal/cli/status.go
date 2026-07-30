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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/tochemey/mig/pkg/mig"
)

// newStatusCommand builds the command that reports what the ledger holds.
func newStatusCommand() *cobra.Command {
	var (
		cfg    config
		asJSON bool
	)

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report what the ledger records about every step",
		Long: "Print each recorded step with its state, attempt count, checkpoint and\n" +
			"last error. It reads the ledger and writes nothing.\n\n" +
			"Whether work remains is a question for verify, which asks the catalog.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStatus(cmd.Context(), cfg, asJSON, cmd.OutOrStdout())
		},
	}

	bindDSN(cmd, &cfg)
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the report as JSON")

	return cmd
}

// runStatus prints the ledger's account of every step.
func runStatus(ctx context.Context, cfg config, asJSON bool, stdout io.Writer) (err error) {
	if cfg.dsn == "" {
		return fmt.Errorf("no connection string: pass --dsn or set %s", EnvDSN)
	}

	db, err := open(ctx, cfg.dsn, controlConns)
	if err != nil {
		return err
	}

	defer func() {
		err = errors.Join(err, closePool(db))
	}()

	status, err := mig.Status(ctx, db)
	if err != nil {
		return err
	}

	if asJSON {
		return writeJSON(stdout, status)
	}

	return writeTable(stdout, status)
}

// writeJSON emits the report for a machine.
func writeJSON(stdout io.Writer, status []mig.StepStatus) error {
	encoded, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("encode status: %w", err)
	}

	if _, err := fmt.Fprintln(stdout, string(encoded)); err != nil {
		return fmt.Errorf("write status: %w", err)
	}

	return nil
}

// writeTable emits the report for a person.
func writeTable(stdout io.Writer, status []mig.StepStatus) error {
	if len(status) == 0 {
		if _, err := fmt.Fprintln(stdout, "no migrations recorded"); err != nil {
			return fmt.Errorf("write status: %w", err)
		}

		return nil
	}

	out := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)

	if _, err := fmt.Fprintln(out, "MIGRATION\tSTEP\tKIND\tSTATUS\tATTEMPTS\tDETAIL"); err != nil {
		return fmt.Errorf("write status: %w", err)
	}

	for _, s := range status {
		detail := s.Checkpoint
		if s.Error != "" {
			detail = s.Error
		}

		if _, err := fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%d\t%s\n",
			s.Migration, s.Name, s.Kind, s.Status, s.Attempts, detail); err != nil {
			return fmt.Errorf("write status: %w", err)
		}
	}

	if err := out.Flush(); err != nil {
		return fmt.Errorf("write status: %w", err)
	}

	return nil
}
