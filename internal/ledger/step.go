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

package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// StepKey identifies a step within a migration.
type StepKey struct {
	MigrationID string
	Index       int
}

// String renders a step key for logs and errors.
func (k StepKey) String() string {
	return fmt.Sprintf("%s[%d]", k.MigrationID, k.Index)
}

// Step is a step's ledger row.
type Step struct {
	Key      StepKey
	Name     string
	Kind     string
	Checksum []byte
	Status   Status
	Attempts int
	Error    string
}

// Attempted reports whether any runner has previously worked on this step.
//
// It is the trigger for reconciliation, not an authority on what happened. A
// row that exists at all means an earlier run reached this step, and the
// database may therefore be part-way through the work.
func (s Step) Attempted() bool {
	return s.Attempts > 0 || s.Status == StatusRunning || s.Status == StatusFailed
}

const upsertStepQuery = `
INSERT INTO mig.steps (migration_id, idx, name, kind, checksum, status)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (migration_id, idx) DO UPDATE
   SET name = excluded.name, kind = excluded.kind`

// UpsertStep records a step as pending without disturbing the progress of one
// already recorded. It must be called inside a fenced transaction.
//
// The checksum is deliberately not updated: comparing the recorded checksum
// against the current one is how drift is detected, and overwriting it here
// would erase the evidence.
func UpsertStep(ctx context.Context, tx *sql.Tx, key StepKey, name, kind string, checksum []byte) error {
	_, err := tx.ExecContext(ctx, upsertStepQuery,
		key.MigrationID, key.Index, name, kind, checksum, StatusPending)
	if err != nil {
		return fmt.Errorf("upsert step %s: %w", key, err)
	}

	return nil
}

const setStepStatusQuery = `
UPDATE mig.steps
   SET status      = $3,
       error       = $4,
       started_at  = CASE WHEN $3 = 'running' THEN now() ELSE started_at END,
       finished_at = CASE WHEN $3 IN ('succeeded', 'failed') THEN now() ELSE finished_at END
 WHERE migration_id = $1 AND idx = $2
RETURNING idx`

// SetStepStatus records a step's status, and its error when it failed. It must
// be called inside a fenced transaction.
func SetStepStatus(ctx context.Context, tx *sql.Tx, key StepKey, status Status, cause string) error {
	var idx int

	err := tx.QueryRowContext(ctx, setStepStatusQuery, key.MigrationID, key.Index, status, cause).Scan(&idx)

	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("set step %s status to %q: %w", key, status, ErrNotRecorded)
	}

	if err != nil {
		return fmt.Errorf("set step %s status to %q: %w", key, status, err)
	}

	return nil
}

const incrementAttemptsQuery = `
UPDATE mig.steps
   SET attempts = attempts + 1
 WHERE migration_id = $1 AND idx = $2
RETURNING attempts`

// IncrementAttempts records that a runner is about to do the work, and returns
// the new count. It must be called inside a fenced transaction.
//
// It commits before the work starts, so that a crash during the work leaves
// evidence that an attempt was made even though nothing else was recorded.
func IncrementAttempts(ctx context.Context, tx *sql.Tx, key StepKey) (int, error) {
	var attempts int

	err := tx.QueryRowContext(ctx, incrementAttemptsQuery, key.MigrationID, key.Index).Scan(&attempts)

	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("increment attempts of step %s: %w", key, ErrNotRecorded)
	}

	if err != nil {
		return 0, fmt.Errorf("increment attempts of step %s: %w", key, err)
	}

	return attempts, nil
}

const loadStepQuery = `
SELECT name, kind, checksum, status, attempts, coalesce(error, '')
  FROM mig.steps
 WHERE migration_id = $1 AND idx = $2`

// LoadStep reads a step's ledger row, returning [ErrNotRecorded] when there is
// none.
func LoadStep(ctx context.Context, db *sql.DB, key StepKey) (Step, error) {
	step := Step{Key: key}

	err := db.QueryRowContext(ctx, loadStepQuery, key.MigrationID, key.Index).
		Scan(&step.Name, &step.Kind, &step.Checksum, &step.Status, &step.Attempts, &step.Error)

	if errors.Is(err, sql.ErrNoRows) {
		return Step{}, fmt.Errorf("load step %s: %w", key, ErrNotRecorded)
	}

	if err != nil {
		return Step{}, fmt.Errorf("load step %s: %w", key, err)
	}

	return step, nil
}

const setCheckpointQuery = `
UPDATE mig.steps
   SET checkpoint = $3
 WHERE migration_id = $1 AND idx = $2
RETURNING idx`

// SetCheckpoint records a resumable step's progress. It must be called inside
// the transaction that performs the work the progress describes.
//
// Committing the two together is what makes "the work landed but the cursor did
// not" impossible. An index build cannot be made atomic with its ledger row; a
// batch and its cursor can, so they are.
func SetCheckpoint(ctx context.Context, tx *sql.Tx, key StepKey, state []byte) error {
	var idx int

	err := tx.QueryRowContext(ctx, setCheckpointQuery, key.MigrationID, key.Index, state).Scan(&idx)

	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("checkpoint step %s: %w", key, ErrNotRecorded)
	}

	if err != nil {
		return fmt.Errorf("checkpoint step %s: %w", key, err)
	}

	return nil
}

const loadCheckpointQuery = `
SELECT coalesce(checkpoint, 'null'::jsonb)
  FROM mig.steps
 WHERE migration_id = $1 AND idx = $2`

// LoadCheckpoint reads a resumable step's progress, returning nil when it has
// none yet.
func LoadCheckpoint(ctx context.Context, db *sql.DB, key StepKey) ([]byte, error) {
	var state []byte

	err := db.QueryRowContext(ctx, loadCheckpointQuery, key.MigrationID, key.Index).Scan(&state)

	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("load checkpoint of step %s: %w", key, ErrNotRecorded)
	}

	if err != nil {
		return nil, fmt.Errorf("load checkpoint of step %s: %w", key, err)
	}

	if string(state) == "null" {
		return nil, nil
	}

	return state, nil
}
