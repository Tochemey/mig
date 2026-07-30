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

package plan_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/tochemey/mig/internal/plan"
	"github.com/tochemey/mig/internal/step"
)

// TestLoadReadsStepsInOrder covers the ordinary case: several annotated steps
// in one file, in the order they were written.
func TestLoadReadsStepsInOrder(t *testing.T) {
	dir := write(t, map[string]string{
		"20240817120000_add_email.sql": `
-- +mig step: add_column
ALTER TABLE users ADD COLUMN email text;

-- +mig step: index_email
-- +mig notx
CREATE INDEX CONCURRENTLY idx_users_email ON users (email);
`,
	})

	loaded, err := plan.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if len(loaded.Migrations) != 1 {
		t.Fatalf("got %d migrations, want 1", len(loaded.Migrations))
	}

	migration := loaded.Migrations[0]

	if migration.ID != "20240817120000_add_email" {
		t.Fatalf("id is %q", migration.ID)
	}

	if migration.Version != 20240817120000 || migration.Name != "add_email" {
		t.Fatalf("version %d name %q", migration.Version, migration.Name)
	}

	if len(migration.Steps) != 2 {
		t.Fatalf("got %d steps, want 2", len(migration.Steps))
	}

	first, second := migration.Steps[0], migration.Steps[1]

	if first.Name != "add_column" || first.Kind != step.KindDDLTx {
		t.Fatalf("first step is %q kind %q", first.Name, first.Kind)
	}

	if second.Name != "index_email" || second.Kind != step.KindDDLNoTx {
		t.Fatalf("second step is %q kind %q", second.Name, second.Kind)
	}

	if second.Index != 1 {
		t.Fatalf("second step has index %d, want 1", second.Index)
	}

	if len(second.Checksum) == 0 {
		t.Fatal("step has no checksum")
	}
}

// TestLoadOrdersByVersion covers the ordering guarantee. Zero-padded timestamps
// sort chronologically, and the executor applies them in that order.
func TestLoadOrdersByVersion(t *testing.T) {
	dir := write(t, map[string]string{
		"20240901000000_third.sql":  "SELECT 1;\n",
		"20240101000000_first.sql":  "SELECT 1;\n",
		"20240501000000_second.sql": "SELECT 1;\n",
	})

	loaded, err := plan.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	want := []string{"first", "second", "third"}

	for i, migration := range loaded.Migrations {
		if migration.Name != want[i] {
			t.Fatalf("migration %d is %q, want %q", i, migration.Name, want[i])
		}
	}
}

// TestLoadNamesImplicitSteps covers a plain SQL file with no annotations, which
// is what an adopted migration looks like before anyone edits it.
func TestLoadNamesImplicitSteps(t *testing.T) {
	dir := write(t, map[string]string{
		"20240817120000_adopted.sql": "ALTER TABLE users ADD COLUMN email text;\n",
	})

	loaded, err := plan.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	steps := loaded.Migrations[0].Steps

	if len(steps) != 1 || steps[0].Name != "step_1" {
		t.Fatalf("steps are %+v", steps)
	}
}

// TestLoadReadsSatisfiedAnnotation covers the escape hatch for a step whose
// predicate cannot be inferred.
func TestLoadReadsSatisfiedAnnotation(t *testing.T) {
	dir := write(t, map[string]string{
		"20240817120000_vacuum.sql": `
-- +mig step: vacuum_users
-- +mig notx
-- +mig satisfied: sql(SELECT EXISTS (SELECT 1 FROM pg_enum WHERE enumlabel = 'ok'))
VACUUM users;
`,
	})

	loaded, err := plan.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	got := loaded.Migrations[0].Steps[0]

	const want = "SELECT EXISTS (SELECT 1 FROM pg_enum WHERE enumlabel = 'ok')"
	if got.Satisfied != want {
		t.Fatalf("satisfied is %q, want %q", got.Satisfied, want)
	}

	// With an author-supplied predicate the step is buildable even though
	// nothing about VACUUM can be inferred.
	if _, err := got.Build(); err != nil {
		t.Fatalf("build: %v", err)
	}
}

// TestLoadReadsNoLockTimeout covers the annotation that suppresses the default
// lock timeout, which the design requires to be explicit.
func TestLoadReadsNoLockTimeout(t *testing.T) {
	dir := write(t, map[string]string{
		"20240817120000_slow.sql": `
-- +mig step: patient
-- +mig no_lock_timeout
ALTER TABLE users ADD COLUMN email text;
`,
	})

	loaded, err := plan.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !loaded.Migrations[0].Steps[0].NoLockTimeout {
		t.Fatal("no_lock_timeout was not recorded")
	}
}

// TestBuildRefusesUnreconcilableNoTxStep is the plan-time refusal. A
// non-transactional step with no predicate cannot tell finished work from
// unstarted work after a crash, so it is rejected before it can run rather
// than leaving the database in a state nobody can reason about.
func TestBuildRefusesUnreconcilableNoTxStep(t *testing.T) {
	dir := write(t, map[string]string{
		"20240817120000_vacuum.sql": `
-- +mig step: vacuum_users
-- +mig notx
VACUUM users;
`,
	})

	loaded, err := plan.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	_, err = loaded.Migrations[0].Steps[0].Build()
	if !errors.Is(err, step.ErrNoPredicate) {
		t.Fatalf("build returned %v, want ErrNoPredicate", err)
	}
}

// TestBuildAcceptsInferredNoTxStep is the other half: an index build needs no
// annotation, because the statement says everything needed to check it.
func TestBuildAcceptsInferredNoTxStep(t *testing.T) {
	dir := write(t, map[string]string{
		"20240817120000_index.sql": `
-- +mig step: index_email
-- +mig notx
CREATE INDEX CONCURRENTLY idx_users_email ON users (email);
`,
	})

	loaded, err := plan.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	built, err := loaded.Migrations[0].Steps[0].Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if built.Meta().Kind != step.KindDDLNoTx {
		t.Fatalf("built step has kind %q", built.Meta().Kind)
	}
}

// TestBuildAcceptsATransactionalStep covers the default kind. A step with no
// notx annotation runs inside a transaction shared with its ledger write, and
// needs no predicate because there is no window to reconcile.
func TestBuildAcceptsATransactionalStep(t *testing.T) {
	dir := write(t, map[string]string{
		"20240817120000_tx.sql": "ALTER TABLE users ADD COLUMN email text;\n",
	})

	loaded, err := plan.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	built, err := loaded.Migrations[0].Steps[0].Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if built.Meta().Kind != step.KindDDLTx {
		t.Fatalf("built step has kind %q", built.Meta().Kind)
	}

	if _, ok := built.(step.TxStep); !ok {
		t.Fatalf("built step is %T, which does not apply inside a transaction", built)
	}
}

// TestBuildRejectsUnsupportedKinds records what this build cannot run yet, so
// an unimplemented kind fails loudly rather than being silently skipped.
func TestBuildRejectsUnsupportedKinds(t *testing.T) {
	unknown := plan.Step{Name: "mystery", Kind: "teleport"}

	if _, err := unknown.Build(); !errors.Is(err, plan.ErrKindUnsupported) {
		t.Fatalf("build returned %v, want ErrKindUnsupported", err)
	}
}

// TestLoadReadsBackfillAnnotation covers the settings a backfill carries and
// the cursor placeholders, which are not Postgres syntax and so are rewritten
// before anything parses them.
func TestLoadReadsBackfillAnnotation(t *testing.T) {
	dir := write(t, map[string]string{
		"20240817120000_fill.sql": `
-- +mig step: fill_email
-- +mig backfill: table=users key=id batch=250 max_lag_bytes=1048576
-- +mig satisfied: sql(SELECT NOT EXISTS (SELECT 1 FROM users WHERE email IS NULL))
UPDATE users SET email = legacy_email
 WHERE id > :cursor_lo AND id <= :cursor_hi AND email IS NULL;
`,
	})

	loaded, err := plan.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	got := loaded.Migrations[0].Steps[0]

	if got.Kind != step.KindBackfill {
		t.Fatalf("kind is %q, want %q", got.Kind, step.KindBackfill)
	}

	want := step.BackfillConfig{Table: "users", Key: "id", Batch: 250, MaxLagBytes: 1048576}
	if got.Backfill != want {
		t.Fatalf("backfill is %+v, want %+v", got.Backfill, want)
	}

	if !strings.Contains(got.SQL, "$1") || !strings.Contains(got.SQL, "$2") {
		t.Fatalf("cursor placeholders were not rewritten: %s", got.SQL)
	}

	if _, err := got.Build(); err != nil {
		t.Fatalf("build: %v", err)
	}
}

// TestBackfillDefaultsItsBatch covers an annotation that names only what it
// must.
func TestBackfillDefaultsItsBatch(t *testing.T) {
	dir := write(t, map[string]string{
		"20240817120000_fill.sql": `
-- +mig step: fill
-- +mig backfill: table=users key=id
-- +mig satisfied: sql(SELECT true)
UPDATE users SET email = legacy_email WHERE id > :cursor_lo AND id <= :cursor_hi;
`,
	})

	loaded, err := plan.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if got := loaded.Migrations[0].Steps[0].Backfill.Batch; got != plan.DefaultBatch {
		t.Fatalf("batch defaulted to %d, want %d", got, plan.DefaultBatch)
	}
}

// TestBackfillRequiresAPredicate is the refusal that keeps the ledger from
// deciding whether a backfill is done. The cursor lives there, and a cursor
// cannot say whether rows were added behind it.
func TestBackfillRequiresAPredicate(t *testing.T) {
	dir := write(t, map[string]string{
		"20240817120000_fill.sql": `
-- +mig step: fill
-- +mig backfill: table=users key=id
UPDATE users SET email = legacy_email WHERE id > :cursor_lo AND id <= :cursor_hi;
`,
	})

	loaded, err := plan.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	_, err = loaded.Migrations[0].Steps[0].Build()
	if !errors.Is(err, step.ErrNoBackfillPredicate) {
		t.Fatalf("build returned %v, want ErrNoBackfillPredicate", err)
	}
}

// TestLoadRejectsBadBackfillAnnotations covers every way the annotation can be
// wrong. Each would otherwise become a backfill that walks the wrong range.
func TestLoadRejectsBadBackfillAnnotations(t *testing.T) {
	cases := map[string]string{
		"no table":           "-- +mig backfill: key=id",
		"no key":             "-- +mig backfill: table=users",
		"not a pair":         "-- +mig backfill: table=users key=id batch",
		"unknown setting":    "-- +mig backfill: table=users key=id stride=10",
		"batch not a number": "-- +mig backfill: table=users key=id batch=lots",
		"batch not positive": "-- +mig backfill: table=users key=id batch=0",
		"lag not a number":   "-- +mig backfill: table=users key=id max_lag_bytes=huge",
	}

	for name, annotation := range cases {
		t.Run(name, func(t *testing.T) {
			const statement = "\nUPDATE users SET email = legacy_email" +
				" WHERE id > :cursor_lo AND id <= :cursor_hi;\n"

			dir := write(t, map[string]string{
				"20240817120000_fill.sql": annotation + statement,
			})

			if _, err := plan.Load(dir); !errors.Is(err, plan.ErrBadBackfill) {
				t.Fatalf("load returned %v, want ErrBadBackfill", err)
			}
		})
	}
}

// TestBackfillTakesOneStatement covers a cursor that would not describe
// everything the step did.
func TestBackfillTakesOneStatement(t *testing.T) {
	dir := write(t, map[string]string{
		"20240817120000_fill.sql": `
-- +mig step: fill
-- +mig backfill: table=users key=id
-- +mig satisfied: sql(SELECT true)
UPDATE users SET email = legacy_email WHERE id > :cursor_lo AND id <= :cursor_hi;
UPDATE users SET name = 'x' WHERE id > :cursor_lo AND id <= :cursor_hi;
`,
	})

	if _, err := plan.Load(dir); !errors.Is(err, plan.ErrBadBackfill) {
		t.Fatalf("load returned %v, want ErrBadBackfill", err)
	}
}

// TestLoadRejectsBadInput covers everything the loader refuses. Each of these
// would otherwise become a confusing failure part-way through a migration.
func TestLoadRejectsBadInput(t *testing.T) {
	cases := map[string]struct {
		files map[string]string
		want  error
	}{
		"no migrations": {
			files: map[string]string{"README.md": "not a migration"},
			want:  plan.ErrNoMigrations,
		},
		"duplicate version": {
			files: map[string]string{
				"20240817120000_one.sql": "SELECT 1;\n",
				"20240817120000_two.sql": "SELECT 1;\n",
			},
			want: plan.ErrDuplicateVersion,
		},
		"duplicate step name": {
			files: map[string]string{
				"20240817120000_dup.sql": `
-- +mig step: same
SELECT 1;
-- +mig step: same
SELECT 2;
`,
			},
			want: plan.ErrDuplicateStep,
		},
		"unknown annotation": {
			files: map[string]string{
				"20240817120000_odd.sql": "-- +mig teleport\nSELECT 1;\n",
			},
			want: plan.ErrUnknownAnnotation,
		},
		"empty step": {
			files: map[string]string{
				"20240817120000_empty.sql": "-- +mig step: nothing\n-- just a comment\n",
			},
			want: plan.ErrEmptyStep,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := plan.Load(write(t, tc.files)); !errors.Is(err, tc.want) {
				t.Fatalf("load returned %v, want %v", err, tc.want)
			}
		})
	}
}

// TestLoadRejectsMalformedNames covers file names that carry no usable version,
// which would make the apply order arbitrary. Any number of digits is a version,
// because an adopted history counts from 1; what is refused is a name with no
// leading number, or a number with no name after it.
func TestLoadRejectsMalformedNames(t *testing.T) {
	for _, name := range []string{"add_email.sql", "_add_email.sql", "20240817120000.sql"} {
		t.Run(name, func(t *testing.T) {
			dir := write(t, map[string]string{name: "SELECT 1;\n"})

			if _, err := plan.Load(dir); err == nil {
				t.Fatalf("load accepted %q", name)
			}
		})
	}
}

// TestLoadRejectsInvalidSatisfied covers a malformed escape hatch, which must
// not silently leave the step with no predicate at all.
func TestLoadRejectsInvalidSatisfied(t *testing.T) {
	for name, annotation := range map[string]string{
		"not wrapped": "-- +mig satisfied: SELECT true",
		"empty":       "-- +mig satisfied: sql()",
		"unclosed":    "-- +mig satisfied: sql(SELECT true",
	} {
		t.Run(name, func(t *testing.T) {
			dir := write(t, map[string]string{
				"20240817120000_bad.sql": annotation + "\nSELECT 1;\n",
			})

			if _, err := plan.Load(dir); err == nil {
				t.Fatalf("load accepted %q", annotation)
			}
		})
	}
}

// TestLoadRejectsMissingDirectory covers a mistyped --dir.
func TestLoadRejectsMissingDirectory(t *testing.T) {
	if _, err := plan.Load(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("load accepted a directory that does not exist")
	}
}

// TestLoadRejectsInvalidSQL keeps a typo from reaching the database.
func TestLoadRejectsInvalidSQL(t *testing.T) {
	dir := write(t, map[string]string{
		"20240817120000_typo.sql": "CREATE INDEX ON;\n",
	})

	if _, err := plan.Load(dir); err == nil {
		t.Fatal("load accepted invalid SQL")
	}
}

// write builds a migration directory and returns its path.
func write(t *testing.T, files map[string]string) string {
	t.Helper()

	dir := t.TempDir()

	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %q: %v", name, err)
		}
	}

	return dir
}

// An adopted history is the reason versions are numbers rather than text: goose
// and golang-migrate both count from 1, and a shorter version has to sort by
// what it is worth rather than by where its digits fall.

// TestLoadOrdersByVersionNotByName covers exactly that: 2 before 10.
func TestLoadOrdersByVersionNotByName(t *testing.T) {
	dir := write(t, map[string]string{
		"10_third.sql": "CREATE TABLE c (id bigint PRIMARY KEY);\n",
		"2_second.sql": "CREATE TABLE b (id bigint PRIMARY KEY);\n",
		"1_first.sql":  "CREATE TABLE a (id bigint PRIMARY KEY);\n",
	})

	loaded, err := plan.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	var ids []string
	for _, migration := range loaded.Migrations {
		ids = append(ids, migration.ID)
	}

	want := []string{"1_first", "2_second", "10_third"}
	if !slices.Equal(ids, want) {
		t.Fatalf("order is %v, want %v", ids, want)
	}
}

// TestLoadIgnoresRollbackFiles covers a golang-migrate directory, which holds an
// up and a down file for every version. Reading both would make each version
// look duplicated and the whole directory unloadable.
func TestLoadIgnoresRollbackFiles(t *testing.T) {
	dir := write(t, map[string]string{
		"1_create_users.up.sql":   "CREATE TABLE users (id bigint PRIMARY KEY);\n",
		"1_create_users.down.sql": "DROP TABLE users;\n",
	})

	loaded, err := plan.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if len(loaded.Migrations) != 1 {
		t.Fatalf("got %d migrations, want 1", len(loaded.Migrations))
	}

	migration := loaded.Migrations[0]

	if migration.ID != "1_create_users" {
		t.Fatalf("id is %q, want the .up suffix trimmed", migration.ID)
	}

	if migration.File != "1_create_users.up.sql" {
		t.Fatalf("file is %q, want the name on disk", migration.File)
	}
}

// TestLoadFSReadsAnEmbeddedDirectory covers the shape a service binary uses: no
// directory shipped alongside it, migrations compiled in.
func TestLoadFSReadsAnEmbeddedDirectory(t *testing.T) {
	loaded, err := plan.LoadFS(fstest.MapFS{
		"20240817120000_add_email.sql": &fstest.MapFile{
			Data: []byte("ALTER TABLE users ADD COLUMN email text;\n"),
		},
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if len(loaded.Migrations) != 1 || len(loaded.Migrations[0].Steps) != 1 {
		t.Fatalf("loaded %+v", loaded)
	}
}

// TestLoadFSRejectsAnEmptyFS covers a caller who embedded the wrong path, which
// is otherwise silent: nothing to apply looks exactly like nothing to do.
func TestLoadFSRejectsAnEmptyFS(t *testing.T) {
	if _, err := plan.LoadFS(fstest.MapFS{}); !errors.Is(err, plan.ErrNoMigrations) {
		t.Fatalf("load returned %v, want ErrNoMigrations", err)
	}
}

// TestLoadRejectsAVersionTooLargeToOrder covers a name whose digits do not fit
// the number that orders it.
func TestLoadRejectsAVersionTooLargeToOrder(t *testing.T) {
	dir := write(t, map[string]string{
		"99999999999999999999999_huge.sql": "CREATE TABLE a (id bigint PRIMARY KEY);\n",
	})

	if _, err := plan.Load(dir); err == nil {
		t.Fatal("load accepted a version that does not fit")
	}
}

// TestLoadRejectsUnreadableFile covers a directory entry that lists but cannot
// be read, which is what a permission change between the two calls looks like.
func TestLoadRejectsUnreadableFile(t *testing.T) {
	listable := unreadableFS{files: fstest.MapFS{
		"1_unreadable.sql": &fstest.MapFile{Data: []byte("SELECT 1;\n")},
	}}

	if _, err := plan.LoadFS(listable); err == nil {
		t.Fatal("load accepted a file it cannot read")
	}
}

// unreadableFS lists its entries and refuses to open them, so that the read
// fails after the listing has already succeeded.
type unreadableFS struct {
	files fstest.MapFS
}

// ReadDir lists, as the real directory did a moment earlier.
func (u unreadableFS) ReadDir(name string) ([]fs.DirEntry, error) {
	return u.files.ReadDir(name)
}

// Open always fails.
func (unreadableFS) Open(name string) (fs.File, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrPermission}
}

// TestLoadIgnoresNonMigrationEntries covers a directory holding a README and a
// subdirectory next to the migrations.
func TestLoadIgnoresNonMigrationEntries(t *testing.T) {
	dir := write(t, map[string]string{
		"1_first.sql": "CREATE TABLE a (id bigint PRIMARY KEY);\n",
		"README.md":   "not a migration\n",
	})

	if err := os.Mkdir(filepath.Join(dir, "archive.sql"), 0o750); err != nil {
		t.Fatalf("make the subdirectory: %v", err)
	}

	loaded, err := plan.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if len(loaded.Migrations) != 1 {
		t.Fatalf("got %d migrations, want 1", len(loaded.Migrations))
	}
}
