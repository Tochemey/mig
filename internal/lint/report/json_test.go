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
	"encoding/json"
	"strings"
	"testing"

	"github.com/tochemey/mig/internal/lint/report"
	"github.com/tochemey/mig/internal/lint/rules"
)

func TestJSONRendersFindings(t *testing.T) {
	findings := []rules.Finding{{
		RuleID: "L001", Severity: rules.SeverityWarn, Message: "w",
		File: "m.sql", Step: "s", Span: rules.Span{Start: 1, End: 2, Line: 1},
	}}

	var out strings.Builder

	if err := report.JSON(&out, findings); err != nil {
		t.Fatalf("render: %v", err)
	}

	var decoded struct {
		Findings []map[string]any `json:"findings"`
	}

	if err := json.Unmarshal([]byte(out.String()), &decoded); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out.String())
	}

	if len(decoded.Findings) != 1 || decoded.Findings[0]["rule"] != "L001" ||
		decoded.Findings[0]["severity"] != "warn" {
		t.Errorf("decoded %+v", decoded.Findings)
	}
}

func TestJSONRendersAnEmptyListWhenClean(t *testing.T) {
	var out strings.Builder

	if err := report.JSON(&out, nil); err != nil {
		t.Fatalf("render: %v", err)
	}

	want := "{\n  \"findings\": []\n}\n"
	if out.String() != want {
		t.Errorf("rendered %q, want %q", out.String(), want)
	}
}

func TestJSONReportsItsSink(t *testing.T) {
	if err := report.JSON(failWriter{}, nil); err == nil {
		t.Error("a failed write went unreported")
	}
}
