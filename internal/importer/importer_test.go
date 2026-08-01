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
	"context"
	"database/sql"
	"errors"
	"os"
	"slices"
	"testing"
	"testing/fstest"
	"time"

	"github.com/tochemey/mig/internal/importer"
	"github.com/tochemey/mig/internal/lease"
	"github.com/tochemey/mig/internal/ledger"
	"github.com/tochemey/mig/internal/plan"
	"github.com/tochemey/mig/test/harness"
)

// The directory an adopting repository already has: sequential versions, one
// file each, contents untouched by the import.
var adopted = fstest.MapFS{
	"1_create_users.sql": &fstest.MapFile{Data: []byte(
		"CREATE TABLE users (id bigint PRIMARY KEY);\n")},
	"2_add_email.sql": &fstest.MapFile{Data: []byte(
		"ALTER TABLE users ADD COLUMN email text;\n")},
	"10_add_phone.sql": &fstest.MapFile{Data: []byte(
		"ALTER TABLE users ADD COLUMN phone text;\n")},
}

// shared is the container for this package, or nil when docker is absent.
var shared *harness.Harness

// TestMain brings up one container for the package.
func TestMain(m *testing.M) {
	os.Exit(harness.Main(m, 0, func(_ context.Context, h *harness.Harness) error {
		shared = h
		return nil
	}))
}

// TestGooseHistoryIsAdopted covers the path that matters: what goose applied is
// recorded as succeeded, and what it did not is left to reconcile.
func TestGooseHistoryIsAdopted(t *testing.T) {
	db := newDatabase(t)

	createGoose(t, db)
	applyGoose(t, db, 1, 2)

	report := importHistory(t, db, importer.Goose)

	if !slices.Equal(report.Adopted, []string{"1_create_users", "2_add_email"}) {
		t.Fatalf("adopted %v", report.Adopted)
	}

	if !slices.Equal(report.Recheck, []string{"10_add_phone"}) {
		t.Fatalf("recheck %v", report.Recheck)
	}

	assertStatus(t, db, "1_create_users", ledger.StatusSucceeded)
	assertStatus(t, db, "2_add_email", ledger.StatusSucceeded)
	assertStatus(t, db, "10_add_phone", "")
}

// TestGooseRollbackIsNotAdopted covers the reason goose's newest row per
// version is the only one that counts. A migration applied and then rolled back
// is outstanding, and adopting it would skip it forever.
func TestGooseRollbackIsNotAdopted(t *testing.T) {
	db := newDatabase(t)

	createGoose(t, db)
	applyGoose(t, db, 1, 2)
	rollbackGoose(t, db, 2)

	report := importHistory(t, db, importer.Goose)

	if !slices.Equal(report.Adopted, []string{"1_create_users"}) {
		t.Fatalf("adopted %v, want the rolled-back migration left out", report.Adopted)
	}

	assertStatus(t, db, "2_add_email", "")
}

// TestGooseUnknownVersionIsReported covers a history holding a version whose
// file has since been deleted. It is named, not refused.
func TestGooseUnknownVersionIsReported(t *testing.T) {
	db := newDatabase(t)

	createGoose(t, db)
	applyGoose(t, db, 1, 99)

	report := importHistory(t, db, importer.Goose)

	if !slices.Equal(report.Unknown, []int64{99}) {
		t.Fatalf("unknown %v, want [99]", report.Unknown)
	}

	if !slices.Equal(report.Adopted, []string{"1_create_users"}) {
		t.Fatalf("adopted %v", report.Adopted)
	}
}

// TestGolangMigrateHistoryIsAdopted covers the high-water mark: everything at
// or below the recorded version is applied.
func TestGolangMigrateHistoryIsAdopted(t *testing.T) {
	db := newDatabase(t)

	createGolangMigrate(t, db, 2, false)

	report := importHistory(t, db, importer.GolangMigrate)

	if !slices.Equal(report.Adopted, []string{"1_create_users", "2_add_email"}) {
		t.Fatalf("adopted %v", report.Adopted)
	}

	if !slices.Equal(report.Recheck, []string{"10_add_phone"}) {
		t.Fatalf("recheck %v", report.Recheck)
	}

	if report.Dirty {
		t.Fatal("a clean history was reported dirty")
	}
}

// TestGolangMigrateDirtyIsLeftToTheCatalog covers the flag that stops every
// golang-migrate deployment until a human clears it. The version it names is
// reported and left outstanding, so the next run reconciles it.
func TestGolangMigrateDirtyIsLeftToTheCatalog(t *testing.T) {
	db := newDatabase(t)

	createGolangMigrate(t, db, 2, true)

	report := importHistory(t, db, importer.GolangMigrate)

	if !report.Dirty || report.DirtyVersion != 2 {
		t.Fatalf("dirty %v at %d, want true at 2", report.Dirty, report.DirtyVersion)
	}

	if !slices.Equal(report.Adopted, []string{"1_create_users"}) {
		t.Fatalf("adopted %v, want the dirty version left out", report.Adopted)
	}

	assertStatus(t, db, "2_add_email", "")
}

// TestGolangMigrateEmptyHistoryAdoptsNothing covers a tool that was set up and
// never ran. Everything is outstanding, which is not an error.
func TestGolangMigrateEmptyHistoryAdoptsNothing(t *testing.T) {
	db := newDatabase(t)

	createGolangMigrate(t, db, 0, false)

	if _, err := db.ExecContext(t.Context(), "DELETE FROM schema_migrations"); err != nil {
		t.Fatalf("empty the history: %v", err)
	}

	report := importHistory(t, db, importer.GolangMigrate)

	if len(report.Adopted) != 0 {
		t.Fatalf("adopted %v from an empty history", report.Adopted)
	}

	if len(report.Recheck) != 3 {
		t.Fatalf("recheck %v, want every migration", report.Recheck)
	}
}

// TestImportIsRepeatable covers running it twice, which is what happens when
// the first run is killed part way through.
func TestImportIsRepeatable(t *testing.T) {
	db := newDatabase(t)

	createGoose(t, db)
	applyGoose(t, db, 1, 2)

	first := importHistory(t, db, importer.Goose)
	second := importHistory(t, db, importer.Goose)

	if !slices.Equal(first.Adopted, second.Adopted) {
		t.Fatalf("second import adopted %v, want %v", second.Adopted, first.Adopted)
	}
}

// TestMissingHistoryIsReported covers pointing an import at a database the
// other tool never ran against.
func TestMissingHistoryIsReported(t *testing.T) {
	db := newDatabase(t)

	for _, source := range importer.Sources() {
		t.Run(string(source), func(t *testing.T) {
			if _, err := importer.Read(t.Context(), db, source); !errors.Is(err, importer.ErrNoHistory) {
				t.Fatalf("read returned %v, want ErrNoHistory", err)
			}
		})
	}
}

// TestUnknownSourceIsRefused covers a --from nobody has an adapter for.
func TestUnknownSourceIsRefused(t *testing.T) {
	db := newDatabase(t)

	if _, err := importer.Read(t.Context(), db, "flyway"); !errors.Is(err, importer.ErrUnknownSource) {
		t.Fatalf("read returned %v, want ErrUnknownSource", err)
	}
}

// TestReadRejectsAnUnusableHistoryTable covers a table of the right name whose
// columns are not the ones the adapter reads.
func TestReadRejectsAnUnusableHistoryTable(t *testing.T) {
	cases := map[string]struct {
		create string
		source importer.Source
	}{
		"goose": {
			create: "CREATE TABLE goose_db_version (nonsense text)",
			source: importer.Goose,
		},
		"golang-migrate": {
			create: "CREATE TABLE schema_migrations (nonsense text)",
			source: importer.GolangMigrate,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			db := newDatabase(t)

			if _, err := db.ExecContext(t.Context(), tc.create); err != nil {
				t.Fatalf("create the table: %v", err)
			}

			if _, err := importer.Read(t.Context(), db, tc.source); err == nil {
				t.Fatal("read accepted a table it cannot understand")
			}
		})
	}
}

// TestCoversWithoutADatabase covers the two history shapes directly, including
// the ceiling a tool with no rows leaves behind.
func TestCoversWithoutADatabase(t *testing.T) {
	cases := map[string]struct {
		history importer.History
		version int64
		want    bool
	}{
		"set holds it":        {importer.History{Applied: []int64{1, 3}}, 3, true},
		"set has a hole":      {importer.History{Applied: []int64{1, 3}}, 2, false},
		"ceiling covers":      {importer.History{Ceiling: true, Through: 3}, 3, true},
		"ceiling stops":       {importer.History{Ceiling: true, Through: 3}, 4, false},
		"ceiling covers none": {importer.History{Ceiling: true, Through: -1}, 0, false},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := tc.history.Covers(tc.version); got != tc.want {
				t.Fatalf("covers(%d) is %v, want %v", tc.version, got, tc.want)
			}
		})
	}
}

// importHistory runs an import under a real lease, as the command does.
func importHistory(t *testing.T, db *sql.DB, source importer.Source) importer.Report {
	t.Helper()

	loaded, err := plan.LoadFS(adopted)
	if err != nil {
		t.Fatalf("load the plan: %v", err)
	}

	if err := ledger.EnsureSchema(t.Context(), db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	held, err := lease.Acquire(t.Context(), db, lease.Config{
		Owner:    lease.NewOwner(),
		TTL:      time.Minute,
		OnLocked: lease.Fail,
	})
	if err != nil {
		t.Fatalf("acquire the lease: %v", err)
	}

	report, importErr := importer.Import(t.Context(), db, held.Fence(), loaded, source)

	// Released here rather than at cleanup, so that a test importing twice
	// contends with nothing but itself.
	if err := held.Release(t.Context()); err != nil {
		t.Fatalf("release the lease: %v", err)
	}

	if importErr != nil {
		t.Fatalf("import: %v", importErr)
	}

	return report
}

// assertStatus checks what the ledger now says about a migration's only step.
// An empty want means no row at all.
func assertStatus(t *testing.T, db *sql.DB, id string, want ledger.Status) {
	t.Helper()

	key := ledger.StepKey{MigrationID: id, Index: 0}

	row, err := ledger.LoadStep(t.Context(), db, key)

	if want == "" {
		if !errors.Is(err, ledger.ErrNotRecorded) {
			t.Fatalf("step %q returned %v (%q), want no row", id, err, row.Status)
		}

		return
	}

	if err != nil {
		t.Fatalf("load step %q: %v", id, err)
	}

	if row.Status != want {
		t.Fatalf("step %q is %q, want %q", id, row.Status, want)
	}
}

// createGoose builds the history table goose keeps.
func createGoose(t *testing.T, db *sql.DB) {
	t.Helper()

	const ddl = `
CREATE TABLE goose_db_version (
    id serial PRIMARY KEY,
    version_id bigint NOT NULL,
    is_applied boolean NOT NULL,
    tstamp timestamp DEFAULT now()
)`

	exec(t, db, ddl)

	// Goose's own bootstrap row, which never matches a file.
	exec(t, db, "INSERT INTO goose_db_version (version_id, is_applied) VALUES (0, true)")
}

// applyGoose records versions as applied, as goose does on the way up.
func applyGoose(t *testing.T, db *sql.DB, versions ...int64) {
	t.Helper()

	for _, version := range versions {
		exec(t, db, "INSERT INTO goose_db_version (version_id, is_applied) VALUES ($1, true)", version)
	}
}

// rollbackGoose appends the row goose writes on the way down.
func rollbackGoose(t *testing.T, db *sql.DB, version int64) {
	t.Helper()

	exec(t, db, "INSERT INTO goose_db_version (version_id, is_applied) VALUES ($1, false)", version)
}

// createGolangMigrate builds the single-row table golang-migrate keeps.
func createGolangMigrate(t *testing.T, db *sql.DB, version int64, dirty bool) {
	t.Helper()

	exec(t, db, "CREATE TABLE schema_migrations (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL)")
	exec(t, db, "INSERT INTO schema_migrations (version, dirty) VALUES ($1, $2)", version, dirty)
}

// exec runs a setup statement and fails the test if it does not.
func exec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()

	if _, err := db.ExecContext(t.Context(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// newDatabase gives the test its own database and a pool onto it.
func newDatabase(t *testing.T) *sql.DB {
	t.Helper()

	return openOn(t, newNamedDatabase(t))
}

// newNamedDatabase gives the test its own database, for a case that needs to
// open more than one pool onto it.
func newNamedDatabase(t *testing.T) string {
	t.Helper()

	if shared == nil {
		t.Skip("postgres container not available")
	}

	name, err := shared.Clone(t.Context(), harness.Template)
	if err != nil {
		t.Fatalf("clone template: %v", err)
	}

	t.Cleanup(func() {
		if err := shared.DropDatabase(context.Background(), name); err != nil {
			t.Errorf("drop clone %q: %v", name, err)
		}
	})

	return name
}

// openOn connects to a database that already exists.
func openOn(t *testing.T, name string) *sql.DB {
	t.Helper()

	db, err := shared.Open(t.Context(), name)
	if err != nil {
		t.Fatalf("open %q: %v", name, err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close pool: %v", err)
		}
	})

	return db
}
