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

// Package autofix_test holds the linter's fixes to the executor's oracle: a
// fixed migration must run clean through the executor and leave a schema
// fingerprint identical to the unsafe statement it replaced. An autofix that
// produces a different schema is worse than no autofix.
package autofix_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/tochemey/mig/internal/catalog"
	"github.com/tochemey/mig/pkg/mig"
	"github.com/tochemey/mig/test/harness"
)

// fixCase is one unsafe migration and the world it runs against.
type fixCase struct {
	name   string
	seed   []string
	unsafe string
}

// cases covers every rule whose fix is executable. The type change is
// deliberately absent: its fix is a scaffold, and a scaffold does not run.
var cases = []fixCase{
	{
		name: "add_column_with_volatile_default",
		seed: []string{
			"CREATE TABLE accounts (id int, name text)",
			"INSERT INTO accounts SELECT g, 'u' || g FROM generate_series(1, 200) g",
		},
		unsafe: "-- +mig step: add_token\n" +
			"ALTER TABLE accounts ADD COLUMN token uuid NOT NULL DEFAULT gen_random_uuid();\n",
	},
	{
		name: "set_not_null",
		seed: []string{
			"CREATE TABLE accounts (id int, name text)",
			"INSERT INTO accounts SELECT g, 'u' || g FROM generate_series(1, 200) g",
		},
		unsafe: "-- +mig step: require_name\n" +
			"ALTER TABLE accounts ALTER COLUMN name SET NOT NULL;\n",
	},
	{
		name: "add_foreign_key",
		seed: []string{
			"CREATE TABLE accounts (id int PRIMARY KEY)",
			"CREATE TABLE invoices (id int, uid int)",
			"INSERT INTO accounts SELECT g FROM generate_series(1, 50) g",
			"INSERT INTO invoices SELECT g, 1 + g % 50 FROM generate_series(1, 200) g",
		},
		unsafe: "-- +mig step: add_fk\n" +
			"ALTER TABLE invoices ADD CONSTRAINT invoices_fk " +
			"FOREIGN KEY (uid) REFERENCES accounts (id);\n",
	},
	{
		name: "add_check",
		seed: []string{
			"CREATE TABLE accounts (id int, score int)",
			"INSERT INTO accounts SELECT g, g FROM generate_series(1, 200) g",
		},
		unsafe: "-- +mig step: add_check\n" +
			"ALTER TABLE accounts ADD CONSTRAINT accounts_score_positive CHECK (score > 0);\n",
	},
	{
		name: "concurrent_index_reconciled_by_hand",
		seed: []string{
			"CREATE TABLE accounts (id int, name text)",
			"INSERT INTO accounts SELECT g, 'u' || g FROM generate_series(1, 200) g",
		},
		unsafe: "-- +mig step: index_name\n" +
			"-- +mig notx\n" +
			"-- +mig satisfied: sql(SELECT to_regclass('idx_accounts_name') IS NOT NULL)\n" +
			"CREATE INDEX CONCURRENTLY idx_accounts_name ON accounts (name);\n",
	},
	{
		name: "add_primary_key",
		seed: []string{
			"CREATE TABLE accounts (id int, name text)",
			"INSERT INTO accounts SELECT g, 'u' || g FROM generate_series(1, 200) g",
		},
		unsafe: "-- +mig step: add_pk\n" +
			"ALTER TABLE accounts ADD CONSTRAINT accounts_pk PRIMARY KEY (id);\n",
	},
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

// fixOf runs the linter over the unsafe migration and returns the one
// executable fix it must produce.
func fixOf(t *testing.T, unsafe string) string {
	t.Helper()

	fsys := fstest.MapFS{"20240817120000_case.sql": &fstest.MapFile{Data: []byte(unsafe)}}

	linted, err := mig.Lint(fsys, mig.DefaultTargetVersion)
	if err != nil {
		t.Fatalf("lint the unsafe migration: %v", err)
	}

	if len(linted.Findings) != 1 {
		t.Fatalf("expected one finding, got %+v", linted.Findings)
	}

	finding := linted.Findings[0]
	if finding.Fix == "" || finding.FixScaffold {
		t.Fatalf("no executable fix on %s", finding.RuleID)
	}

	return finding.Fix
}

// converge seeds a fresh clone, applies the migration through the executor,
// verifies convergence, and returns the schema fingerprint beside its
// readable form, which is what a mismatch needs to become a diff.
func converge(t *testing.T, seed []string, migration string) (string, string) {
	t.Helper()

	ctx := t.Context()

	name, err := shared.Clone(ctx, harness.Template)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	db, err := shared.Open(ctx, name)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close %q: %v", name, err)
		}
	})

	for _, stmt := range seed {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	fsys := fstest.MapFS{"20240817120000_case.sql": &fstest.MapFile{Data: []byte(migration)}}

	if _, err := mig.Up(ctx, db, fsys, mig.Options{}); err != nil {
		t.Fatalf("apply:\n%s\n%v", migration, err)
	}

	if err := mig.Verify(ctx, db, fsys); err != nil {
		t.Fatalf("verify after apply: %v", err)
	}

	fingerprint, err := mig.Fingerprint(ctx, db)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}

	described, err := catalog.Describe(ctx, db)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}

	return fingerprint, described
}

// TestFixedMigrationsConvergeToTheSameSchema is V2's acceptance: for each
// rule with an executable fix, the generated steps run clean through the
// executor and land on the exact schema the unsafe statement would have.
func TestFixedMigrationsConvergeToTheSameSchema(t *testing.T) {
	if shared == nil {
		t.Skip("docker unavailable")
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixed := fixOf(t, tc.unsafe)

			// The fix must not merely converge: it must lint clean, or the
			// linter is recommending statements it would then flag.
			fsys := fstest.MapFS{"20240817120000_case.sql": &fstest.MapFile{Data: []byte(fixed)}}

			relinted, err := mig.Lint(fsys, mig.DefaultTargetVersion)
			if err != nil {
				t.Fatalf("lint the fixed migration: %v", err)
			}

			if len(relinted.Findings) != 0 {
				t.Fatalf("the fix lints dirty: %+v", relinted.Findings)
			}

			unsafePrint, unsafeSchema := converge(t, tc.seed, tc.unsafe)
			fixedPrint, fixedSchema := converge(t, tc.seed, fixed)

			if unsafePrint != fixedPrint {
				t.Errorf("schemas diverge\n--- unsafe\n%s\n--- fixed\n%s",
					unsafeSchema, fixedSchema)
			}

			if !strings.Contains(fixed, "-- +mig step:") {
				t.Error("the fix is not annotated mig format")
			}
		})
	}
}
