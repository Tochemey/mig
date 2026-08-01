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
	"strings"
	"testing"

	"github.com/tochemey/mig/internal/ledger"
	"github.com/tochemey/mig/pkg/mig"
)

// TestStatusReportsWhatTheLedgerHolds covers the attempts, checkpoints and
// errors no catalog can report.
func TestStatusReportsWhatTheLedgerHolds(t *testing.T) {
	database := newDatabase(t)
	dir := migrationDir(t)
	dsn := shared.DSN(database)

	stdout, _, err := run(t, "status", "--dsn", dsn)
	if err != nil {
		t.Fatalf("status before any run: %v", err)
	}

	if !strings.Contains(stdout, "no migrations recorded") {
		t.Fatalf("stdout is %q", stdout)
	}

	if _, _, err := run(t, "up", "--dsn", dsn, "--dir", dir); err != nil {
		t.Fatalf("up: %v", err)
	}

	stdout, _, err = run(t, "status", "--dsn", dsn)
	if err != nil {
		t.Fatalf("status: %v", err)
	}

	for _, want := range []string{"MIGRATION", "add_email", "succeeded"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout %q does not mention %q", stdout, want)
		}
	}
}

// TestStatusEmitsJSON covers the form a pipeline reads.
func TestStatusEmitsJSON(t *testing.T) {
	database := newDatabase(t)
	dsn := shared.DSN(database)

	stdout, _, err := run(t, "status", "--dsn", dsn, "--json")
	if err != nil {
		t.Fatalf("status before any run: %v", err)
	}

	empty := decodeStatus(t, stdout)
	if len(empty) != 0 {
		t.Fatalf("an untouched database reported %+v", empty)
	}

	if _, _, err := run(t, "up", "--dsn", dsn, "--dir", migrationDir(t)); err != nil {
		t.Fatalf("up: %v", err)
	}

	stdout, _, err = run(t, "status", "--dsn", dsn, "--json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}

	reported := decodeStatus(t, stdout)

	if len(reported) != 1 {
		t.Fatalf("reported %+v, want one step", reported)
	}

	step := reported[0]

	if step.Name != "add_email" || step.Status != ledger.StatusSucceeded || step.Attempts != 1 {
		t.Fatalf("step is %+v", step)
	}
}

// TestStatusRejectsBadInvocations covers the ways the report can be asked for
// and not produced.
func TestStatusRejectsBadInvocations(t *testing.T) {
	requireHarness(t)

	cases := map[string][]string{
		"no dsn":              {"status"},
		"unreachable":         {"status", "--dsn", shared.DSN("no_such_database")},
		"unexpected argument": {"status", "--dsn", "postgres://x/y", "extra"},
	}

	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := run(t, args...); err == nil {
				t.Fatalf("%v was accepted", args)
			}
		})
	}
}

// decodeStatus reads the JSON report from a status run.
func decodeStatus(t *testing.T, stdout string) []mig.StepStatus {
	t.Helper()

	var status []mig.StepStatus

	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &status); err != nil {
		t.Fatalf("decode status %q: %v", stdout, err)
	}

	return status
}
