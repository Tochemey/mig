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

package report_test

import (
	"strings"
	"testing"

	"github.com/tochemey/mig/internal/lint/report"
	"github.com/tochemey/mig/internal/lint/rules"
)

// markdownOf renders one comment.
func markdownOf(t *testing.T, findings []rules.Finding) string {
	t.Helper()

	var out strings.Builder

	if err := report.Markdown(&out, findings); err != nil {
		t.Fatalf("render: %v", err)
	}

	return out.String()
}

func TestMarkdownSummarisesTheLocks(t *testing.T) {
	findings := []rules.Finding{
		{
			RuleID: rules.L004, Severity: rules.SeverityError,
			Message:  "this changes the column's type, which rewrites the table",
			Detail:   "ACCESS EXCLUSIVE on users (41.2M rows, 340.0 GB); blocks reads and writes",
			Estimate: "estimated 38m to 55m",
			File:     "20240817120000_widen.sql", Span: rules.Span{Line: 3},
			Fix: "-- +mig step: x\n",
		},
		{
			RuleID: rules.L001, Severity: rules.SeverityWarn,
			Message: "this build takes SHARE",
			File:    "20240817120000_widen.sql",
		},
	}

	got := markdownOf(t, findings)

	for _, want := range []string{
		"### mig lint",
		"2 findings: 1 error(s), 1 warning(s).",
		"| file | rule | severity | what happens | lock | estimate |",
		"| `20240817120000_widen.sql:3` | L004 | error |",
		"ACCESS EXCLUSIVE on users (41.2M rows, 340.0 GB); blocks reads and writes",
		"estimated 38m to 55m",
		"1 finding has a rewrite available: `mig lint --fix`.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("comment lacks %q:\n%s", want, got)
		}
	}

	// A finding the engine could not place is named by its file alone rather
	// than pointed at line zero, and its empty columns keep their shape.
	if !strings.Contains(got, "| `20240817120000_widen.sql` | L001 | warn | this build takes SHARE | - | - |") {
		t.Errorf("the unlocated finding rendered wrong:\n%s", got)
	}
}

// TestMarkdownEscapesTheTable pins that a finding cannot break out of its
// cell: a pipe would end the column and a newline the row.
func TestMarkdownEscapesTheTable(t *testing.T) {
	findings := []rules.Finding{{
		RuleID: rules.L040, Severity: rules.SeverityWarn,
		Message: "this UPDATE has no WHERE\nand touches a | b",
		File:    "1_m.sql", Span: rules.Span{Line: 1},
	}}

	got := markdownOf(t, findings)

	if strings.Contains(got, "a | b") {
		t.Errorf("an unescaped pipe reached the table:\n%s", got)
	}

	if !strings.Contains(got, `this UPDATE has no WHERE and touches a \| b`) {
		t.Errorf("the message did not survive escaping:\n%s", got)
	}

	// One finding is one row: the heading, the count and the table's two
	// header lines are the rest, and a message with a newline in it must not
	// have added a seventh.
	if lines := strings.Count(strings.TrimSpace(got), "\n"); lines != 6 {
		t.Errorf("comment has %d line breaks:\n%s", lines, got)
	}
}

// TestMarkdownSaysSoWhenThereIsNothing covers the comment a clean run posts,
// which has to replace the previous one rather than leave it standing.
func TestMarkdownSaysSoWhenThereIsNothing(t *testing.T) {
	got := markdownOf(t, nil)

	if !strings.Contains(got, "No lock hazards found.") {
		t.Errorf("a clean run rendered:\n%s", got)
	}
}

// TestMarkdownCountsOneOfEach covers the singular phrasings.
func TestMarkdownCountsOneOfEach(t *testing.T) {
	got := markdownOf(t, []rules.Finding{{
		RuleID: rules.L001, Severity: rules.SeverityWarn, File: "1_m.sql",
	}})

	if !strings.Contains(got, "1 finding: 0 error(s), 1 warning(s).") {
		t.Errorf("comment lacks the singular count:\n%s", got)
	}

	if strings.Contains(got, "rewrite available") {
		t.Errorf("a finding without a fix advertised one:\n%s", got)
	}
}

// TestMarkdownCountsSeveralFixes covers the plural of the fix line.
func TestMarkdownCountsSeveralFixes(t *testing.T) {
	fixable := rules.Finding{
		RuleID: rules.L001, Severity: rules.SeverityWarn, File: "1_m.sql", Fix: "-- +mig step: x\n",
	}

	got := markdownOf(t, []rules.Finding{fixable, fixable})

	if !strings.Contains(got, "2 findings have a rewrite available") {
		t.Errorf("comment lacks the plural fix line:\n%s", got)
	}
}

func TestMarkdownReportsItsSink(t *testing.T) {
	if err := report.Markdown(failWriter{}, nil); err == nil {
		t.Error("a failed write went unreported")
	}
}
