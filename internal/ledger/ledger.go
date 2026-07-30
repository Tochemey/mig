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

// Package ledger records what the catalog cannot know: backfill cursors,
// attempt counts, prior errors and lease ownership.
//
// The ledger is advisory. Whether work is required is decided from the
// catalog, never from a row here. Every mutation is fenced by the caller's
// lease token through [Write] or [Guard].
package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Schema is the schema holding the ledger tables.
const Schema = "mig"

// schemaLockKey serialises concurrent schema creation. CREATE ... IF NOT
// EXISTS is not race-free: two runners starting together can still collide on
// the catalog's unique index.
const schemaLockKey int64 = 7_768_436_744_119_353

// schemaStatements build the ledger, and run in one transaction.
//
// The status columns are diagnostic. Nothing may read them to decide whether
// work is required.
var schemaStatements = []string{
	`CREATE SCHEMA IF NOT EXISTS mig`,

	`CREATE TABLE IF NOT EXISTS mig.migrations (
		id           text PRIMARY KEY,
		name         text NOT NULL,
		status       text NOT NULL,
		started_at   timestamptz,
		finished_at  timestamptz
	)`,

	`CREATE TABLE IF NOT EXISTS mig.steps (
		migration_id text NOT NULL REFERENCES mig.migrations(id),
		idx          int  NOT NULL,
		name         text NOT NULL,
		kind         text NOT NULL,
		checksum     bytea NOT NULL,
		status       text NOT NULL,
		attempts     int  NOT NULL DEFAULT 0,
		checkpoint   jsonb,
		error        text,
		started_at   timestamptz,
		finished_at  timestamptz,
		PRIMARY KEY (migration_id, idx)
	)`,

	// Ownership and expiry are separate rows on purpose.
	//
	// Every ledger write locks the ownership row for the length of its
	// transaction, and a backfill's transaction lasts as long as its batch. If
	// the heartbeat also wrote that row it would queue behind the batch and the
	// holder would give up a lease it still owns — under exactly the load the
	// lease exists to survive. Keeping them apart lets a long batch block a
	// takeover, which is correct, without blocking renewal.
	`CREATE TABLE IF NOT EXISTS mig.lease (
		id    int PRIMARY KEY DEFAULT 1 CHECK (id = 1),
		owner text,
		fence bigint NOT NULL DEFAULT 0
	)`,

	`CREATE TABLE IF NOT EXISTS mig.lease_expiry (
		id           int PRIMARY KEY DEFAULT 1 CHECK (id = 1),
		expires_at   timestamptz,
		heartbeat_at timestamptz
	)`,

	// The singleton rows must exist before anyone can contend for them.
	`INSERT INTO mig.lease (id) VALUES (1) ON CONFLICT (id) DO NOTHING`,
	`INSERT INTO mig.lease_expiry (id) VALUES (1) ON CONFLICT (id) DO NOTHING`,
}

// EnsureSchema creates the ledger schema if it is absent. It is idempotent and
// safe to call from several runners at once.
func EnsureSchema(ctx context.Context, db *sql.DB) (err error) {
	tx, err := db.BeginTx(ctx, TxOptions())
	if err != nil {
		return fmt.Errorf("begin ledger schema transaction: %w", err)
	}

	defer func() {
		err = errors.Join(err, rollback(tx))
	}()

	// Transaction-scoped, so the commit below releases it and a runner dying
	// here cannot leave it held.
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", schemaLockKey); err != nil {
		return fmt.Errorf("lock ledger schema: %w", err)
	}

	for _, stmt := range schemaStatements {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("create ledger schema: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit ledger schema: %w", err)
	}

	return nil
}
