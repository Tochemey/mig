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

// Package cli holds the command-line surface.
//
// It lives here rather than in cmd/mig so the commands can be driven directly,
// without a subprocess, wherever a subprocess is not what is being tested.
package cli

import (
	"errors"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/tochemey/mig/pkg/mig"
)

// Version identifies this build in application_name and in logs.
const Version = "0.1.0"

// Exit codes. They are part of the interface: a job scheduler has to tell
// "another runner is applying" from "the migration failed".
const (
	// ExitOK means the run converged.
	ExitOK = 0

	// ExitError means the run failed.
	ExitError = 1

	// ExitPending means verify found work outstanding. It is distinct from
	// ExitError because a scheduler treats "not migrated yet" and "the check
	// itself failed" differently.
	ExitPending = 3

	// ExitLocked means another runner holds the lease.
	ExitLocked = 4
)

// Environment variables supplying defaults for the flags of the same name.
const (
	EnvDSN      = "MIG_DSN"
	EnvDir      = "MIG_DIR"
	EnvLeaseTTL = "MIG_LEASE_TTL"
)

// New builds the command tree.
//
// Usage is silenced on failure: a migration that could not run is not a usage
// problem, and burying the reason under a page of flags helps nobody. Errors
// are silenced too, so that main prints them once.
func New() *cobra.Command {
	root := &cobra.Command{
		Use:           "mig",
		Short:         "Postgres migrations you can kill and re-run",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(newUpCommand())
	root.AddCommand(newPlanCommand())
	root.AddCommand(newVerifyCommand())
	root.AddCommand(newStatusCommand())
	root.AddCommand(newImportCommand())
	root.AddCommand(newFingerprintCommand())

	return root
}

// exitError carries the process exit code for a failure the caller has to
// distinguish from an ordinary one.
type exitError struct {
	code int
	err  error
}

// Error reports the underlying failure.
func (e exitError) Error() string {
	return e.err.Error()
}

// Unwrap exposes the underlying failure to errors.Is and errors.As.
func (e exitError) Unwrap() error {
	return e.err
}

// ExitCode maps the result of Execute to a process exit code.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}

	var exit exitError
	if errors.As(err, &exit) {
		return exit.code
	}

	return ExitError
}

// config is what every command needs to reach the database.
type config struct {
	dsn      string
	dir      string
	ttl      time.Duration
	onLocked string
	drift    bool
	verbose  bool
}

// bindDSN registers the flag every command that connects needs.
func bindDSN(cmd *cobra.Command, cfg *config) {
	cmd.Flags().StringVar(&cfg.dsn, "dsn", os.Getenv(EnvDSN), "postgres connection string")
}

// bindDatabase adds the migration directory, for a command that reads it.
func bindDatabase(cmd *cobra.Command, cfg *config) {
	bindDSN(cmd, cfg)

	cmd.Flags().StringVar(&cfg.dir, "dir", envOr(EnvDir, "migrations"),
		"directory holding migration files")
}

// bindApply adds the flags only a run that writes needs.
func bindApply(cmd *cobra.Command, cfg *config) {
	bindDatabase(cmd, cfg)

	flags := cmd.Flags()

	flags.DurationVar(&cfg.ttl, "lease-ttl", envDuration(EnvLeaseTTL, mig.DefaultTTL), "lease validity window")
	flags.StringVar(&cfg.onLocked, "on-locked", string(mig.Wait), "wait or fail when another runner holds the lease")
	flags.BoolVar(&cfg.drift, "allow-drift", false, "continue when an applied step's checksum has changed")
	flags.BoolVar(&cfg.verbose, "verbose", false,
		"structured JSON logs on stderr instead of the progress display")
}

// envOr returns the environment value, or fallback when it is unset.
func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}

	return fallback
}

// envDuration parses a duration from the environment, ignoring an unusable one
// so a stray value cannot silently shorten a lease.
func envDuration(name string, fallback time.Duration) time.Duration {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}

	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}

	return parsed
}
