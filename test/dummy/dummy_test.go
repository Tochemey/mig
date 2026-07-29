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

package dummy_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/tochemey/mig/test/dummy"
	"github.com/tochemey/mig/test/harness"
)

// shared is the container for this package, or nil when docker is absent.
var shared *harness.Harness

// TestMain brings up one container for the package.
func TestMain(m *testing.M) {
	os.Exit(harness.Main(m, 0, func(_ context.Context, h *harness.Harness) error {
		shared = h
		return nil
	}))
}

// TestRunHoldsThenReturns covers the ordinary path: connect, announce, hold the
// backend for the requested time, return.
func TestRunHoldsThenReturns(t *testing.T) {
	dsn := newDSN(t)

	var announced string

	start := time.Now()

	err := dummy.Run(t.Context(), dsn, 200*time.Millisecond, func(marker string) {
		announced = marker
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if announced != dummy.ReadyMarker {
		t.Fatalf("announced %q, want %q", announced, dummy.ReadyMarker)
	}

	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Fatalf("returned after %s without holding the backend", elapsed)
	}
}

// TestRunRejectsMalformedDSN covers the connection string never being parsed.
func TestRunRejectsMalformedDSN(t *testing.T) {
	err := dummy.Run(t.Context(), "://not-a-dsn", time.Second, func(string) {
		t.Error("announced readiness despite a malformed DSN")
	})

	if err == nil {
		t.Fatal("run accepted a malformed DSN")
	}
}

// TestRunRejectsUnreachableDatabase covers a DSN that parses but leads nowhere,
// which fails before readiness is announced.
func TestRunRejectsUnreachableDatabase(t *testing.T) {
	if shared == nil {
		t.Skip("postgres container not available")
	}

	err := dummy.Run(t.Context(), shared.DSN("no_such_database"), time.Second, func(string) {
		t.Error("announced readiness without a connection")
	})

	if err == nil {
		t.Fatal("run accepted an unreachable database")
	}
}

// TestRunStopsWithItsContext covers the hold being cut short by cancellation.
func TestRunStopsWithItsContext(t *testing.T) {
	dsn := newDSN(t)

	ctx, cancel := context.WithCancel(t.Context())

	ready := make(chan struct{})

	go func() {
		<-ready
		cancel()
	}()

	err := dummy.Run(ctx, dsn, time.Hour, func(string) {
		close(ready)
	})

	if err == nil {
		t.Fatal("run held the backend past its cancelled context")
	}
}

// newDSN gives the test its own database and returns a connection string.
func newDSN(t *testing.T) string {
	t.Helper()

	if shared == nil {
		t.Skip("postgres container not available")
	}

	name, err := shared.Clone(t.Context(), harness.Template)
	if err != nil {
		t.Fatalf("clone template: %v", err)
	}

	t.Cleanup(func() {
		if err := shared.DropDatabase(context.Background(), name); err != nil {
			t.Errorf("drop clone %q: %v", name, err)
		}
	})

	return shared.DSN(name)
}
