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

package ledger_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/tochemey/mig/internal/ledger"
)

// Losing the database mid-run is ordinary rather than exotic: a failover, a
// restart, a connection reaper. These tests drive those paths.

// TestEnsureSchemaOnClosedDatabase covers the first thing a run does failing.
func TestEnsureSchemaOnClosedDatabase(t *testing.T) {
	db := newDatabase(t)
	closeDatabase(t, db)

	if err := ledger.EnsureSchema(t.Context(), db); err == nil {
		t.Fatal("ensure schema on a closed database returned no error")
	}
}

// TestEnsureSchemaRejectsIncompatibleTables covers a ledger half-created by
// hand or by an older version. mig.steps references mig.migrations, so a
// migrations table without its primary key cannot support the foreign key.
func TestEnsureSchemaRejectsIncompatibleTables(t *testing.T) {
	db := newDatabase(t)
	ctx := t.Context()

	for _, stmt := range []string{
		`CREATE SCHEMA mig`,
		`CREATE TABLE mig.migrations (id text, name text, status text)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("prepare incompatible ledger: %v", err)
		}
	}

	err := ledger.EnsureSchema(ctx, db)
	if err == nil {
		t.Fatal("ensure schema accepted an incompatible ledger")
	}

	if !strings.Contains(err.Error(), "create ledger schema") {
		t.Fatalf("error does not identify the failing stage: %v", err)
	}
}

// TestWriteOnClosedDatabase covers a fenced write that cannot even begin.
func TestWriteOnClosedDatabase(t *testing.T) {
	db := newLedger(t)
	fence := takeLease(t, db, heldFence)

	closeDatabase(t, db)

	err := ledger.Write(t.Context(), db, fence, func(context.Context, *sql.Tx) error {
		return nil
	})

	if err == nil {
		t.Fatal("write on a closed database returned no error")
	}
}

// TestWriteReportsCommitFailure covers a transaction that fails at COMMIT
// rather than at a statement. A deferred constraint produces one reliably: the
// violation is detected only when the transaction tries to commit.
func TestWriteReportsCommitFailure(t *testing.T) {
	db := newLedger(t)
	fence := takeLease(t, db, heldFence)

	err := ledger.Write(t.Context(), db, fence, func(ctx context.Context, tx *sql.Tx) error {
		for _, stmt := range []string{
			`CREATE TABLE deferred (id int)`,
			`ALTER TABLE deferred ADD CONSTRAINT deferred_id UNIQUE (id) DEFERRABLE INITIALLY DEFERRED`,
			`INSERT INTO deferred VALUES (1), (1)`,
		} {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}

		return nil
	})

	if err == nil {
		t.Fatal("write whose commit failed returned no error")
	}

	if !strings.Contains(err.Error(), "commit fenced write") {
		t.Fatalf("error does not identify the failing stage: %v", err)
	}
}

// TestGuardReportsQueryFailure covers the fence check itself failing, which is
// not the same as the fence passing.
func TestGuardReportsQueryFailure(t *testing.T) {
	db := newLedger(t)
	fence := takeLease(t, db, heldFence)

	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	err = ledger.Guard(t.Context(), tx, fence)

	if err == nil || errors.Is(err, ledger.ErrFenced) {
		t.Fatalf("guard on a finished transaction returned %v, want a query error", err)
	}
}

// TestRowWritesReportFailure covers the two row writers failing at the
// statement level rather than at the fence.
func TestRowWritesReportFailure(t *testing.T) {
	db := newLedger(t)

	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	if err := ledger.UpsertMigration(t.Context(), tx, "id", "name"); err == nil {
		t.Fatal("upsert on a finished transaction returned no error")
	}

	err = ledger.SetMigrationStatus(t.Context(), tx, "id", ledger.StatusSucceeded)

	if err == nil || errors.Is(err, ledger.ErrNotRecorded) {
		t.Fatalf("status write on a finished transaction returned %v, want a query error", err)
	}
}

// TestLoadMigrationReportsQueryFailure checks that a broken read is not
// reported as a migration that was never recorded.
func TestLoadMigrationReportsQueryFailure(t *testing.T) {
	db := newLedger(t)
	closeDatabase(t, db)

	_, err := ledger.LoadMigration(t.Context(), db, "anything")

	if err == nil || errors.Is(err, ledger.ErrNotRecorded) {
		t.Fatalf("load from a closed database returned %v, want a query error", err)
	}
}

// TestFenceString covers the identifier that appears in every fencing error.
func TestFenceString(t *testing.T) {
	fence := ledger.Fence{Owner: "host/42/abc", Token: 7}

	if got := fence.String(); got != "host/42/abc#7" {
		t.Fatalf("fence renders as %q", got)
	}
}

// closeDatabase closes a pool and defuses the cleanup that would close it again.
func closeDatabase(t *testing.T, db *sql.DB) {
	t.Helper()

	if err := db.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}
}
