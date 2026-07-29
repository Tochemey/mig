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
// It lives here rather than in cmd/mig so that the commands can be driven
// directly, without a subprocess, wherever a subprocess is not what is being
// tested.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/tochemey/mig/internal/lease"
)

// Version identifies this build in application_name and in logs.
const Version = "0.1.0"

// Exit codes. They are part of the interface: a job scheduler distinguishes
// "someone else is applying" from "the migration failed".
const (
	// ExitOK means the run converged.
	ExitOK = 0

	// ExitError means the run failed.
	ExitError = 1

	// ExitPending is reserved for verify against a database with work
	// outstanding.
	ExitPending = 3

	// ExitLocked means another runner holds the lease.
	ExitLocked = 4
)

// Environment variables that supply defaults for the flags of the same name.
const (
	EnvDSN      = "MIG_DSN"
	EnvDir      = "MIG_DIR"
	EnvLeaseTTL = "MIG_LEASE_TTL"
)

// ErrUsage reports a command line that could not be understood.
var ErrUsage = errors.New("usage: mig up [flags]")

// Main dispatches a command and returns the process exit code.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	if len(args) == 0 {
		return ExitError, ErrUsage
	}

	switch args[0] {
	case "up":
		return up(ctx, args[1:], stdout, stderr)

	default:
		return ExitError, fmt.Errorf("%w: unknown command %q", ErrUsage, args[0])
	}
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

// bind registers the common flags, defaulting from the environment.
func bind(flags *flag.FlagSet, cfg *config) {
	flags.StringVar(&cfg.dsn, "dsn", os.Getenv(EnvDSN), "postgres connection string")
	flags.StringVar(&cfg.dir, "dir", envOr(EnvDir, "migrations"), "directory holding migration files")
	flags.DurationVar(&cfg.ttl, "lease-ttl", envDuration(EnvLeaseTTL, lease.DefaultTTL), "lease validity window")
	flags.StringVar(&cfg.onLocked, "on-locked", string(lease.Wait), "wait or fail when another runner holds the lease")
	flags.BoolVar(&cfg.drift, "allow-drift", false, "continue when an applied step's checksum has changed")
	flags.BoolVar(&cfg.verbose, "verbose", false, "log every step transition to stderr")
}

// envOr returns the environment value, or fallback when it is unset.
func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}

	return fallback
}

// envDuration parses a duration from the environment, ignoring an unusable one
// so that a stray value cannot silently shorten a lease.
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
