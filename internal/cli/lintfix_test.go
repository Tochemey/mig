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
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tochemey/mig/internal/cli"
	"github.com/tochemey/mig/internal/lint/rules"
)

// TestLintFixPagesByTheRealKeyWhenConnected covers the catalog reaching the
// one output that ends up in the author's file. Offline the generated
// backfill can only assume a key; given a database it reads the primary key,
// and a table keyed by anything but id is where the difference shows.
func TestLintFixPagesByTheRealKeyWhenConnected(t *testing.T) {
	database := newDatabase(t)
	dir := t.TempDir()

	db, err := requireHarness(t).Open(t.Context(), database)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close pool: %v", err)
		}
	}()

	if _, err := db.ExecContext(t.Context(),
		"CREATE TABLE accounts (account_id bigint PRIMARY KEY, name text)"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	write(t, dir, "20240817120000_token.sql", `-- +mig step: add_token
ALTER TABLE accounts ADD COLUMN token uuid NOT NULL DEFAULT gen_random_uuid();
`)

	if _, _, err := run(t, "lint", "--fix", "--yes",
		"--dir", dir, "--dsn", shared.DSN(database)); err != nil {
		t.Fatalf("lint --fix: %v", err)
	}

	fixed := readBack(t, dir, "20240817120000_token.sql")

	if !strings.Contains(fixed, "key=account_id") {
		t.Errorf("the fix did not page by the table's own key:\n%s", fixed)
	}
}

// unsafeMigration carries one fixable hazard and one that has no fix.
const unsafeMigration = `-- +mig step: fk
ALTER TABLE orders ADD CONSTRAINT orders_fk FOREIGN KEY (uid) REFERENCES users (id);

-- +mig step: index
CREATE INDEX idx_orders_uid ON orders (uid);
`

// runFix executes the command tree with the given stdin.
func runFix(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()

	var out bytes.Buffer

	root := cli.New()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)

	err := root.ExecuteContext(t.Context())

	return out.String(), err
}

// readBack returns the migration file's contents after a run.
func readBack(t *testing.T, dir, name string) string {
	t.Helper()

	content, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	return string(content)
}

func TestLintFixRewritesAfterConsent(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "20240817120000_fk.sql", unsafeMigration)

	stdout, err := runFix(t, "y\n", "lint", "--dir", dir, "--fix")
	if err != nil {
		t.Fatalf("fix: %v\n%s", err, stdout)
	}

	for _, expect := range []string{
		"- ALTER TABLE orders ADD CONSTRAINT orders_fk",
		"+ ALTER TABLE orders ADD CONSTRAINT orders_fk FOREIGN KEY (uid) REFERENCES users (id) NOT VALID;",
		"applied 1 fix(es) to 1 file(s)",
	} {
		if !strings.Contains(stdout, expect) {
			t.Errorf("output lacks %q:\n%s", expect, stdout)
		}
	}

	after := readBack(t, dir, "20240817120000_fk.sql")

	if !strings.Contains(after, "VALIDATE CONSTRAINT orders_fk") {
		t.Errorf("the validation step is missing:\n%s", after)
	}

	// The statement with no fix stays exactly where it was.
	if !strings.Contains(after, "CREATE INDEX idx_orders_uid ON orders (uid);") {
		t.Errorf("an unfixable statement was disturbed:\n%s", after)
	}

	// The rewritten file lints clean of the fixed hazard.
	verdict, _, err := run(t, "lint", "--dir", dir)
	if err != nil {
		t.Fatalf("re-lint: %v", err)
	}

	if strings.Contains(verdict, rules.L006) {
		t.Errorf("the fixed hazard is still reported:\n%s", verdict)
	}
}

func TestLintFixKeepsItsHandsOffWithoutConsent(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "20240817120000_fk.sql", unsafeMigration)

	stdout, err := runFix(t, "n\n", "lint", "--dir", dir, "--fix")
	if err != nil {
		t.Fatalf("fix: %v", err)
	}

	if !strings.Contains(stdout, "nothing written") {
		t.Errorf("no refusal reported:\n%s", stdout)
	}

	if readBack(t, dir, "20240817120000_fk.sql") != unsafeMigration {
		t.Error("the file changed without consent")
	}
}

func TestLintFixTreatsSilenceAsRefusal(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "20240817120000_fk.sql", unsafeMigration)

	stdout, err := runFix(t, "", "lint", "--dir", dir, "--fix")
	if err != nil {
		t.Fatalf("fix: %v", err)
	}

	if !strings.Contains(stdout, "nothing written") {
		t.Errorf("a closed stdin was read as consent:\n%s", stdout)
	}
}

func TestLintFixScaffoldsKeepTheStatement(t *testing.T) {
	dir := t.TempDir()

	write(t, dir, "20240817120000_type.sql", `-- +mig step: widen
ALTER TABLE users ALTER COLUMN id TYPE bigint;
`)

	stdout, err := runFix(t, "", "lint", "--dir", dir, "--fix", "--yes")
	if err != nil {
		t.Fatalf("fix: %v\n%s", err, stdout)
	}

	after := readBack(t, dir, "20240817120000_type.sql")

	if !strings.Contains(after, "ALTER TABLE users ALTER COLUMN id TYPE bigint;") {
		t.Errorf("the scaffold deleted the statement it comments on:\n%s", after)
	}

	if !strings.Contains(after, "-- TODO: writes landing between the backfill and the swap") {
		t.Errorf("the scaffold plan is missing:\n%s", after)
	}

	index := strings.Index(after, "-- TODO:")
	statement := strings.Index(after, "ALTER TABLE users ALTER COLUMN id TYPE bigint;")

	if index > statement {
		t.Error("the plan should sit above the statement it explains")
	}
}

func TestLintFixHandlesUnterminatedStatements(t *testing.T) {
	dir := t.TempDir()

	// The first file is an implicit single step starting at byte zero, with
	// no terminating semicolon. The second ends its flagged statement
	// without a semicolon before the next step begins.
	write(t, dir, "20240817120000_a.sql",
		"ALTER TABLE t ALTER COLUMN c SET NOT NULL")

	write(t, dir, "20240817120001_b.sql", `-- +mig step: one
ALTER TABLE u ALTER COLUMN d SET NOT NULL

-- +mig step: two
-- +mig notx
VACUUM users;

-- +mig step: three
ALTER TABLE u ALTER COLUMN e SET NOT NULL;
`)

	stdout, err := runFix(t, "", "lint", "--dir", dir, "--fix", "--yes")
	if err != nil {
		t.Fatalf("fix: %v\n%s", err, stdout)
	}

	first := readBack(t, dir, "20240817120000_a.sql")
	if !strings.Contains(first, "VALIDATE CONSTRAINT t_c_nn") {
		t.Errorf("the first file was not rewritten:\n%s", first)
	}

	second := readBack(t, dir, "20240817120001_b.sql")

	for _, expect := range []string{
		"VALIDATE CONSTRAINT u_d_nn",
		"VALIDATE CONSTRAINT u_e_nn",
		"VACUUM users;",
	} {
		if !strings.Contains(second, expect) {
			t.Errorf("the second file lacks %q:\n%s", expect, second)
		}
	}
}

func TestLintFixPreservesComments(t *testing.T) {
	dir := t.TempDir()

	// A banner above an implicit step, and a note above an annotated step:
	// neither belongs to the statement being replaced.
	write(t, dir, "20240817120000_banner.sql", `-- Copyright and a description.
-- Neither line is part of any step.
ALTER TABLE t ALTER COLUMN c SET NOT NULL;
`)

	write(t, dir, "20240817120001_note.sql", `-- +mig step: seed
SELECT 1;
-- A note about what follows.
-- +mig step: nn
-- an annotation comment inside the step
ALTER TABLE u ALTER COLUMN d SET NOT NULL;
`)

	stdout, err := runFix(t, "", "lint", "--dir", dir, "--fix", "--yes")
	if err != nil {
		t.Fatalf("fix: %v\n%s", err, stdout)
	}

	banner := readBack(t, dir, "20240817120000_banner.sql")

	for _, expect := range []string{
		"-- Copyright and a description.\n-- Neither line is part of any step.\n",
		"VALIDATE CONSTRAINT t_c_nn",
	} {
		if !strings.Contains(banner, expect) {
			t.Errorf("banner file lacks %q:\n%s", expect, banner)
		}
	}

	note := readBack(t, dir, "20240817120001_note.sql")

	if !strings.Contains(note, "-- A note about what follows.\n") {
		t.Errorf("the note above the replaced step was deleted:\n%s", note)
	}

	if strings.Contains(note, "an annotation comment inside the step") {
		t.Errorf("a comment inside the replaced step survived:\n%s", note)
	}
}

func TestLintFixClaimsAHeaderAcrossABlankLine(t *testing.T) {
	dir := t.TempDir()

	// The loader accepts a blank line between a step's annotation and its
	// statement; a fix that failed to claim the header across it would
	// leave an empty step behind, and the file would refuse to load.
	write(t, dir, "20240817120000_gap.sql", `-- +mig step: nn

ALTER TABLE t ALTER COLUMN c SET NOT NULL;
`)

	stdout, err := runFix(t, "", "lint", "--dir", dir, "--fix", "--yes")
	if err != nil {
		t.Fatalf("fix: %v\n%s", err, stdout)
	}

	after := readBack(t, dir, "20240817120000_gap.sql")

	if strings.Contains(after, "-- +mig step: nn\n") {
		t.Errorf("the old header was orphaned:\n%s", after)
	}

	// The rewritten file must still load, which re-linting proves.
	if _, _, err := run(t, "lint", "--dir", dir); err != nil {
		t.Fatalf("the rewritten file does not load: %v", err)
	}
}

func TestLintFixReportsWhenThereIsNothingToDo(t *testing.T) {
	dir := t.TempDir()

	write(t, dir, "20240817120000_safe.sql", `-- +mig step: index
-- +mig notx
CREATE INDEX CONCURRENTLY idx_users_email ON users (email);
`)

	stdout, err := runFix(t, "", "lint", "--dir", dir, "--fix", "--yes")
	if err != nil {
		t.Fatalf("fix: %v", err)
	}

	if !strings.Contains(stdout, "no applicable fixes") {
		t.Errorf("no report of an empty run:\n%s", stdout)
	}
}

func TestLintFixRefusesAFormat(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "20240817120000_fk.sql", unsafeMigration)

	_, err := runFix(t, "", "lint", "--dir", dir, "--fix", "--format", "json")
	if err == nil || !strings.Contains(err.Error(), "--fix renders a diff") {
		t.Fatalf("err = %v, want the format refusal", err)
	}
}

func TestLintFixReportsAMissingDirectory(t *testing.T) {
	if _, err := runFix(t, "", "lint", "--dir", "/does/not/exist", "--fix", "--yes"); err == nil {
		t.Fatal("a missing directory fixed clean")
	}
}

// countdownOut accepts a number of writes and then refuses, standing in for
// a terminal that goes away mid-conversation.
type countdownOut struct {
	remaining int
}

func (w *countdownOut) Write(p []byte) (int, error) {
	if w.remaining == 0 {
		return 0, errors.New("terminal gone")
	}

	w.remaining--

	return len(p), nil
}

func TestLintFixReportsARefusingTerminal(t *testing.T) {
	hazardous := func(t *testing.T) string {
		t.Helper()

		dir := t.TempDir()
		write(t, dir, "20240817120000_fk.sql", unsafeMigration)

		return dir
	}

	cases := []struct {
		name   string
		dir    func(*testing.T) string
		stdin  string
		writes int
		args   []string
	}{
		{name: "the diff", dir: hazardous, writes: 0, args: []string{"--yes"}},
		{name: "the prompt", dir: hazardous, writes: 1},
		{name: "the refusal", dir: hazardous, stdin: "n\n", writes: 2},
		{name: "the summary", dir: hazardous, writes: 1, args: []string{"--yes"}},
		{
			name: "the empty report",
			dir: func(t *testing.T) string {
				t.Helper()

				dir := t.TempDir()
				write(t, dir, "20240817120000_safe.sql", "SELECT 1;\n")

				return dir
			},
			writes: 0,
			args:   []string{"--yes"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := cli.New()
			root.SetOut(&countdownOut{remaining: tc.writes})
			root.SetErr(io.Discard)
			root.SetIn(strings.NewReader(tc.stdin))
			root.SetArgs(append([]string{"lint", "--dir", tc.dir(t), "--fix"}, tc.args...))

			if err := root.ExecuteContext(t.Context()); err == nil {
				t.Error("a failed write went unreported")
			}
		})
	}
}
