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

	"github.com/tochemey/mig/pkg/mig"
)

// newVerifyCommand builds the read-only check a service makes at startup.
func newVerifyCommand() *cobra.Command {
	var cfg config

	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Report whether every migration has been applied, without applying any",
		Long: "Check the database against the migration directory and report what is\n" +
			"outstanding. It takes no lease and writes nothing.\n\n" +
			"This is the check a service makes as it starts. Applying on boot from the\n" +
			"service binary is the shape to avoid: every replica races, none is elected\n" +
			"to do it, and a slow migration collides with the readiness probe.\n\n" +
			"Exits 0 when nothing is outstanding, 3 when something is, 1 on failure.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runVerify(cmd.Context(), cfg, cmd.OutOrStdout())
		},
	}

	bindDatabase(cmd, &cfg)

	return cmd
}

// runVerify reports what the database has still to apply.
func runVerify(ctx context.Context, cfg config, stdout io.Writer) (err error) {
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

	outstanding, err := mig.Pending(ctx, db, os.DirFS(cfg.dir))
	if err != nil {
		return err
	}

	if len(outstanding) == 0 {
		if _, err := fmt.Fprintln(stdout, "up to date"); err != nil {
			return fmt.Errorf("write result: %w", err)
		}

		return nil
	}

	for _, step := range outstanding {
		if _, err := fmt.Fprintln(stdout, "pending:", step); err != nil {
			return fmt.Errorf("write result: %w", err)
		}
	}

	// A distinct code, because a scheduler treats "not migrated yet" and "the
	// check itself failed" differently.
	return exitError{
		code: ExitPending,
		err:  fmt.Errorf("%w: %d step(s) outstanding", mig.ErrPending, len(outstanding)),
	}
}
