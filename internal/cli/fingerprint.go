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

	"github.com/spf13/cobra"

	"github.com/tochemey/mig/pkg/mig"
)

// newFingerprintCommand builds the command that hashes the schema.
func newFingerprintCommand() *cobra.Command {
	var (
		cfg      config
		describe bool
	)

	cmd := &cobra.Command{
		Use:   "fingerprint",
		Short: "Print a canonical hash of the schema",
		Long: "Hash every relation, column, index and constraint into one digest, so\n" +
			"that two databases can be compared without diffing dumps.\n\n" +
			"Two runs that converge to the same schema produce the same digest. That\n" +
			"is what makes a killed-and-resumed run checkable against one that was\n" +
			"left alone.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runFingerprint(cmd.Context(), cfg, describe, cmd.OutOrStdout())
		},
	}

	bindDSN(cmd, &cfg)
	cmd.Flags().BoolVar(&describe, "describe", false,
		"print the hashed rows instead of the digest")

	return cmd
}

// runFingerprint prints the schema digest.
func runFingerprint(ctx context.Context, cfg config, describe bool, stdout io.Writer) (err error) {
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

	report := mig.Fingerprint
	if describe {
		report = mig.Describe
	}

	out, err := report(ctx, db)
	if err != nil {
		return err
	}

	if _, err := fmt.Fprintln(stdout, out); err != nil {
		return fmt.Errorf("write fingerprint: %w", err)
	}

	return nil
}
