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

// Package throttle paces a backfill against what the database can absorb.
//
// The loop is closed rather than a fixed sleep. A fixed sleep is tuned for one
// machine on one day: too slow and the backfill never finishes, too fast and it
// buries the replicas. Measuring what the last batch cost and what the replicas
// are carrying lets the rate find its own level.
package throttle

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// lagQuery reports the furthest-behind replica. A primary with no replicas
// returns no rows, which is no lag rather than an error.
const lagQuery = `
SELECT coalesce(max(sent_lsn - replay_lsn), 0)::bigint
  FROM pg_stat_replication
 WHERE replay_lsn IS NOT NULL`

const (
	// MinBatch and MaxBatch bound the batch size however the feedback moves.
	// Below the floor the per-batch overhead dominates; above the ceiling one
	// batch holds locks for long enough to matter.
	MinBatch = 100
	MaxBatch = 50_000

	// shrinkFactor halves the batch when the database is struggling. Backing
	// off sharply and recovering gently is what keeps a throttle from
	// oscillating.
	shrinkFactor = 2

	// growNumerator and growDenominator grow the batch by a quarter.
	growNumerator   = 5
	growDenominator = 4
)

// DefaultTargetLatency is the batch duration the throttle aims at. Longer than
// this and a batch holds its locks long enough to be noticed by other traffic.
const DefaultTargetLatency = 500 * time.Millisecond

// LagSource reports how far replicas are behind, in bytes.
type LagSource interface {
	Lag(ctx context.Context) (int64, error)
}

// Config parameterises [New].
type Config struct {
	// Batch is the starting batch size, clamped into [MinBatch, MaxBatch].
	Batch int

	// MaxLagBytes is the replication lag above which the throttle backs off.
	// Zero disables the lag signal.
	MaxLagBytes int64

	// TargetLatency is the batch duration to aim at. Zero means
	// [DefaultTargetLatency].
	TargetLatency time.Duration

	// Lag reports replication lag. Nil disables the lag signal.
	Lag LagSource
}

// Throttle paces a backfill and sizes its batches.
type Throttle struct {
	batch         int
	maxLagBytes   int64
	targetLatency time.Duration
	lag           LagSource
}

// New builds a throttle from cfg.
func New(config Config) *Throttle {
	if config.TargetLatency <= 0 {
		config.TargetLatency = DefaultTargetLatency
	}

	return &Throttle{
		batch:         clamp(config.Batch),
		maxLagBytes:   config.MaxLagBytes,
		targetLatency: config.TargetLatency,
		lag:           config.Lag,
	}
}

// Batch is the size the next batch should use.
func (x *Throttle) Batch() int {
	return x.batch
}

// Wait closes the feedback loop after a batch finishes.
//
// It reads two signals: whether lastBatch ran longer than the target latency,
// and whether replica lag is above MaxLagBytes. Either one means the database
// is struggling, so the next [Batch] is halved (then clamped). When both are
// clear the batch grows by a quarter, recovering gently so the throttle does
// not oscillate.
//
// Resizing always runs first. A slow batch still needs a smaller next write
// even when the replicas are keeping up: the fix for holding locks too long is
// to write less, not to sleep. Only when lag is above the limit does Wait then
// pause for lastBatch, giving the replicas roughly the time back that writing
// the batch cost them. Cancellation during that pause returns ctx.Err.
func (x *Throttle) Wait(ctx context.Context, lastBatch time.Duration) error {
	lag, err := x.observe(ctx)
	if err != nil {
		return err
	}

	behind := x.maxLagBytes > 0 && lag > x.maxLagBytes
	slow := lastBatch > x.targetLatency

	if behind || slow {
		x.batch = clamp(x.batch / shrinkFactor)
	} else {
		x.batch = clamp(x.batch * growNumerator / growDenominator)
	}

	if !behind {
		return nil
	}

	// Pause for as long as the last batch took, so the replicas get roughly the
	// time back that writing it cost them.
	select {
	case <-ctx.Done():
		return fmt.Errorf("throttling backfill: %w", ctx.Err())
	case <-time.After(lastBatch):
		return nil
	}
}

// observe reads the replication lag, treating an absent source as no lag.
func (x *Throttle) observe(ctx context.Context) (int64, error) {
	if x.lag == nil {
		return 0, nil
	}

	return x.lag.Lag(ctx)
}

// clamp holds a batch size inside the supported range.
func clamp(batch int) int {
	if batch < MinBatch {
		return MinBatch
	}

	if batch > MaxBatch {
		return MaxBatch
	}

	return batch
}

// replicationLag reads lag from pg_stat_replication.
type replicationLag struct {
	db *sql.DB
}

// Replication reports lag from the primary's view of its replicas.
func Replication(db *sql.DB) LagSource {
	return replicationLag{db: db}
}

// Lag reports how far the furthest-behind replica is, in bytes.
func (r replicationLag) Lag(ctx context.Context) (int64, error) {
	var lag int64

	err := r.db.QueryRowContext(ctx, lagQuery).Scan(&lag)

	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}

	if err != nil {
		return 0, fmt.Errorf("read replication lag: %w", err)
	}

	return lag, nil
}
