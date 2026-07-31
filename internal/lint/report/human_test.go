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
	"errors"
	"strings"
	"testing"

	"github.com/tochemey/mig/internal/lint/report"
	"github.com/tochemey/mig/internal/lint/rules"
)

// failWriter refuses every write, for the paths where the sink is the
// problem.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) {
	return 0, errors.New("sink failed")
}

func TestHumanRendersPositionCaretAndDetail(t *testing.T) {
	source := "ALTER TABLE t DROP COLUMN c;\nCREATE INDEX i ON t (c);\n"

	finding := rules.Finding{
		RuleID:   "L001",
		Severity: rules.SeverityWarn,
		Message:  "blocks writes",
		Detail:   "SHARE on t",
		File:     "m.sql",
		Span:     rules.Span{Start: 29, End: 53, Line: 2},
	}

	var out strings.Builder

	if err := report.Human(&out, []rules.Finding{finding}, map[string]string{"m.sql": source}); err != nil {
		t.Fatalf("render: %v", err)
	}

	want := "m.sql:2: warn L001: blocks writes\n" +
		"    CREATE INDEX i ON t (c);\n" +
		"    ^\n" +
		"    SHARE on t\n" +
		"\n" +
		"1 finding(s): 0 error(s), 1 warning(s)\n"

	if out.String() != want {
		t.Errorf("rendered:\n%q\nwant:\n%q", out.String(), want)
	}
}

func TestHumanDegradesWithoutSource(t *testing.T) {
	findings := []rules.Finding{
		{
			// No entry in sources, so no quoted line.
			RuleID: "L010", Severity: rules.SeverityError, Message: "no", File: "gone.sql",
			Span: rules.Span{Line: 3},
		},
		{
			// An unlocated statement has no line to point at, and no detail.
			RuleID: "L002", Severity: rules.SeverityInfo, Message: "hm", File: "m.sql",
		},
		{
			// A finding on a final line that ends without a newline.
			RuleID: "L001", Severity: rules.SeverityWarn, Message: "w", File: "m.sql",
			Span: rules.Span{Start: 0, End: 8, Line: 1},
		},
	}

	var out strings.Builder

	err := report.Human(&out, findings, map[string]string{"m.sql": "SELECT 1"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	rendered := out.String()

	for _, expect := range []string{
		"gone.sql:3: error L010: no\n",
		"m.sql:0: info L002: hm\n",
		"    SELECT 1\n    ^\n",
		"3 finding(s): 1 error(s), 1 warning(s)\n",
	} {
		if !strings.Contains(rendered, expect) {
			t.Errorf("rendered output lacks %q:\n%s", expect, rendered)
		}
	}

	if strings.Contains(rendered, "gone.sql:3: error L010: no\n    ") {
		t.Error("a finding without source still quoted a line")
	}
}

func TestHumanStaysSilentWhenClean(t *testing.T) {
	var out strings.Builder

	if err := report.Human(&out, nil, nil); err != nil {
		t.Fatalf("render: %v", err)
	}

	if out.Len() != 0 {
		t.Errorf("clean run rendered %q", out.String())
	}
}

func TestHumanReportsitsSink(t *testing.T) {
	findings := []rules.Finding{{RuleID: "L001", Severity: rules.SeverityWarn, Message: "w"}}

	if err := report.Human(failWriter{}, findings, nil); err == nil {
		t.Error("a failed write went unreported")
	}
}
