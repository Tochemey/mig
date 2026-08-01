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
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/tochemey/mig/internal/lint/rules"
	"github.com/tochemey/mig/pkg/mig"
)

// typeChange is one rewriting statement per table, so a run reports one
// finding for each and their severities can be compared side by side.
const typeChange = `-- +mig step: widen_big
ALTER TABLE big ALTER COLUMN n TYPE bigint;

-- +mig step: widen_small
ALTER TABLE small ALTER COLUMN n TYPE bigint;
`

// hazardous is a migration with one warning and one error.
const hazardous = `-- +mig step: index
CREATE INDEX idx_users_email ON users (email);

-- +mig step: compact
-- +mig notx
VACUUM FULL users;
`

func TestLintReportsHazards(t *testing.T) {
	fsys := fstest.MapFS{"1_m.sql": &fstest.MapFile{Data: []byte(hazardous)}}

	linted, err := mig.Lint(fsys, mig.DefaultTargetVersion, nil)
	if err != nil {
		t.Fatalf("lint: %v", err)
	}

	if len(linted.Findings) != 2 {
		t.Fatalf("found %d findings, want 2: %+v", len(linted.Findings), linted.Findings)
	}

	if linted.Findings[0].RuleID != rules.L001 || linted.Findings[1].RuleID != rules.L010 {
		t.Errorf("found %s and %s, want L001 and L010",
			linted.Findings[0].RuleID, linted.Findings[1].RuleID)
	}

	if linted.Errors() != 1 {
		t.Errorf("Errors() = %d, want 1", linted.Errors())
	}

	if linted.Sources["1_m.sql"] != hazardous {
		t.Error("the report does not carry the linted source")
	}
}

// TestLintConnectedGradesFindingsBySize is §6's credibility rule: the same
// statement is an outage on a large table and an observation on a lookup
// table, and a linter that grades them alike gets muted.
func TestLintConnectedGradesFindingsBySize(t *testing.T) {
	db := newDatabase(t)

	apply(t, db,
		"CREATE TABLE big (n int)",
		"CREATE TABLE small (n int)",
		"INSERT INTO big SELECT g FROM generate_series(1, 50000) g",
		"INSERT INTO small SELECT g FROM generate_series(1, 20) g",
		"ANALYZE big, small",

		// Both tables hold real pages, so both have a size to estimate
		// against. The row count that decides the grade is then told to the
		// catalog rather than inserted: forty million rows in a unit test
		// would be measuring Postgres, and the acceptance run does that
		// against a table that really holds ten million.
		"UPDATE pg_class SET reltuples = 40e6 WHERE oid = 'big'::regclass")

	fsys := fstest.MapFS{"1_m.sql": &fstest.MapFile{Data: []byte(typeChange)}}

	linted, err := mig.LintConnected(t.Context(), db, fsys, nil)
	if err != nil {
		t.Fatalf("lint connected: %v", err)
	}

	if len(linted.Findings) != 2 {
		t.Fatalf("found %d findings, want one per table: %+v", len(linted.Findings), linted.Findings)
	}

	big, small := linted.Findings[0], linted.Findings[1]

	if big.Severity != mig.SeverityError {
		t.Errorf("the large table's rewrite is %s, want error: %s", big.Severity, big.Detail)
	}

	if small.Severity != mig.SeverityInfo {
		t.Errorf("the lookup table's rewrite is %s, want info: %s", small.Severity, small.Detail)
	}

	// The detail line has to say what the grade was based on, or the reader
	// has no way to disagree with it.
	if !strings.Contains(big.Detail, "40.0M rows") {
		t.Errorf("detail = %q, want the row estimate it was graded on", big.Detail)
	}

	// The probe ran, so this server has a measured throughput. Neither table
	// here is large enough to be worth an estimate; what an estimate reads
	// like is pinned in the rules package, and that it is within twice the
	// truth is the acceptance run's job.
	if linted.Uncalibrated != "" {
		t.Fatalf("the server was not measured: %s", linted.Uncalibrated)
	}
}

// TestLintConnectedTakesTheVersionFromTheServer covers §4: connected, the
// target version is not a flag to get wrong.
func TestLintConnectedTakesTheVersionFromTheServer(t *testing.T) {
	db := newDatabase(t)

	apply(t, db, "CREATE TABLE t (id int)")

	const addDefault = "-- +mig step: add\nALTER TABLE t ADD COLUMN c int DEFAULT 42;\n"

	fsys := fstest.MapFS{"1_m.sql": &fstest.MapFile{Data: []byte(addDefault)}}

	linted, err := mig.LintConnected(t.Context(), db, fsys, nil)
	if err != nil {
		t.Fatalf("lint connected: %v", err)
	}

	// Every server the harness runs stores the default in the catalog, so the
	// statement is safe and nothing is reported.
	if len(linted.Findings) != 0 {
		t.Errorf("a constant default was flagged against a modern server: %+v", linted.Findings)
	}

	// The same statement written for Postgres 10 rewrites the table, which is
	// what the flag says and the server overrode.
	offline, err := mig.Lint(fsys, 10, nil)
	if err != nil {
		t.Fatalf("lint: %v", err)
	}

	if len(offline.Findings) != 1 || offline.Findings[0].RuleID != rules.L003 {
		t.Errorf("targeting 10 reported %+v, want the rewrite", offline.Findings)
	}
}

// TestLintConnectedFailsOnAClosedDatabase covers the connection error path.
func TestLintConnectedFailsOnAClosedDatabase(t *testing.T) {
	db := newDatabase(t)

	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	fsys := fstest.MapFS{"1_m.sql": &fstest.MapFile{Data: []byte(hazardous)}}

	if _, err := mig.LintConnected(t.Context(), db, fsys, nil); err == nil {
		t.Error("linting reported success against a closed pool")
	}
}

// TestLintConnectedRejectsAnEmptyDirectory covers the load failure reaching
// the caller before anything connects.
func TestLintConnectedRejectsAnEmptyDirectory(t *testing.T) {
	db := newDatabase(t)

	if _, err := mig.LintConnected(t.Context(), db, fstest.MapFS{}, nil); err == nil {
		t.Error("an empty directory linted clean")
	}
}

func TestLintRejectsAnEmptyDirectory(t *testing.T) {
	if _, err := mig.Lint(fstest.MapFS{}, mig.DefaultTargetVersion, nil); err == nil {
		t.Error("an empty directory linted clean")
	}
}

// vanishingFS serves a file a limited number of times, standing in for a
// directory that changes under the linter.
type vanishingFS struct {
	inner fs.FS
	name  string
	opens int
	limit int
}

func (v *vanishingFS) Open(name string) (fs.File, error) {
	if name == v.name {
		v.opens++

		if v.opens > v.limit {
			return nil, errors.New("vanished")
		}
	}

	return v.inner.Open(name)
}

func TestLintReportsAVanishingFile(t *testing.T) {
	inner := fstest.MapFS{"1_m.sql": &fstest.MapFile{Data: []byte(hazardous)}}

	// The loader reads the file once; the linter's second read fails.
	fsys := &vanishingFS{inner: inner, name: "1_m.sql", limit: 1}

	if _, err := mig.Lint(fsys, mig.DefaultTargetVersion, nil); err == nil {
		t.Error("a vanishing migration linted clean")
	}
}

// TestLintWithAPolicyAndASuppression covers the public surface V6 adds: a
// policy file read from disk, and the directives the report carries back for
// an audit.
func TestLintWithAPolicyAndASuppression(t *testing.T) {
	body := "-- +mig step: index\n" +
		"-- +mig lint:ignore L001 reason=\"the table has twelve rows\"\n" +
		"CREATE INDEX idx_users_email ON users (email);\n\n" +
		"-- +mig step: compact\n-- +mig notx\nVACUUM FULL users;\n"

	fsys := fstest.MapFS{"20240817120000_m.sql": &fstest.MapFile{Data: []byte(body)}}

	dir := t.TempDir()
	path := filepath.Join(dir, mig.PolicyFileName)

	if err := os.WriteFile(path, []byte("rules:\n  L010: warn\n"), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}

	pol, err := mig.LoadPolicy(path, dir)
	if err != nil {
		t.Fatalf("load policy: %v", err)
	}

	linted, err := mig.Lint(fsys, mig.DefaultTargetVersion, pol)
	if err != nil {
		t.Fatalf("lint: %v", err)
	}

	if len(linted.Findings) != 1 || linted.Findings[0].RuleID != rules.L010 {
		t.Fatalf("found %+v, want the vacuum alone", linted.Findings)
	}

	// The policy downgraded it, so a run that would have failed now passes.
	if linted.Errors() != 0 {
		t.Errorf("Errors() = %d, want the policy's warning", linted.Errors())
	}

	if len(linted.Suppressions) != 1 || !linted.Suppressions[0].Used {
		t.Errorf("suppressions = %+v, want the one that silenced the index build",
			linted.Suppressions)
	}
}
