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

package lint_test

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/tochemey/mig/internal/lint"
	"github.com/tochemey/mig/internal/parse"
	"github.com/tochemey/mig/internal/plan"
)

// current is the target version the tests run at.
const current = 18

// load reads a plan out of an in-memory directory.
func load(t *testing.T, fsys fstest.MapFS) *plan.Plan {
	t.Helper()

	loaded, err := plan.LoadFS(fsys)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	return loaded
}

// TestRunOrdersAndLocatesFindings covers the pipeline end to end: findings
// sorted by file, position and rule, spans pointing at the statement text,
// and every linted file's contents handed back for rendering.
func TestRunOrdersAndLocatesFindings(t *testing.T) {
	first := "-- +mig step: one\nCREATE INDEX i ON t (c);\n"

	// One statement tripping two rules pins the rule-id tiebreak on an
	// identical span.
	second := "-- +mig step: two\n" +
		"ALTER TABLE t ADD COLUMN h text UNIQUE DEFAULT gen_random_uuid()::text;\n"

	fsys := fstest.MapFS{
		"1_a.sql": &fstest.MapFile{Data: []byte(first)},
		"2_b.sql": &fstest.MapFile{Data: []byte(second)},
	}

	findings, sources, err := lint.Run(fsys, load(t, fsys), current)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	got := make([]string, 0, len(findings))

	for _, f := range findings {
		got = append(got, f.File+"/"+f.RuleID)
	}

	want := []string{"1_a.sql/L001", "2_b.sql/L003", "2_b.sql/L009"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("findings = %v, want %v", got, want)
	}

	if sources["1_a.sql"] != first || sources["2_b.sql"] != second {
		t.Error("sources do not carry the linted files' contents")
	}

	index := findings[0]

	if index.Span.Line != 2 || index.Step != "one" {
		t.Errorf("finding located at line %d step %q, want line 2 step one",
			index.Span.Line, index.Step)
	}

	// The loader splits statements without their terminating semicolon, so
	// the span stops before it too.
	if text := first[index.Span.Start:index.Span.End]; text != "CREATE INDEX i ON t (c)" {
		t.Errorf("span covers %q, not the statement", text)
	}

	if findings[1].Span != findings[2].Span {
		t.Error("two findings on one statement should share its span")
	}
}

// TestRunLeavesRewrittenStatementsUnlocated covers a backfill body, whose
// cursor placeholders were rewritten after loading and so cannot be found
// verbatim in the file.
func TestRunLeavesRewrittenStatementsUnlocated(t *testing.T) {
	body := "-- +mig step: fill\n" +
		"-- +mig backfill: table=t key=id\n" +
		"-- +mig satisfied: sql(SELECT true)\n" +
		"UPDATE t SET c = 1 WHERE id > :cursor_lo AND id <= :cursor_hi;\n"

	fsys := fstest.MapFS{"1_fill.sql": &fstest.MapFile{Data: []byte(body)}}

	findings, _, err := lint.Run(fsys, load(t, fsys), current)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(findings) != 0 {
		t.Fatalf("a chunked backfill was flagged: %+v", findings)
	}
}

// TestRunStripsFixesOnSharedSteps pins the application boundary: a fix
// replaces its whole step, so a statement sharing a step keeps the finding
// and loses the fix.
func TestRunStripsFixesOnSharedSteps(t *testing.T) {
	shared := "-- +mig step: both\n" +
		"ALTER TABLE t ALTER COLUMN c SET NOT NULL;\n" +
		"SELECT 1;\n"

	alone := "-- +mig step: one\n" +
		"ALTER TABLE t ALTER COLUMN c SET NOT NULL;\n"

	fsys := fstest.MapFS{
		"1_shared.sql": &fstest.MapFile{Data: []byte(shared)},
		"2_alone.sql":  &fstest.MapFile{Data: []byte(alone)},
	}

	findings, _, err := lint.Run(fsys, load(t, fsys), current)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(findings) != 2 {
		t.Fatalf("found %d findings, want 2: %+v", len(findings), findings)
	}

	if findings[0].Fix != "" {
		t.Error("a statement sharing its step kept a fix")
	}

	if findings[1].Fix == "" {
		t.Error("a statement alone in its step lost its fix")
	}
}

// TestRunRejectsHandBuiltPlans covers the failures only a plan that did not
// come from the loader can produce.
func TestRunRejectsHandBuiltPlans(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want string
	}{
		{name: "unparsable statement", sql: "NOT SQL", want: "parse statement"},
		{name: "two statements in one", sql: "SELECT 1; SELECT 2", want: "expected one statement"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &plan.Plan{Migrations: []plan.Migration{{
				File:  "x.sql",
				Steps: []plan.Step{{Name: "s", Statements: []parse.Statement{{SQL: tc.sql}}}},
			}}}

			fsys := fstest.MapFS{"x.sql": &fstest.MapFile{Data: []byte(tc.sql)}}

			_, _, err := lint.Run(fsys, p, current)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		p := &plan.Plan{Migrations: []plan.Migration{{File: "gone.sql"}}}

		_, _, err := lint.Run(fstest.MapFS{}, p, current)
		if err == nil || !strings.Contains(err.Error(), "read migration") {
			t.Errorf("err = %v, want a read failure", err)
		}
	})
}
