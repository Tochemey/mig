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

package throttle_test

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"
	"time"

	"github.com/tochemey/mig/internal/throttle"
)

// TestBatchGrowsWhenHealthy covers the recovery path: batches creep back up
// when nothing is struggling.
func TestBatchGrowsWhenHealthy(t *testing.T) {
	th := throttle.New(throttle.Config{Batch: 1000, TargetLatency: time.Second})

	before := th.Batch()

	if err := th.Wait(t.Context(), 10*time.Millisecond); err != nil {
		t.Fatalf("wait: %v", err)
	}

	if after := th.Batch(); after <= before {
		t.Fatalf("batch went from %d to %d, want it to grow", before, after)
	}
}

// TestBatchShrinksWhenBatchesAreSlow covers the latency signal. A batch that
// runs long is holding its locks long, whatever the replicas are doing.
func TestBatchShrinksWhenBatchesAreSlow(t *testing.T) {
	th := throttle.New(throttle.Config{Batch: 1000, TargetLatency: 10 * time.Millisecond})

	before := th.Batch()

	if err := th.Wait(t.Context(), time.Second); err != nil {
		t.Fatalf("wait: %v", err)
	}

	if after := th.Batch(); after >= before {
		t.Fatalf("batch went from %d to %d, want it to shrink", before, after)
	}
}

// TestBatchShrinksAndPausesWhenReplicasLag covers the other signal, and the
// pause that goes with it: shrinking alone would keep writing, just in smaller
// pieces.
func TestBatchShrinksAndPausesWhenReplicasLag(t *testing.T) {
	th := throttle.New(throttle.Config{
		Batch:         1000,
		MaxLagBytes:   1024,
		TargetLatency: time.Second,
		Lag:           fixedLag(4096),
	})

	before := th.Batch()

	const lastBatch = 50 * time.Millisecond

	started := time.Now()

	if err := th.Wait(t.Context(), lastBatch); err != nil {
		t.Fatalf("wait: %v", err)
	}

	if after := th.Batch(); after >= before {
		t.Fatalf("batch went from %d to %d, want it to shrink", before, after)
	}

	if elapsed := time.Since(started); elapsed < lastBatch {
		t.Fatalf("paused for %s, want at least the %s the last batch took", elapsed, lastBatch)
	}
}

// TestLagBelowTheLimitDoesNotPause keeps the throttle from slowing a backfill
// the replicas are keeping up with.
func TestLagBelowTheLimitDoesNotPause(t *testing.T) {
	th := throttle.New(throttle.Config{
		Batch:         1000,
		MaxLagBytes:   4096,
		TargetLatency: time.Second,
		Lag:           fixedLag(10),
	})

	started := time.Now()

	if err := th.Wait(t.Context(), 500*time.Millisecond); err != nil {
		t.Fatalf("wait: %v", err)
	}

	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("paused for %s with the replicas keeping up", elapsed)
	}
}

// TestBatchStaysWithinBounds covers the clamps. Below the floor the per-batch
// overhead dominates; above the ceiling one batch holds its locks too long.
func TestBatchStaysWithinBounds(t *testing.T) {
	shrinking := throttle.New(throttle.Config{Batch: throttle.MinBatch, TargetLatency: time.Nanosecond})

	for range 20 {
		if err := shrinking.Wait(t.Context(), time.Second); err != nil {
			t.Fatalf("wait: %v", err)
		}
	}

	if got := shrinking.Batch(); got != throttle.MinBatch {
		t.Fatalf("batch shrank to %d, below the floor of %d", got, throttle.MinBatch)
	}

	growing := throttle.New(throttle.Config{Batch: throttle.MaxBatch, TargetLatency: time.Hour})

	for range 20 {
		if err := growing.Wait(t.Context(), time.Nanosecond); err != nil {
			t.Fatalf("wait: %v", err)
		}
	}

	if got := growing.Batch(); got != throttle.MaxBatch {
		t.Fatalf("batch grew to %d, above the ceiling of %d", got, throttle.MaxBatch)
	}
}

// TestConfiguredBatchIsClamped covers an annotation asking for a size outside
// the supported range.
func TestConfiguredBatchIsClamped(t *testing.T) {
	if got := throttle.New(throttle.Config{Batch: 1}).Batch(); got != throttle.MinBatch {
		t.Fatalf("a batch of 1 became %d, want %d", got, throttle.MinBatch)
	}

	if got := throttle.New(throttle.Config{Batch: 1 << 30}).Batch(); got != throttle.MaxBatch {
		t.Fatalf("an enormous batch became %d, want %d", got, throttle.MaxBatch)
	}
}

// TestNoLagSourceMeansNoLag covers a primary with nothing replicating from it,
// which must not be mistaken for a database in trouble.
func TestNoLagSourceMeansNoLag(t *testing.T) {
	th := throttle.New(throttle.Config{Batch: 1000, MaxLagBytes: 1, TargetLatency: time.Second})

	before := th.Batch()

	if err := th.Wait(t.Context(), time.Millisecond); err != nil {
		t.Fatalf("wait: %v", err)
	}

	if after := th.Batch(); after <= before {
		t.Fatalf("batch went from %d to %d with no replicas to fall behind", before, after)
	}
}

// TestLagFailureStopsTheBackfill keeps a throttle that cannot see the replicas
// from carrying on as though they were healthy.
func TestLagFailureStopsTheBackfill(t *testing.T) {
	th := throttle.New(throttle.Config{Batch: 1000, MaxLagBytes: 1024, Lag: failingLag{}})

	if err := th.Wait(t.Context(), time.Millisecond); err == nil {
		t.Fatal("a throttle that cannot read lag reported success")
	}
}

// TestWaitHonoursCancellation covers a run abandoned while the throttle is
// pausing.
func TestWaitHonoursCancellation(t *testing.T) {
	th := throttle.New(throttle.Config{
		Batch:         1000,
		MaxLagBytes:   1,
		TargetLatency: time.Second,
		Lag:           fixedLag(1 << 20),
	})

	ctx, cancel := context.WithCancel(t.Context())

	time.AfterFunc(50*time.Millisecond, cancel)

	if err := th.Wait(ctx, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait returned %v, want context.Canceled", err)
	}
}

// TestReplicationReportsLag covers the happy path through pg_stat_replication's
// adapter: a row comes back and becomes a byte count the throttle can use.
func TestReplicationReportsLag(t *testing.T) {
	const want int64 = 4096

	source := throttle.Replication(openStub(t, []driver.Value{want}, nil))

	got, err := source.Lag(t.Context())
	if err != nil {
		t.Fatalf("lag: %v", err)
	}

	if got != want {
		t.Fatalf("lag = %d, want %d", got, want)
	}
}

// TestReplicationTreatsNoRowsAsNoLag covers a primary with nothing in
// pg_stat_replication. That is healthy, not an error.
func TestReplicationTreatsNoRowsAsNoLag(t *testing.T) {
	source := throttle.Replication(openStub(t, nil, nil))

	got, err := source.Lag(t.Context())
	if err != nil {
		t.Fatalf("lag: %v", err)
	}

	if got != 0 {
		t.Fatalf("lag = %d, want 0 when there are no replicas", got)
	}
}

// TestReplicationWrapsQueryFailures covers a lag read that cannot run. The
// throttle must see the failure rather than treat it as zero lag.
func TestReplicationWrapsQueryFailures(t *testing.T) {
	injected := errors.New("connection reset")
	source := throttle.Replication(openStub(t, nil, injected))

	_, err := source.Lag(t.Context())
	if err == nil {
		t.Fatal("lag reported success on a failed query")
	}

	if !errors.Is(err, injected) {
		t.Fatalf("lag error %v does not wrap the query failure", err)
	}
}
