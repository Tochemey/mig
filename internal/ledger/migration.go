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

// Status is the recorded state of a migration or a step. It is diagnostic:
// whether work is required is decided from the catalog.
type Status string

const (
	// StatusPending means the row has been recorded but not started.
	StatusPending Status = "pending"

	// StatusRunning means a runner claimed the work. It does not mean the work
	// happened.
	StatusRunning Status = "running"

	// StatusSucceeded means a runner observed the work as done.
	StatusSucceeded Status = "succeeded"

	// StatusFailed records an error for a human to read.
	StatusFailed Status = "failed"
)

// ErrNotRecorded reports that no ledger row exists for the requested migration.
var ErrNotRecorded = errors.New("migration not recorded in ledger")

// Migration is a migration's ledger row.
type Migration struct {
	ID     string
	Name   string
	Status Status
}

const upsertMigrationQuery = `
INSERT INTO mig.migrations (id, name, status)
VALUES ($1, $2, $3)
ON CONFLICT (id) DO UPDATE SET name = excluded.name`

// UpsertMigration records a migration as pending without disturbing the status
// of one already recorded. It must be called inside a fenced transaction.
func UpsertMigration(ctx context.Context, tx *sql.Tx, id, name string) error {
	if _, err := tx.ExecContext(ctx, upsertMigrationQuery, id, name, StatusPending); err != nil {
		return fmt.Errorf("upsert migration %q: %w", id, err)
	}

	return nil
}

// setMigrationStatusQuery returns a row only when the migration exists, so a
// write against an unrecorded migration is reported rather than lost.
const setMigrationStatusQuery = `
UPDATE mig.migrations
   SET status      = $2,
       started_at  = CASE WHEN $2 = 'running' THEN now() ELSE started_at END,
       finished_at = CASE WHEN $2 IN ('succeeded', 'failed') THEN now() ELSE finished_at END
 WHERE id = $1
RETURNING id`

// SetMigrationStatus records a migration's status. It must be called inside a
// fenced transaction.
func SetMigrationStatus(ctx context.Context, tx *sql.Tx, id string, status Status) error {
	var updated string

	err := tx.QueryRowContext(ctx, setMigrationStatusQuery, id, status).Scan(&updated)

	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("set migration %q status to %q: %w", id, status, ErrNotRecorded)
	}

	if err != nil {
		return fmt.Errorf("set migration %q status to %q: %w", id, status, err)
	}

	return nil
}

const loadMigrationQuery = `SELECT id, name, status FROM mig.migrations WHERE id = $1`

// LoadMigration reads a migration's ledger row, returning [ErrNotRecorded]
// when there is none. Reads are not fenced; only writes need to be.
func LoadMigration(ctx context.Context, db *sql.DB, id string) (Migration, error) {
	var m Migration

	err := db.QueryRowContext(ctx, loadMigrationQuery, id).Scan(&m.ID, &m.Name, &m.Status)

	if errors.Is(err, sql.ErrNoRows) {
		return Migration{}, fmt.Errorf("load migration %q: %w", id, ErrNotRecorded)
	}

	if err != nil {
		return Migration{}, fmt.Errorf("load migration %q: %w", id, err)
	}

	return m, nil
}
