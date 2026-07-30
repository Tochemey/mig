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
	"path/filepath"
	"strings"
	"testing"

	"github.com/tochemey/mig/pkg/mig"
)

// The plan command is the one that answers "what will this run check?" before
// anyone points it at a database. It touches nothing, so it needs no container.

// TestPlanShowsWhatEachStepWillBeJudgedBy covers the whole inference table
// through the command that prints it.
func TestPlanShowsWhatEachStepWillBeJudgedBy(t *testing.T) {
	dir := t.TempDir()

	const body = `-- +mig step: add_column
ALTER TABLE users ADD COLUMN email text;

-- +mig step: index_email
-- +mig notx
CREATE INDEX CONCURRENTLY idx_users_email ON users (email);

-- +mig step: not_null
ALTER TABLE users ADD CONSTRAINT users_email_nn CHECK (email IS NOT NULL) NOT VALID;

-- +mig step: validate
-- +mig notx
ALTER TABLE users VALIDATE CONSTRAINT users_email_nn;

-- +mig step: audit
CREATE TABLE audit (id bigint PRIMARY KEY);

-- +mig step: drop_legacy
ALTER TABLE users DROP COLUMN legacy_email;

-- +mig step: mood
-- +mig notx
ALTER TYPE mood ADD VALUE 'ok';
`

	write(t, dir, "20240817120000_email.sql", body)

	stdout, _, err := run(t, "plan", "--dir", dir)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	want := []string{
		"20240817120000_email",
		"column users.email exists",
		"index idx_users_email exists and is valid and ready",
		"constraint users.users_email_nn exists",
		"constraint users.users_email_nn exists and is validated",
		"relation audit exists",
		"table users exists and column users.legacy_email does not",
		`enum mood has label "ok"`,
	}

	for _, line := range want {
		if !strings.Contains(stdout, line) {
			t.Fatalf("plan does not mention %q:\n%s", line, stdout)
		}
	}

	// Kinds are shown too, since they decide how a step is recovered.
	for _, kind := range []string{"ddl_tx", "ddl_notx"} {
		if !strings.Contains(stdout, kind) {
			t.Fatalf("plan does not show the %s kind:\n%s", kind, stdout)
		}
	}
}

// TestPlanReportsAnUnreconcilableStep covers the refusal being visible before a
// database is involved. Showing the step and failing is more useful than either
// hiding it or printing nothing.
func TestPlanReportsAnUnreconcilableStep(t *testing.T) {
	dir := t.TempDir()

	write(t, dir, "20240817120000_odd.sql", `-- +mig step: vacuum
-- +mig notx
VACUUM users;
`)

	stdout, _, err := run(t, "plan", "--dir", dir)
	if err == nil {
		t.Fatalf("plan accepted an unreconcilable step:\n%s", stdout)
	}

	if !strings.Contains(stdout, mig.Unreconcilable) {
		t.Fatalf("plan does not show which step cannot be reconciled:\n%s", stdout)
	}

	if !strings.Contains(err.Error(), "satisfied:") {
		t.Fatalf("the failure does not say how to fix it: %v", err)
	}
}

// TestPlanShowsATransactionalStepNeedsNoPredicate covers the one kind that is
// judged by its ledger row, because that row commits with its work.
func TestPlanShowsATransactionalStepNeedsNoPredicate(t *testing.T) {
	dir := t.TempDir()

	write(t, dir, "20240817120000_odd.sql", "VACUUM users;\n")

	stdout, _, err := run(t, "plan", "--dir", dir)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	if !strings.Contains(stdout, "ledger") {
		t.Fatalf("plan does not say what judges a transactional step:\n%s", stdout)
	}
}

// TestPlanRejectsAMissingDirectory covers a mistyped --dir.
func TestPlanRejectsAMissingDirectory(t *testing.T) {
	if _, _, err := run(t, "plan", "--dir", filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("plan accepted a directory that does not exist")
	}
}
