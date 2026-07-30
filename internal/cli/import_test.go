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

package cli_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tochemey/mig/internal/cli"
	"github.com/tochemey/mig/internal/importer"
)

// The import command. Adoption is the distribution strategy: a repository that
// has to rewrite its migrations to try this tool never tries it.

// TestImportAdoptsAGooseHistory covers the whole path an adopting repository
// takes: import, then run, and the adopted migration is not applied again.
func TestImportAdoptsAGooseHistory(t *testing.T) {
	database := newDatabase(t)
	dsn := shared.DSN(database)
	dir := migrationDir(t)

	db := openDatabase(t, database)

	// The state goose leaves behind: the column already added, and its history
	// saying so.
	mustExec(t, db, "ALTER TABLE users ADD COLUMN email text")
	mustExec(t, db, `CREATE TABLE goose_db_version (
		id serial PRIMARY KEY,
		version_id bigint NOT NULL,
		is_applied boolean NOT NULL,
		tstamp timestamp DEFAULT now())`)
	mustExec(t, db,
		"INSERT INTO goose_db_version (version_id, is_applied) VALUES (20240817120000, true)")

	stdout, _, err := run(t, "import", "--dsn", dsn, "--dir", dir, "--from", "goose")
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	if !strings.Contains(stdout, "adopted:  20240817120000_add_email") {
		t.Fatalf("stdout is %q", stdout)
	}

	// The point of adopting: the next run has nothing to do, and does not try
	// to add a column that is already there.
	stdout, _, err = run(t, "up", "--dsn", dsn, "--dir", dir)
	if err != nil {
		t.Fatalf("up after import: %v", err)
	}

	if applied := decode(t, stdout).Applied; applied != 0 {
		t.Fatalf("the run applied %d steps after adopting them", applied)
	}
}

// TestImportLeavesUnknownHistoryToReconcile covers a golang-migrate history
// that does not cover the migration. It is not adopted, and the run applies it.
func TestImportLeavesUnknownHistoryToReconcile(t *testing.T) {
	database := newDatabase(t)
	dsn := shared.DSN(database)
	dir := migrationDir(t)

	db := openDatabase(t, database)

	mustExec(t, db, "CREATE TABLE schema_migrations (version bigint PRIMARY KEY, dirty boolean NOT NULL)")
	mustExec(t, db, "INSERT INTO schema_migrations VALUES (1, false)")

	stdout, _, err := run(t, "import", "--dsn", dsn, "--dir", dir, "--from", "golang-migrate")
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	if !strings.Contains(stdout, "recheck:  20240817120000_add_email") {
		t.Fatalf("stdout is %q", stdout)
	}

	stdout, _, err = run(t, "up", "--dsn", dsn, "--dir", dir)
	if err != nil {
		t.Fatalf("up after import: %v", err)
	}

	if applied := decode(t, stdout).Applied; applied != 1 {
		t.Fatalf("the run applied %d steps, want the unadopted one", applied)
	}
}

// TestImportReportsADirtyHistory covers golang-migrate's flag, which stops
// every deployment there until a human clears it by hand. Here it is reported
// and the version is left for the catalog to settle.
func TestImportReportsADirtyHistory(t *testing.T) {
	database := newDatabase(t)
	dsn := shared.DSN(database)

	db := openDatabase(t, database)

	mustExec(t, db, "CREATE TABLE schema_migrations (version bigint PRIMARY KEY, dirty boolean NOT NULL)")
	mustExec(t, db, "INSERT INTO schema_migrations VALUES (20240817120000, true)")

	stdout, _, err := run(t, "import", "--dsn", dsn, "--dir", migrationDir(t), "--from", "golang-migrate")
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	if !strings.Contains(stdout, "dirty:") {
		t.Fatalf("stdout %q does not report the dirty flag", stdout)
	}

	if !strings.Contains(stdout, "recheck:") {
		t.Fatalf("stdout %q adopted the dirty version", stdout)
	}
}

// TestImportRejectsBadInvocations covers everything that must fail before a
// single ledger row is written.
func TestImportRejectsBadInvocations(t *testing.T) {
	requireHarness(t)

	dir := migrationDir(t)

	cases := map[string][]string{
		"no dsn":            {"import", "--dir", dir, "--from", "goose"},
		"no source":         {"import", "--dsn", "postgres://x/y", "--dir", dir},
		"unknown source":    {"import", "--dsn", "postgres://x/y", "--dir", dir, "--from", "flyway"},
		"missing directory": {"import", "--dsn", "postgres://x/y", "--dir", filepath.Join(dir, "absent"), "--from", "goose"},
		"unreachable":       {"import", "--dsn", shared.DSN("no_such_database"), "--dir", dir, "--from", "goose"},
		"unexpected argument": {
			"import", "--dsn", "postgres://x/y", "--dir", dir, "--from", "goose", "extra",
		},
	}

	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := run(t, args...); err == nil {
				t.Fatalf("%v was accepted", args)
			}
		})
	}
}

// TestImportRejectsAnAbsentHistory covers pointing it at a database the other
// tool never ran against, which is a mistake worth naming rather than an empty
// adoption.
func TestImportRejectsAnAbsentHistory(t *testing.T) {
	database := newDatabase(t)

	_, _, err := run(t, "import", "--dsn", shared.DSN(database),
		"--dir", migrationDir(t), "--from", "goose")

	if !errors.Is(err, importer.ErrNoHistory) {
		t.Fatalf("import returned %v, want ErrNoHistory", err)
	}
}

// TestImportReportsBeingLocked covers meeting a run already in progress. Two
// writers to one ledger is exactly what the lease exists to prevent.
func TestImportReportsBeingLocked(t *testing.T) {
	database := newDatabase(t)

	holder := holdLease(t, database)

	t.Cleanup(func() {
		if err := holder.Release(context.Background()); err != nil {
			t.Errorf("release lease: %v", err)
		}
	})

	_, _, err := run(t, "import", "--dsn", shared.DSN(database),
		"--dir", migrationDir(t), "--from", "goose")

	if code := cli.ExitCode(err); code != cli.ExitLocked {
		t.Fatalf("import returned %v with exit code %d, want %d", err, code, cli.ExitLocked)
	}
}

// TestImportOutputFailureIsReported covers the same for the one command that
// has to take the lease before it can report anything.
func TestImportOutputFailureIsReported(t *testing.T) {
	database := newDatabase(t)
	db := openDatabase(t, database)

	mustExec(t, db, "CREATE TABLE schema_migrations (version bigint PRIMARY KEY, dirty boolean NOT NULL)")
	mustExec(t, db, "INSERT INTO schema_migrations VALUES (1, false)")

	err := runTo(t, brokenWriter{}, "import", "--dsn", shared.DSN(database),
		"--dir", migrationDir(t), "--from", "golang-migrate")

	if !errors.Is(err, errWrite) {
		t.Fatalf("import returned %v, want the write failure", err)
	}
}
