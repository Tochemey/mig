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
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tochemey/mig/internal/cli"
	"github.com/tochemey/mig/internal/lint/rules"
)

// The lint command connects to nothing, so it needs no container.

func TestLintStaysQuietOnASafeMigration(t *testing.T) {
	dir := t.TempDir()

	write(t, dir, "20240817120000_safe.sql", `-- +mig step: index_email
-- +mig notx
CREATE INDEX CONCURRENTLY idx_users_email ON users (email);
`)

	stdout, _, err := run(t, "lint", "--dir", dir)
	if err != nil {
		t.Fatalf("lint: %v", err)
	}

	if stdout != "" {
		t.Errorf("a safe migration produced output:\n%s", stdout)
	}
}

func TestLintWarnsWithoutFailing(t *testing.T) {
	dir := t.TempDir()

	write(t, dir, "20240817120000_index.sql", `-- +mig step: index_email
CREATE INDEX idx_users_email ON users (email);
`)

	stdout, _, err := run(t, "lint", "--dir", dir)
	if err != nil {
		t.Fatalf("a warning failed the run: %v", err)
	}

	for _, expect := range []string{rules.L001, "CREATE INDEX idx_users_email", "^", "1 finding(s)"} {
		if !strings.Contains(stdout, expect) {
			t.Errorf("output lacks %q:\n%s", expect, stdout)
		}
	}
}

func TestLintFailsOnAnError(t *testing.T) {
	dir := t.TempDir()

	write(t, dir, "20240817120000_compact.sql", `-- +mig step: compact
-- +mig notx
VACUUM FULL users;
`)

	stdout, _, err := run(t, "lint", "--dir", dir)
	if err == nil || !strings.Contains(err.Error(), "1 error(s)") {
		t.Fatalf("err = %v, want the error count", err)
	}

	if !strings.Contains(stdout, rules.L010) {
		t.Errorf("output lacks the finding:\n%s", stdout)
	}
}

func TestLintWritesJSON(t *testing.T) {
	dir := t.TempDir()

	write(t, dir, "20240817120000_index.sql", `-- +mig step: index_email
CREATE INDEX idx_users_email ON users (email);
`)

	stdout, _, err := run(t, "lint", "--dir", dir, "--format", "json")
	if err != nil {
		t.Fatalf("lint: %v", err)
	}

	var decoded struct {
		Findings []struct {
			Rule string `json:"rule"`
		} `json:"findings"`
	}

	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}

	if len(decoded.Findings) != 1 || decoded.Findings[0].Rule != rules.L001 {
		t.Errorf("decoded %+v", decoded.Findings)
	}
}

func TestLintHonoursTheTargetVersion(t *testing.T) {
	dir := t.TempDir()

	write(t, dir, "20240817120000_default.sql", `-- +mig step: add_score
ALTER TABLE users ADD COLUMN score int DEFAULT 0;
`)

	stdout, _, err := run(t, "lint", "--dir", dir)
	if err != nil || stdout != "" {
		t.Fatalf("a stored default flagged on a modern target: %v\n%s", err, stdout)
	}

	stdout, _, err = run(t, "lint", "--dir", dir, "--target-version", "10")
	if err != nil {
		t.Fatalf("lint at 10: %v", err)
	}

	if !strings.Contains(stdout, rules.L003) {
		t.Errorf("the same default did not flag before Postgres 11:\n%s", stdout)
	}
}

// TestLintConnectedGradesAndEstimates covers the --dsn path end to end: the
// severity comes from the size the catalog reports, and the report carries
// both that size and how long the work will take.
func TestLintConnectedGradesAndEstimates(t *testing.T) {
	database := newDatabase(t)
	dir := t.TempDir()

	write(t, dir, "20240817120000_widen.sql", `-- +mig step: widen_id
ALTER TABLE users ALTER COLUMN id TYPE numeric;
`)

	stdout, _, err := run(t, "lint", "--dir", dir, "--dsn", shared.DSN(database))
	if err != nil {
		t.Fatalf("lint: %v", err)
	}

	// The seeded template is small, so the rewrite is worth knowing about and
	// not worth failing over, and the size it was graded on is on the line.
	for _, want := range []string{rules.L004, "info", "kB"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("connected output lacks %q:\n%s", want, stdout)
		}
	}
}

// TestLintConnectedReportsAnUnreachableDatabase covers the connection failing
// before anything is linted.
func TestLintConnectedReportsAnUnreachableDatabase(t *testing.T) {
	dir := t.TempDir()

	write(t, dir, "20240817120000_x.sql", "SELECT 1;\n")

	_, _, err := run(t, "lint", "--dir", dir, "--dsn", "postgres://nobody@127.0.0.1:1/none")
	if err == nil {
		t.Fatal("lint reported success against an unreachable database")
	}
}

func TestLintRejectsAnUnknownFormat(t *testing.T) {
	dir := t.TempDir()

	write(t, dir, "20240817120000_x.sql", "SELECT 1;\n")

	_, _, err := run(t, "lint", "--dir", dir, "--format", "yaml")
	if err == nil || !strings.Contains(err.Error(), "unknown format") {
		t.Fatalf("err = %v, want an unknown format complaint", err)
	}
}

func TestLintReportsAMissingDirectory(t *testing.T) {
	if _, _, err := run(t, "lint", "--dir", "/does/not/exist"); err == nil {
		t.Fatal("a missing directory linted clean")
	}
}

// failingOut refuses every write, standing in for a closed terminal.
type failingOut struct{}

func (failingOut) Write([]byte) (int, error) {
	return 0, errors.New("terminal gone")
}

func TestLintReportsARefusingTerminal(t *testing.T) {
	dir := t.TempDir()

	write(t, dir, "20240817120000_index.sql", `-- +mig step: index_email
CREATE INDEX idx_users_email ON users (email);
`)

	root := cli.New()
	root.SetOut(failingOut{})
	root.SetErr(io.Discard)
	root.SetArgs([]string{"lint", "--dir", dir})

	if err := root.ExecuteContext(t.Context()); err == nil {
		t.Fatal("a failed write went unreported")
	}
}

func TestLintWritesSARIF(t *testing.T) {
	dir := t.TempDir()

	write(t, dir, "20240817120000_index.sql", `-- +mig step: index_email
CREATE INDEX idx_users_email ON users (email);
`)

	stdout, _, err := run(t, "lint", "--dir", dir, "--format", "sarif")
	if err != nil {
		t.Fatalf("lint: %v", err)
	}

	var decoded struct {
		Version string `json:"version"`

		Runs []struct {
			Results []struct {
				RuleID string `json:"ruleId"`
				Level  string `json:"level"`

				Locations []struct {
					PhysicalLocation struct {
						ArtifactLocation struct {
							URI string `json:"uri"`
						} `json:"artifactLocation"`
					} `json:"physicalLocation"`
				} `json:"locations"`
			} `json:"results"`
		} `json:"runs"`
	}

	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}

	if decoded.Version != "2.1.0" || len(decoded.Runs) != 1 {
		t.Fatalf("decoded %+v", decoded)
	}

	results := decoded.Runs[0].Results
	if len(results) != 1 || results[0].RuleID != rules.L001 || results[0].Level != "warning" {
		t.Fatalf("results = %+v", results)
	}

	// The finding is named under the directory that was linted, which is how
	// it lands on a line of the pull request.
	if uri := results[0].Locations[0].PhysicalLocation.ArtifactLocation.URI; !strings.HasSuffix(uri,
		strings.TrimPrefix(dir, "/")+"/20240817120000_index.sql") {
		t.Errorf("uri = %q, want it under the linted directory", uri)
	}
}

func TestLintWritesAPullRequestComment(t *testing.T) {
	dir := t.TempDir()

	write(t, dir, "20240817120000_index.sql", `-- +mig step: index_email
CREATE INDEX idx_users_email ON users (email);
`)

	stdout, _, err := run(t, "lint", "--dir", dir, "--format", "github")
	if err != nil {
		t.Fatalf("lint: %v", err)
	}

	for _, want := range []string{"### mig lint", "| file | rule |", rules.L001, "SHARE"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("comment lacks %q:\n%s", want, stdout)
		}
	}
}

// TestLintHonoursAPolicyFile covers the three things a policy decides, from
// the command line down: the severity a rule carries, the target version and
// the sizes a hazard is graded by.
func TestLintHonoursAPolicyFile(t *testing.T) {
	dir := t.TempDir()

	write(t, dir, "20240817120000_default.sql", `-- +mig step: add_score
ALTER TABLE users ADD COLUMN score int DEFAULT 0;
`)

	policy := filepath.Join(dir, "policy.yaml")
	write(t, dir, "policy.yaml", "target_version: 10\nrules:\n  L003: info\n")

	// The stored default rewrites before Postgres 11, which is the version
	// the policy names, and the policy grades that as an observation.
	stdout, _, err := run(t, "lint", "--dir", dir, "--policy", policy)
	if err != nil {
		t.Fatalf("lint: %v", err)
	}

	if !strings.Contains(stdout, "info "+rules.L003) {
		t.Errorf("output lacks the policy's grading:\n%s", stdout)
	}

	// A version named on the command line outranks the policy's.
	stdout, _, err = run(t, "lint", "--dir", dir, "--policy", policy, "--target-version", "18")
	if err != nil || stdout != "" {
		t.Fatalf("the flag did not outrank the policy: %v\n%s", err, stdout)
	}
}

func TestLintReportsAPolicyItCannotRead(t *testing.T) {
	dir := t.TempDir()

	write(t, dir, "20240817120000_x.sql", "SELECT 1;\n")

	_, _, err := run(t, "lint", "--dir", dir, "--policy", filepath.Join(dir, "absent.yaml"))
	if err == nil || !strings.Contains(err.Error(), "read policy") {
		t.Fatalf("err = %v, want the missing policy", err)
	}

	write(t, dir, "policy.yaml", "rules:\n  L999: error\n")

	_, _, err = run(t, "lint", "--dir", dir, "--policy", filepath.Join(dir, "policy.yaml"))
	if err == nil || !strings.Contains(err.Error(), "no rule this build has") {
		t.Fatalf("err = %v, want the unknown rule", err)
	}
}

// TestLintAuditsSuppressions covers --report-suppressions: every directive,
// with what it silences and whether it still silences anything.
func TestLintAuditsSuppressions(t *testing.T) {
	dir := t.TempDir()

	write(t, dir, "20240817120000_index.sql", `-- +mig step: index_email
-- +mig lint:ignore L001 reason="the table has twelve rows"
-- +mig lint:ignore L010 reason="nothing here vacuums"
CREATE INDEX idx_users_email ON users (email);
`)

	stdout, _, err := run(t, "lint", "--dir", dir, "--report-suppressions")
	if err != nil {
		t.Fatalf("lint: %v", err)
	}

	// The suppressed finding is gone from the report and accounted for in the
	// audit, and the directive that silenced nothing is named as such.
	if strings.Contains(stdout, "warn "+rules.L001) {
		t.Errorf("a suppressed finding was reported:\n%s", stdout)
	}

	for _, want := range []string{"FILE", "used", "unused", "the table has twelve rows"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("audit lacks %q:\n%s", want, stdout)
		}
	}
}

func TestLintFailsOnASuppressionWithoutAReason(t *testing.T) {
	dir := t.TempDir()

	write(t, dir, "20240817120000_index.sql", `-- +mig step: index_email
-- +mig lint:ignore L001
CREATE INDEX idx_users_email ON users (email);
`)

	stdout, _, err := run(t, "lint", "--dir", dir)
	if err == nil || !strings.Contains(err.Error(), "1 error(s)") {
		t.Fatalf("err = %v, want the directive to fail the run", err)
	}

	if !strings.Contains(stdout, rules.L000) || !strings.Contains(stdout, "gives no reason") {
		t.Errorf("output lacks the complaint:\n%s", stdout)
	}
}

func TestLintRefusesToAuditInAnotherFormat(t *testing.T) {
	dir := t.TempDir()

	write(t, dir, "20240817120000_x.sql", "SELECT 1;\n")

	_, _, err := run(t, "lint", "--dir", dir, "--format", "json", "--report-suppressions")
	if err == nil || !strings.Contains(err.Error(), "--report-suppressions") {
		t.Fatalf("err = %v, want the format complaint", err)
	}
}

// TestLintReportsARefusingTerminalDuringTheAudit covers the audit's own sink.
// The migration is clean, so the findings report writes nothing and the first
// write of the run is the audit's.
func TestLintReportsARefusingTerminalDuringTheAudit(t *testing.T) {
	dir := t.TempDir()

	write(t, dir, "20240817120000_safe.sql", `-- +mig step: index_email
-- +mig notx
-- +mig lint:ignore L001 reason="already concurrent"
CREATE INDEX CONCURRENTLY idx_users_email ON users (email);
`)

	root := cli.New()
	root.SetOut(failingOut{})
	root.SetErr(io.Discard)
	root.SetArgs([]string{"lint", "--dir", dir, "--report-suppressions"})

	if err := root.ExecuteContext(t.Context()); err == nil {
		t.Fatal("a failed write went unreported")
	}
}
