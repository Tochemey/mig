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

package policy_test

import (
	"strings"
	"testing"

	"github.com/tochemey/mig/internal/lint/policy"
	"github.com/tochemey/mig/internal/lint/rules"
	"github.com/tochemey/mig/internal/plan"
)

// migrationOf is the file a directive was read out of. Scan reads its name
// and its version and nothing else.
func migrationOf(version int64) *plan.Migration {
	return &plan.Migration{File: "1_m.sql", Version: version}
}

// TestScanReadsDirectivesAndTheStepsTheyBelongTo covers the placement rules:
// a directive inside a step addresses that step, and one above the first step
// addresses the file.
func TestScanReadsDirectivesAndTheStepsTheyBelongTo(t *testing.T) {
	content := `-- +mig lint:ignore L010 reason="this schema is rebuilt nightly"
-- +mig step: one
-- +mig lint:ignore L001 reason="the table has twelve rows"
CREATE INDEX i ON t (c);

-- +mig step: two
VACUUM FULL t;
`

	directives := policy.Scan(migrationOf(20240817120000), content)
	if len(directives) != 2 {
		t.Fatalf("scanned %d directives, want 2: %+v", len(directives), directives)
	}

	file, step := directives[0], directives[1]

	if file.Step != "" || file.RuleID != rules.L010 || file.Span.Line != 1 {
		t.Errorf("the file-level directive read as %+v", file)
	}

	if step.Step != "one" || step.RuleID != rules.L001 || step.Span.Line != 3 {
		t.Errorf("the step-level directive read as %+v", step)
	}

	if step.Reason != "the table has twelve rows" {
		t.Errorf("reason = %q", step.Reason)
	}

	// A directive written with tabs is the same directive: the recogniser
	// that let the line through accepts any whitespace after lint:ignore, and
	// what follows it is read the same way.
	tabbed := policy.Scan(migrationOf(1), "-- +mig lint:ignore\tL001\treason=\"tabs, not spaces\"\n")
	if len(tabbed) != 1 || tabbed[0].Problem != "" || tabbed[0].RuleID != rules.L001 {
		t.Errorf("a tab-separated directive read as %+v", tabbed)
	}

	// The span has to cover the directive's own line, which is what puts the
	// caret under it when the linter complains about the directive itself.
	if got := content[file.Span.Start:file.Span.End]; !strings.HasPrefix(got, "-- +mig lint:ignore") {
		t.Errorf("span covers %q", got)
	}

	if file.Written.Year() != 2024 || file.Written.Month() != 8 {
		t.Errorf("written = %v, want the version's timestamp", file.Written)
	}
}

// TestScanDatesOnlyTimestampVersions pins what age means: a migration adopted
// from another tool carries a sequence number, which dates nothing.
func TestScanDatesOnlyTimestampVersions(t *testing.T) {
	content := "-- +mig lint:ignore L010 reason=\"rebuilt nightly\"\nVACUUM FULL t;\n"

	cases := []struct {
		name    string
		version int64
		dated   bool
	}{
		{name: "timestamp", version: 20240817120000, dated: true},
		{name: "sequence number", version: 7, dated: false},
		{name: "fourteen digits that are no date", version: 99999999999999, dated: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			directives := policy.Scan(migrationOf(tc.version), content)
			if len(directives) != 1 {
				t.Fatalf("scanned %d directives, want 1", len(directives))
			}

			if dated := !directives[0].Written.IsZero(); dated != tc.dated {
				t.Errorf("dated = %v, want %v", dated, tc.dated)
			}
		})
	}
}

// TestScanReportsDirectivesItCannotHonour covers the design's rule that a
// suppression nobody had to justify is a lint error of its own, and the
// spellings around it.
func TestScanReportsDirectivesItCannotHonour(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{
			name: "no rule",
			line: "-- +mig lint:ignore",
			want: "names no rule",
		},
		{
			name: "a rule this build does not have",
			line: `-- +mig lint:ignore L999 reason="x"`,
			want: "no rule this build has",
		},
		{
			name: "no reason",
			line: "-- +mig lint:ignore L010",
			want: "gives no reason",
		},
		{
			name: "a reason that is not a setting",
			line: `-- +mig lint:ignore L010 "rebuilt nightly"`,
			want: "gives no reason",
		},
		{
			name: "an unquoted reason",
			line: "-- +mig lint:ignore L010 reason=rebuilt",
			want: "not a non-empty quoted string",
		},
		{
			name: "an empty reason",
			line: `-- +mig lint:ignore L010 reason="   "`,
			want: "not a non-empty quoted string",
		},
		{
			name: "the linter's own rule",
			line: `-- +mig lint:ignore L000 reason="broken directives should not fail CI"`,
			want: "cannot be silenced",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			directives := policy.Scan(migrationOf(1), tc.line+"\nVACUUM FULL t;\n")
			if len(directives) != 1 {
				t.Fatalf("scanned %d directives, want 1", len(directives))
			}

			if !strings.Contains(directives[0].Problem, tc.want) {
				t.Errorf("problem = %q, want it to mention %q", directives[0].Problem, tc.want)
			}
		})
	}
}

// TestScanIgnoresLinesThatAreNotDirectives covers the recognition boundary:
// the loader rejects a misspelled annotation, and the linter must not read one
// as a directive on the way there.
func TestScanIgnoresLinesThatAreNotDirectives(t *testing.T) {
	content := `-- lint:ignore L010 reason="a plain comment"
-- +mig lint:ignoreL010 reason="a misspelling"
-- +mig notx
VACUUM FULL t;
`

	if directives := policy.Scan(migrationOf(1), content); len(directives) != 0 {
		t.Errorf("scanned %+v, want nothing", directives)
	}
}

func TestSilences(t *testing.T) {
	finding := rules.Finding{RuleID: rules.L010, File: "1_m.sql", Step: "compact"}

	cases := []struct {
		name      string
		directive policy.Directive
		want      bool
	}{
		{
			name:      "the step it sits in",
			directive: policy.Directive{File: "1_m.sql", Step: "compact", RuleID: rules.L010},
			want:      true,
		},
		{
			name:      "the whole file",
			directive: policy.Directive{File: "1_m.sql", RuleID: rules.L010},
			want:      true,
		},
		{
			name:      "another step",
			directive: policy.Directive{File: "1_m.sql", Step: "other", RuleID: rules.L010},
		},
		{
			name:      "another file",
			directive: policy.Directive{File: "2_m.sql", RuleID: rules.L010},
		},
		{
			name:      "another rule",
			directive: policy.Directive{File: "1_m.sql", RuleID: rules.L001},
		},
		{
			name: "a directive the linter cannot honour",
			directive: policy.Directive{
				File: "1_m.sql", RuleID: rules.L010, Problem: "gives no reason",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.directive.Silences(finding); got != tc.want {
				t.Errorf("Silences = %v, want %v", got, tc.want)
			}
		})
	}
}
