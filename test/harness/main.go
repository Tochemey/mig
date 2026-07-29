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

package harness

import (
	"context"
	"log"
	"os"
	"testing"
	"time"
)

const (
	// Template is the database that [Main] seeds and that [Harness.Clone]
	// copies for each test.
	Template = "mig_seed"

	// setupTimeout bounds container startup, seeding and any package setup.
	setupTimeout = 5 * time.Minute

	// EnvCoverDir names the directory where spawned binaries write coverage
	// counters.
	EnvCoverDir = "MIG_COVERDIR"
)

// Main is the standard TestMain body for a package that needs Postgres. It
// starts a container, seeds [Template] with rows fixture rows, runs setup, runs
// the tests, and tears everything down.
//
// When no docker daemon is reachable it logs and reports success without
// running anything, leaving the harness nil so tests skip rather than fail.
//
// Usage:
//
//	func TestMain(m *testing.M) {
//	    os.Exit(harness.Main(m, 0, func(_ context.Context, h *harness.Harness) error {
//	        shared = h
//	        return nil
//	    }))
//	}
func Main(m *testing.M, rows int, setup func(context.Context, *Harness) error) int {
	coverDir = os.Getenv(EnvCoverDir)

	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()

	h, err := New(ctx)
	if err != nil {
		log.Printf("harness: postgres container unavailable, skipping package: %v", err)
		return 0
	}

	defer func() {
		// A fresh context, since the setup context may already have expired.
		if err := h.Close(context.Background()); err != nil {
			log.Printf("harness: close: %v", err)
		}
	}()

	binDir, err := os.MkdirTemp("", "mig-bin-")
	if err != nil {
		log.Printf("harness: temp dir: %v", err)
		return 1
	}

	defer func() {
		_ = os.RemoveAll(binDir)
	}()

	h.binDir = binDir

	if err := h.SeedTemplate(ctx, Template, rows); err != nil {
		log.Printf("harness: seed template: %v", err)
		return 1
	}

	if setup != nil {
		if err := setup(ctx, h); err != nil {
			log.Printf("harness: package setup: %v", err)
			return 1
		}
	}

	return m.Run()
}

// BinDir returns a temporary directory, removed when the package finishes, for
// binaries built by [Build].
func (h *Harness) BinDir() string {
	return h.binDir
}
