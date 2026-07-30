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

package mig_test

import (
	"errors"
	"slices"
	"testing"
	"testing/fstest"

	"github.com/tochemey/mig/pkg/mig"
)

// TestImportAdoptsAndThenRunsClean covers the whole adoption path: the history
// says the column is there, and the run that follows does not try to add it.
func TestImportAdoptsAndThenRunsClean(t *testing.T) {
	db := newDatabase(t)

	// The state goose leaves behind.
	apply(t, db,
		"ALTER TABLE users ADD COLUMN email text",
		`CREATE TABLE goose_db_version (
			id serial PRIMARY KEY,
			version_id bigint NOT NULL,
			is_applied boolean NOT NULL,
			tstamp timestamp DEFAULT now())`,
		"INSERT INTO goose_db_version (version_id, is_applied) VALUES (20240817120000, true)")

	report, err := mig.Import(t.Context(), db, migrations(t), mig.Goose, mig.Options{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	if !slices.Equal(report.Adopted, []string{"20240817120000_add_email"}) {
		t.Fatalf("adopted %v", report.Adopted)
	}

	if !slices.Equal(report.Recheck, []string{"20240817120001_index_email"}) {
		t.Fatalf("recheck %v", report.Recheck)
	}

	summary, err := mig.Up(t.Context(), db, migrations(t), mig.Options{})
	if err != nil {
		t.Fatalf("up after import: %v", err)
	}

	if summary.Applied != 1 {
		t.Fatalf("the run applied %d steps, want only the unadopted one", summary.Applied)
	}
}

// TestImportRejectsAnUnknownSource covers a caller naming a tool no adapter
// handles.
func TestImportRejectsAnUnknownSource(t *testing.T) {
	db := newDatabase(t)

	_, err := mig.Import(t.Context(), db, migrations(t), "flyway", mig.Options{})
	if err == nil {
		t.Fatal("import accepted a source with no adapter")
	}
}

// TestImportRejectsAnUnloadableSource covers the migration files being
// unreadable, which must fail before the lease is taken.
func TestImportRejectsAnUnloadableSource(t *testing.T) {
	db := newDatabase(t)

	_, err := mig.Import(t.Context(), db, fstest.MapFS{}, mig.Goose, mig.Options{})
	if err == nil {
		t.Fatal("import accepted a source with no migrations")
	}
}

// TestImportReportsBeingLocked covers an import meeting a run in progress. Two
// writers to one ledger is what the lease exists to prevent.
func TestImportReportsBeingLocked(t *testing.T) {
	db := newDatabase(t)

	release := holdLease(t, db)
	defer release()

	_, err := mig.Import(t.Context(), db, migrations(t), mig.GolangMigrate,
		mig.Options{OnLocked: mig.Fail})

	if !errors.Is(err, mig.ErrLocked) {
		t.Fatalf("import returned %v, want ErrLocked", err)
	}
}
