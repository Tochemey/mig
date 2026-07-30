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

package importer_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/tochemey/mig/internal/importer"
	"github.com/tochemey/mig/internal/lease"
	"github.com/tochemey/mig/internal/ledger"
	"github.com/tochemey/mig/internal/plan"
	"github.com/tochemey/mig/test/faultdb"
)

// An import that half worked is the failure with no recovery: a migration
// adopted but not applied is skipped forever, and nothing is left to notice it.
// Every failure below therefore has to surface rather than be reported as a
// partial success.

// TestReadReportsEveryFailedQuery covers the reads that find and interpret the
// other tool's history.
func TestReadReportsEveryFailedQuery(t *testing.T) {
	cases := map[string]struct {
		source importer.Source
		setup  func(*testing.T, *sql.DB)
		fault  faultdb.Fault
	}{
		"finding the goose table": {
			source: importer.Goose,
			setup:  createGoose,
			fault:  faultdb.Fault{Op: faultdb.OpQuery, Match: "to_regclass('goose_db_version')"},
		},
		"reading the goose rows": {
			source: importer.Goose,
			setup:  createGoose,
			fault:  faultdb.Fault{Op: faultdb.OpQuery, Match: "version_id"},
		},
		"finding the migrate table": {
			source: importer.GolangMigrate,
			setup:  func(t *testing.T, db *sql.DB) { createGolangMigrate(t, db, 1, false) },
			fault:  faultdb.Fault{Op: faultdb.OpQuery, Match: "to_regclass('schema_migrations')"},
		},
		"reading the migrate row": {
			source: importer.GolangMigrate,
			setup:  func(t *testing.T, db *sql.DB) { createGolangMigrate(t, db, 1, false) },
			fault:  faultdb.Fault{Op: faultdb.OpQuery, Match: "FROM schema_migrations"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			database := newNamedDatabase(t)

			tc.setup(t, openOn(t, database))

			faults := faultdb.NewFaults(tc.fault)

			if _, err := importer.Read(t.Context(), openFaulted(t, database, faults), tc.source); err == nil {
				t.Fatalf("read reported success with %q broken", name)
			}

			if faults.Fired() == 0 {
				t.Fatalf("no fault was taken, so %q proves nothing", name)
			}
		})
	}
}

// TestAdoptReportsEveryFailedWrite covers the writes that mark a migration as
// already succeeded.
func TestAdoptReportsEveryFailedWrite(t *testing.T) {
	cases := map[string]faultdb.Fault{
		"recording the migration": {Op: faultdb.OpExec, Match: "INSERT INTO mig.migrations"},
		"recording the step":      {Op: faultdb.OpExec, Match: "INSERT INTO mig.steps"},
		"marking the step done":   {Op: faultdb.OpQuery, Match: "UPDATE mig.steps"},
		"marking it succeeded":    {Op: faultdb.OpQuery, Match: "UPDATE mig.migrations"},
		"committing":              {Op: faultdb.OpCommit},
	}

	for name, fault := range cases {
		t.Run(name, func(t *testing.T) {
			database := newNamedDatabase(t)
			control := openOn(t, database)

			createGoose(t, control)
			applyGoose(t, control, 1, 2)

			loaded, err := plan.LoadFS(adopted)
			if err != nil {
				t.Fatalf("load the plan: %v", err)
			}

			if err := ledger.EnsureSchema(t.Context(), control); err != nil {
				t.Fatalf("ensure schema: %v", err)
			}

			held, err := lease.Acquire(t.Context(), control, lease.Config{
				Owner:    lease.NewOwner(),
				TTL:      time.Minute,
				OnLocked: lease.Fail,
			})
			if err != nil {
				t.Fatalf("acquire the lease: %v", err)
			}

			faults := faultdb.NewFaults(fault)
			db := openFaulted(t, database, faults)

			_, importErr := importer.Import(t.Context(), db, held.Fence(), loaded, importer.Goose)

			if err := held.Release(t.Context()); err != nil {
				t.Fatalf("release the lease: %v", err)
			}

			if importErr == nil {
				t.Fatalf("the import reported success with %q broken", name)
			}

			if faults.Fired() == 0 {
				t.Fatalf("no fault was taken, so %q proves nothing", name)
			}
		})
	}
}

// openFaulted connects through the fault-injecting driver.
//
// Every pool gets its own driver name, because database/sql keeps its drivers
// in a process-wide registry that cannot be replaced.
func openFaulted(t *testing.T, database string, faults *faultdb.Faults) *sql.DB {
	t.Helper()

	db, err := faultdb.Open(fmt.Sprintf("faulted_%s_%s", t.Name(), database),
		shared.DSN(database), faults)
	if err != nil {
		t.Fatalf("open %q with faults: %v", database, err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close pool: %v", err)
		}
	})

	return db
}
