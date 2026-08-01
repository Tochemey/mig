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
	"io"
	"strings"
	"testing"

	"github.com/tochemey/mig/internal/cli"
)

// chaosWorkload is small enough to run in a test and complete enough to be
// accepted: setup, traffic, and the long reader without which the command
// refuses to run at all.
const chaosWorkload = `
setup:
  - CREATE TABLE probe (id bigint PRIMARY KEY, name text)
  - INSERT INTO probe SELECT g, 'n' || g FROM generate_series(1, 100) g
keys: 100
baseline: 300ms
settle: 300ms
queries:
  - name: point_read
    sql: SELECT name FROM probe WHERE id = $1
    key: true
    rate: 100
slow_read:
  sql: SELECT count(*) FROM probe WHERE id <= 2 AND pg_sleep(0.1) IS NULL
  every: 300ms
`

// TestLintVerifyMeasuresOnADatabaseOfItsOwn covers the command end to end,
// including the part that keeps it safe to run: the --dsn names a server and
// the database it migrates is one the command made.
func TestLintVerifyMeasuresOnADatabaseOfItsOwn(t *testing.T) {
	dir := t.TempDir()

	write(t, dir, "workload.yaml", chaosWorkload)
	write(t, dir, "20240817120000_note.sql", "-- +mig step: add_note\n"+
		"ALTER TABLE probe ADD COLUMN note text;\n")

	stdout, _, err := run(t, "lint", "verify",
		"--dir", dir,
		"--dsn", shared.DSN(newDatabase(t)),
		"--workload", dir+"/workload.yaml")
	if err != nil {
		t.Fatalf("lint verify: %v\n%s", err, stdout)
	}

	for _, want := range []string{"BASELINE", "DURING", "Migration took"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, stdout)
		}
	}
}

// TestLintVerifyFailsOnItsBudget covers the gate: a budget nothing could meet
// makes the command exit non-zero, which is the whole reason it takes one.
func TestLintVerifyFailsOnItsBudget(t *testing.T) {
	dir := t.TempDir()

	write(t, dir, "workload.yaml", chaosWorkload)
	write(t, dir, "20240817120000_note.sql", "-- +mig step: add_note\n"+
		"ALTER TABLE probe ADD COLUMN note text;\n")

	stdout, _, err := run(t, "lint", "verify",
		"--dir", dir,
		"--dsn", shared.DSN(newDatabase(t)),
		"--workload", dir+"/workload.yaml",
		"--budget", "p50=1ns")

	if err == nil {
		t.Fatalf("a budget of one nanosecond passed:\n%s", stdout)
	}

	if !strings.Contains(stdout, "FAIL p50") {
		t.Errorf("the report does not say which term failed:\n%s", stdout)
	}
}

// TestLintVerifyKeepsTheDatabaseWhenAsked covers --keep, which is what a
// person reaches for when the report raised a question the report cannot
// answer.
func TestLintVerifyKeepsTheDatabaseWhenAsked(t *testing.T) {
	dir := t.TempDir()

	write(t, dir, "workload.yaml", chaosWorkload)
	write(t, dir, "20240817120000_note.sql", "-- +mig step: add_note\n"+
		"ALTER TABLE probe ADD COLUMN note text;\n")

	dsn := shared.DSN(newDatabase(t))

	if _, _, err := run(t, "lint", "verify", "--keep",
		"--dir", dir, "--dsn", dsn, "--workload", dir+"/workload.yaml"); err != nil {
		t.Fatalf("lint verify: %v", err)
	}

	// The maintenance database, which is the one place a list of databases
	// can be read from.
	db, err := requireHarness(t).Open(t.Context(), "postgres")
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close pool: %v", err)
		}
	}()

	var kept []string

	rows, err := db.QueryContext(t.Context(),
		"SELECT datname FROM pg_database WHERE datname LIKE 'mig_verify_%'")
	if err != nil {
		t.Fatalf("list databases: %v", err)
	}

	for rows.Next() {
		var name string

		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}

		kept = append(kept, name)
	}

	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatalf("read databases: %v", err)
	}

	if len(kept) == 0 {
		t.Error("--keep dropped the database anyway")
	}

	for _, name := range kept {
		if err := requireHarness(t).DropDatabase(context.Background(), name); err != nil {
			t.Errorf("drop %q: %v", name, err)
		}
	}
}

// TestLintVerifyReportsARefusingTerminal covers the sink failing, where the
// measurements are taken and there is nowhere to put them.
func TestLintVerifyReportsARefusingTerminal(t *testing.T) {
	dir := t.TempDir()

	write(t, dir, "workload.yaml", chaosWorkload)
	write(t, dir, "20240817120000_note.sql", "-- +mig step: add_note\n"+
		"ALTER TABLE probe ADD COLUMN note text;\n")

	root := cli.New()
	root.SetOut(failingOut{})
	root.SetErr(io.Discard)
	root.SetArgs([]string{"lint", "verify",
		"--dir", dir, "--dsn", shared.DSN(newDatabase(t)), "--workload", dir + "/workload.yaml"})

	if err := root.ExecuteContext(t.Context()); err == nil {
		t.Fatal("a failed write went unreported")
	}
}

// TestLintVerifyRefusesWhatItCannotRun covers the arguments it needs, and the
// workload it will not accept.
func TestLintVerifyRefusesWhatItCannotRun(t *testing.T) {
	dir := t.TempDir()

	write(t, dir, "no_reader.yaml", strings.Split(chaosWorkload, "slow_read:")[0])
	write(t, dir, "20240817120000_note.sql", "SELECT 1;\n")

	dsn := shared.DSN(newDatabase(t))

	cases := map[string][]string{
		"no dsn":      {"lint", "verify", "--dir", dir, "--workload", dir + "/none.yaml"},
		"no workload": {"lint", "verify", "--dir", dir, "--dsn", dsn},
		"a workload not there": {"lint", "verify", "--dir", dir, "--dsn", dsn,
			"--workload", dir + "/none.yaml"},
		"a workload with no slow reader": {"lint", "verify", "--dir", dir, "--dsn", dsn,
			"--workload", dir + "/no_reader.yaml"},
		"a budget that is not one": {"lint", "verify", "--dir", dir, "--dsn", dsn,
			"--workload", dir + "/no_reader.yaml", "--budget", "p99=soon"},
		"a dsn that is not a server": {"lint", "verify", "--dir", dir,
			"--dsn", "not-a-url", "--workload", dir + "/no_reader.yaml"},
	}

	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := run(t, args...); err == nil {
				t.Error("the command ran")
			}
		})
	}
}
