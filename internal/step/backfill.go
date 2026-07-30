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

package step

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tochemey/mig/internal/catalog"
	"github.com/tochemey/mig/internal/crash"
	"github.com/tochemey/mig/internal/throttle"
)

// ErrNoBackfillPredicate reports a backfill with no way to tell whether it has
// finished.
//
// The cursor lives in the ledger, and the ledger may not decide whether work
// remains. Until inference covers backfill shapes, the author supplies the
// predicate.
var ErrNoBackfillPredicate = errors.New(
	"backfill step has no predicate; add a satisfied: annotation that reports when no rows remain")

// BackfillConfig is what the backfill annotation carries.
type BackfillConfig struct {
	// Table and Key name the relation and the column the cursor walks.
	Table string
	Key   string

	// Batch is the starting number of key values covered per transaction.
	Batch int

	// MaxLagBytes is the replication lag above which batches back off.
	MaxLagBytes int64
}

// Cursor is a backfill's recorded progress.
type Cursor struct {
	// Position is the highest key value covered so far.
	Position int64 `json:"cursor"`

	// Rows is how many rows the batches have touched, for an operator watching
	// a long backfill.
	Rows int64 `json:"rows"`
}

// Env is what a resumable step needs from the executor.
//
// Begin and Checkpoint exist as a pair so the step cannot commit a batch
// without its cursor: the transaction it is handed is already fenced, and the
// checkpoint is written into that same transaction.
type Env struct {
	// Begin opens a batch transaction on the work pool, with the lease fence
	// already asserted.
	Begin func(ctx context.Context) (*sql.Tx, error)

	// Checkpoint records progress inside the caller's transaction.
	Checkpoint func(ctx context.Context, tx *sql.Tx, state []byte) error

	// Load reads the progress recorded by an earlier run.
	Load func(ctx context.Context) ([]byte, error)

	// Read runs a query on the work pool outside any transaction, for the
	// bookkeeping that does not belong in a batch.
	Read func(ctx context.Context, query string, args ...any) *sql.Row

	// Retry runs one batch, retrying while the database reports it could not
	// take a lock in time. The policy belongs to the executor; the boundary —
	// one batch — belongs here.
	Retry func(ctx context.Context, batch func(context.Context) error) error

	// Throttle paces the batches and sizes them.
	Throttle *throttle.Throttle

	// Report receives a line per batch, for an operator watching progress.
	Report func(cursor Cursor)
}

// ResumableStep runs long and records its progress as it goes.
type ResumableStep interface {
	Step

	Run(ctx context.Context, env Env) error
}

// Backfill rewrites a table in batches, recording how far it has got.
//
// Pagination is by key range, never by OFFSET: OFFSET rescans everything it
// skips, so the cost grows with the square of the table, and it silently misses
// rows when concurrent writes shift what it is counting past.
type Backfill struct {
	meta      Meta
	statement string
	cfg       BackfillConfig
	satisfied Predicate
}

// NewBackfill builds a backfill step. The predicate is required.
func NewBackfill(meta Meta, statement string, cfg BackfillConfig, satisfied Predicate) (*Backfill, error) {
	if satisfied == nil {
		return nil, fmt.Errorf("step %q: %w", meta.Name, ErrNoBackfillPredicate)
	}

	return &Backfill{meta: meta, statement: statement, cfg: cfg, satisfied: satisfied}, nil
}

// Meta describes the step.
func (s *Backfill) Meta() Meta {
	return s.meta
}

// Satisfied reports whether any rows remain, judged from the data itself.
func (s *Backfill) Satisfied(ctx context.Context, conn *sql.Conn) (bool, error) {
	return s.satisfied(ctx, conn)
}

// Repair does nothing. Every batch commits with its cursor, so a backfill is
// never left part-way through a batch.
func (s *Backfill) Repair(context.Context, *sql.Conn) error {
	return nil
}

// keyRange is the span of key values a backfill has to walk.
type keyRange struct {
	low, high int64

	// empty reports a table with no rows, which is distinct from one whose keys
	// happen to reach zero.
	empty bool
}

// boundsQuery reads the range the cursor has to walk. The table and key are
// identifiers from the annotation, so they are quoted rather than bound.
//
// The aggregates are deliberately not coalesced: a default would make an empty
// table indistinguishable from one whose keys reach the default.
func (s *Backfill) boundsQuery() string {
	key := catalog.QualifiedIdent("", s.cfg.Key)

	return "SELECT min(" + key + "), max(" + key + ") FROM " +
		catalog.QualifiedIdent("", s.cfg.Table)
}

// Run walks the key range, committing each batch with the cursor that covers
// it, and resuming from wherever an earlier run stopped.
func (s *Backfill) Run(ctx context.Context, env Env) error {
	cursor, resumed, err := s.resume(ctx, env)
	if err != nil {
		return err
	}

	span, err := s.bounds(ctx, env)
	if err != nil {
		return err
	}

	if span.empty {
		return nil
	}

	// A first run starts one below the lowest key, so the first batch's
	// exclusive lower bound still covers it. Whether a checkpoint existed is
	// what decides this, not its value: a table whose keys start at or below
	// zero has legitimate cursor positions that would otherwise read as "never
	// started" and rewind the backfill.
	if !resumed {
		cursor.Position = span.low - 1
	}

	for cursor.Position < span.high {
		next, elapsed, err := s.batch(ctx, env, cursor)
		if err != nil {
			return err
		}

		cursor = next

		if env.Report != nil {
			env.Report(cursor)
		}

		if cursor.Position >= span.high {
			return nil
		}

		crash.At(crash.DuringThrottle)

		if err := env.Throttle.Wait(ctx, elapsed); err != nil {
			return err
		}
	}

	return nil
}

// bounds reads the range the cursor has to walk.
//
// It runs outside a transaction: a batch's transaction asserts the fence and
// holds the lease row for its duration, and a read that writes nothing has no
// reason to hold anything.
func (s *Backfill) bounds(ctx context.Context, env Env) (keyRange, error) {
	var low, high sql.NullInt64

	//nolint:gosec // G201: the table and key come from the migration, and are quoted.
	if err := env.Read(ctx, s.boundsQuery()).Scan(&low, &high); err != nil {
		return keyRange{}, fmt.Errorf("read key range of %q: %w", s.cfg.Table, err)
	}

	if !low.Valid || !high.Valid {
		return keyRange{empty: true}, nil
	}

	return keyRange{low: low.Int64, high: high.Int64}, nil
}

// resume reads the cursor an earlier run left behind, reporting whether there
// was one.
func (s *Backfill) resume(ctx context.Context, env Env) (Cursor, bool, error) {
	state, err := env.Load(ctx)
	if err != nil {
		return Cursor{}, false, err
	}

	if len(state) == 0 {
		return Cursor{}, false, nil
	}

	var cursor Cursor

	if err := json.Unmarshal(state, &cursor); err != nil {
		return Cursor{}, false, fmt.Errorf("read checkpoint of step %q: %w", s.meta.Name, err)
	}

	return cursor, true, nil
}

// batch applies one range of keys, retrying while the database reports it
// could not take a lock in time, and reports how long the successful attempt
// took so the throttle can size the next one.
func (s *Backfill) batch(ctx context.Context, env Env, cursor Cursor) (Cursor, time.Duration, error) {
	var (
		next    Cursor
		elapsed time.Duration
	)

	attempt := func(ctx context.Context) error {
		started := time.Now()

		committed, err := s.commitBatch(ctx, env, cursor)
		if err != nil {
			return err
		}

		next, elapsed = committed, time.Since(started)

		return nil
	}

	if err := env.Retry(ctx, attempt); err != nil {
		return Cursor{}, 0, err
	}

	return next, elapsed, nil
}

// commitBatch applies one range of keys and advances the cursor, in one
// transaction.
//
// The two commit together on purpose. Committed rows whose cursor was lost
// would be reapplied by the next run, and a cursor committed without its rows
// would skip them; putting both in one transaction makes either impossible
// rather than unlikely.
func (s *Backfill) commitBatch(ctx context.Context, env Env, cursor Cursor) (_ Cursor, err error) {
	tx, err := env.Begin(ctx)
	if err != nil {
		return Cursor{}, err
	}

	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("roll back batch: %w", rollbackErr))
		}
	}()

	low := cursor.Position
	high := low + int64(env.Throttle.Batch())

	result, err := tx.ExecContext(ctx, s.statement, low, high)
	if err != nil {
		return Cursor{}, fmt.Errorf("step %q: %w", s.meta.Name, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return Cursor{}, fmt.Errorf("count rows of step %q: %w", s.meta.Name, err)
	}

	next := Cursor{Position: high, Rows: cursor.Rows + affected}

	state, err := json.Marshal(next)
	if err != nil {
		return Cursor{}, fmt.Errorf("encode checkpoint of step %q: %w", s.meta.Name, err)
	}

	crash.At(crash.MidBatch)

	if err := env.Checkpoint(ctx, tx, state); err != nil {
		return Cursor{}, err
	}

	if err := tx.Commit(); err != nil {
		return Cursor{}, fmt.Errorf("commit batch of step %q: %w", s.meta.Name, err)
	}

	crash.At(crash.AfterCheckpoint)

	return next, nil
}
