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
	"testing"

	"github.com/tochemey/mig/internal/ledger"
)

// TestReportReadsEveryRecordedStep covers the read behind the status command,
// including the checkpoint and error columns a catalog cannot report.
func TestReportReadsEveryRecordedStep(t *testing.T) {
	db := newLedger(t)

	fence := takeLease(t, db, heldFence)

	record := func(ctx context.Context, tx *sql.Tx) error {
		if err := ledger.UpsertMigration(ctx, tx, "20240817120000_add_email", "add_email"); err != nil {
			return err
		}

		key := ledger.StepKey{MigrationID: "20240817120000_add_email", Index: 0}

		if err := ledger.UpsertStep(ctx, tx, key, "add_email", "ddl_tx", []byte("sum")); err != nil {
			return err
		}

		if _, err := ledger.IncrementAttempts(ctx, tx, key); err != nil {
			return err
		}

		return ledger.SetStepStatus(ctx, tx, key, ledger.StatusFailed, "it did not work")
	}

	if err := ledger.Write(t.Context(), db, fence, record); err != nil {
		t.Fatalf("record the step: %v", err)
	}

	report, err := ledger.Report(t.Context(), db)
	if err != nil {
		t.Fatalf("report: %v", err)
	}

	if len(report) != 1 {
		t.Fatalf("report is %+v, want one step", report)
	}

	step := report[0]

	if step.Name != "add_email" || step.Status != ledger.StatusFailed {
		t.Fatalf("step is %+v", step)
	}

	if step.Attempts != 1 || step.Error != "it did not work" {
		t.Fatalf("step is %+v, want the attempt and the cause recorded", step)
	}
}

// TestReportOfAnUntouchedDatabaseIsEmpty covers a database no run has reached.
// It has no ledger at all, which is nothing recorded rather than a failure.
func TestReportOfAnUntouchedDatabaseIsEmpty(t *testing.T) {
	db := newDatabase(t)

	report, err := ledger.Report(t.Context(), db)
	if err != nil {
		t.Fatalf("report: %v", err)
	}

	if len(report) != 0 {
		t.Fatalf("report is %+v, want nothing", report)
	}
}

// TestReportRejectsAClosedDatabase covers the read failing, which must not be
// reported as a database with nothing recorded.
func TestReportRejectsAClosedDatabase(t *testing.T) {
	db := newLedger(t)

	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, err := ledger.Report(t.Context(), db); err == nil {
		t.Fatal("report accepted a closed database")
	}

	if _, err := ledger.Present(t.Context(), db); err == nil {
		t.Fatal("the ledger check accepted a closed database")
	}
}

// The writes below are all guarded by a row that must already exist. Writing to
// a step nobody recorded has to be reported: silently updating nothing would
// leave a run believing it had saved progress it had not.

// TestWritesToAnUnrecordedStepAreRefused covers each of them.
func TestWritesToAnUnrecordedStepAreRefused(t *testing.T) {
	db := newLedger(t)
	fence := takeLease(t, db, heldFence)

	key := ledger.StepKey{MigrationID: "never_recorded", Index: 0}

	cases := map[string]func(context.Context, *sql.Tx) error{
		"status": func(ctx context.Context, tx *sql.Tx) error {
			return ledger.SetStepStatus(ctx, tx, key, ledger.StatusSucceeded, "")
		},
		"attempts": func(ctx context.Context, tx *sql.Tx) error {
			_, err := ledger.IncrementAttempts(ctx, tx, key)

			return err
		},
		"checkpoint": func(ctx context.Context, tx *sql.Tx) error {
			return ledger.SetCheckpoint(ctx, tx, key, []byte(`{"at":1}`))
		},
		"migration status": func(ctx context.Context, tx *sql.Tx) error {
			return ledger.SetMigrationStatus(ctx, tx, "never_recorded", ledger.StatusSucceeded)
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			err := ledger.Write(t.Context(), db, fence, mutate)

			if !errors.Is(err, ledger.ErrNotRecorded) {
				t.Fatalf("write returned %v, want ErrNotRecorded", err)
			}
		})
	}
}

// TestLoadCheckpointOfAnUnrecordedStepIsRefused covers the read that resumes a
// backfill. A missing row is not an empty checkpoint: resuming from the start
// would redo work the ledger cannot account for.
func TestLoadCheckpointOfAnUnrecordedStepIsRefused(t *testing.T) {
	db := newLedger(t)

	key := ledger.StepKey{MigrationID: "never_recorded", Index: 0}

	if _, err := ledger.LoadCheckpoint(t.Context(), db, key); !errors.Is(err, ledger.ErrNotRecorded) {
		t.Fatalf("load returned %v, want ErrNotRecorded", err)
	}
}
