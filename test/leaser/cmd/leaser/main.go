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

// Command leaser is the executable wrapper around package leaser.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/tochemey/mig/internal/lease"
	"github.com/tochemey/mig/test/leaser"
)

func main() {
	cfg := leaser.Config{}

	onLocked := flag.String("on-locked", string(lease.Wait), "wait or fail when another runner holds the lease")

	flag.StringVar(&cfg.DSN, "dsn", "", "postgres connection string")
	flag.StringVar(&cfg.MigrationID, "id", "run", "ledger migration id to write")
	flag.DurationVar(&cfg.TTL, "ttl", lease.DefaultTTL, "lease validity window")
	flag.DurationVar(&cfg.Hold, "hold", time.Second, "how long to hold the lease after claiming")
	flag.DurationVar(&cfg.WaitTimeout, "wait-timeout", 30*time.Second, "how long to wait for a held lease")
	flag.Parse()

	cfg.OnLocked = lease.OnLocked(*onLocked)

	// Unbuffered, so a SIGKILL cannot swallow a marker a test is waiting on.
	out := func(line string) {
		_, _ = fmt.Fprintln(os.Stdout, line)
	}

	os.Exit(leaser.Run(context.Background(), cfg, out))
}
