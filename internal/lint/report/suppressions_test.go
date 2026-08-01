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
	"time"

	"github.com/tochemey/mig/internal/lint/policy"
	"github.com/tochemey/mig/internal/lint/report"
	"github.com/tochemey/mig/internal/lint/rules"
)

// TestSuppressionsReportsEachDirectiveWithItsAgeAndState covers the audit the
// design asks for: every directive, how old it is, and whether it is still
// doing anything.
func TestSuppressionsReportsEachDirectiveWithItsAgeAndState(t *testing.T) {
	written := time.Date(2024, time.August, 17, 12, 0, 0, 0, time.UTC)
	now := written.Add(100 * 24 * time.Hour)

	directives := []policy.Directive{
		{
			File: "1_m.sql", Span: rules.Span{Line: 3}, RuleID: rules.L010,
			Reason: "this schema is rebuilt nightly", Written: written, Used: true,
		},
		{
			File: "2_m.sql", Span: rules.Span{Line: 7}, RuleID: rules.L001,
			Reason: "the table has twelve rows", Written: written,
		},
		{
			File: "3_m.sql", Span: rules.Span{Line: 1},
			Problem: "names no rule",
		},
	}

	var out strings.Builder

	if err := report.Suppressions(&out, directives, now); err != nil {
		t.Fatalf("render: %v", err)
	}

	got := out.String()

	for _, want := range []string{
		"FILE", "LINE", "RULE", "AGE", "STATE", "REASON",
		"1_m.sql  3     L010  100d  used    this schema is rebuilt nightly",
		"2_m.sql  7     L001  100d  unused  the table has twelve rows",
		"3_m.sql  1     -     -     broken  names no rule",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("audit lacks %q:\n%s", want, got)
		}
	}
}

// TestSuppressionsRefusesToReportAgeBackwards covers a migration dated ahead
// of the clock, which reads as new rather than as a negative number of days.
func TestSuppressionsRefusesToReportAgeBackwards(t *testing.T) {
	written := time.Date(2024, time.August, 17, 12, 0, 0, 0, time.UTC)

	directives := []policy.Directive{{
		File: "1_m.sql", Span: rules.Span{Line: 1}, RuleID: rules.L010,
		Reason: "x", Written: written,
	}}

	var out strings.Builder

	if err := report.Suppressions(&out, directives, written.Add(-48*time.Hour)); err != nil {
		t.Fatalf("render: %v", err)
	}

	if !strings.Contains(out.String(), "0d") {
		t.Errorf("audit reports a negative age:\n%s", out.String())
	}
}

func TestSuppressionsSaysSoWhenThereAreNone(t *testing.T) {
	var out strings.Builder

	if err := report.Suppressions(&out, nil, time.Time{}); err != nil {
		t.Fatalf("render: %v", err)
	}

	if out.String() != "no lint directives\n" {
		t.Errorf("rendered %q", out.String())
	}
}

func TestSuppressionsReportsItsSink(t *testing.T) {
	if err := report.Suppressions(failWriter{}, nil, time.Time{}); err == nil {
		t.Error("a failed write went unreported")
	}

	directives := []policy.Directive{{File: "1_m.sql", RuleID: rules.L010, Reason: "x"}}

	if err := report.Suppressions(failWriter{}, directives, time.Time{}); err == nil {
		t.Error("a failed write of the table went unreported")
	}
}
