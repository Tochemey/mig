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
	"strings"
	"testing"

	"github.com/tochemey/mig/internal/cli"
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

	for _, expect := range []string{"L001", "CREATE INDEX idx_users_email", "^", "1 finding(s)"} {
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

	if !strings.Contains(stdout, "L010") {
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

	if len(decoded.Findings) != 1 || decoded.Findings[0].Rule != "L001" {
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

	if !strings.Contains(stdout, "L003") {
		t.Errorf("the same default did not flag before Postgres 11:\n%s", stdout)
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
