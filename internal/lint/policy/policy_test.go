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
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tochemey/mig/internal/lint/policy"
	"github.com/tochemey/mig/internal/lint/rules"
)

// parse reads a policy for a directory, failing the test if it will not load.
func parse(t *testing.T, body, dir string) *policy.Policy {
	t.Helper()

	loaded, err := policy.Parse([]byte(body), dir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	return loaded
}

func TestParseReadsEverySetting(t *testing.T) {
	loaded := parse(t, `
target_version: 13
thresholds:
  big_rows: 5000000
  big_bytes: 10737418240
  small_rows: 100
  small_bytes: 1048576
rules:
  L001: error
  L022: off
`, "migrations")

	if loaded.TargetVersion() != 13 {
		t.Errorf("target version = %d, want 13", loaded.TargetVersion())
	}

	limits := loaded.Thresholds()
	if limits.BigRows != 5_000_000 || limits.SmallRows != 100 {
		t.Errorf("thresholds = %+v", limits)
	}

	findings := loaded.Apply([]rules.Finding{
		{RuleID: rules.L001, Severity: rules.SeverityWarn},
		{RuleID: rules.L022, Severity: rules.SeverityInfo},
		{RuleID: rules.L010, Severity: rules.SeverityError},
	})

	if len(findings) != 2 {
		t.Fatalf("kept %d findings, want the two the policy did not turn off: %+v", len(findings), findings)
	}

	if findings[0].Severity != rules.SeverityError {
		t.Errorf("L001 came back %s, want the policy's error", findings[0].Severity)
	}

	if findings[1].Severity != rules.SeverityError {
		t.Errorf("a rule the policy says nothing about was re-graded to %s", findings[1].Severity)
	}
}

// TestParseAppliesTheOverridesForThisDirectory pins what per-directory means:
// the override is keyed by the directory being linted, and the last matching
// one wins.
func TestParseAppliesTheOverridesForThisDirectory(t *testing.T) {
	body := `
rules:
  L001: error
overrides:
  - path: services/legacy
    rules:
      L001: off
  - path: services/legacy/migrations
    rules:
      L001: info
  - path: services/billing
    rules:
      L001: warn
`

	cases := []struct {
		name string
		dir  string
		want rules.Severity
	}{
		{name: "no override matches", dir: "services/api/migrations", want: rules.SeverityError},
		{name: "the deeper override wins", dir: "services/legacy/migrations", want: rules.SeverityInfo},
		{name: "a sibling directory is untouched", dir: "services/billing", want: rules.SeverityWarn},
		{name: "a path that only shares a prefix", dir: "services/legacy-two", want: rules.SeverityError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings := parse(t, body, tc.dir).
				Apply([]rules.Finding{{RuleID: rules.L001, Severity: rules.SeverityWarn}})

			if len(findings) != 1 {
				t.Fatalf("kept %d findings, want 1", len(findings))
			}

			if findings[0].Severity != tc.want {
				t.Errorf("severity = %s, want %s", findings[0].Severity, tc.want)
			}
		})
	}
}

// TestParseTurnsOffARuleUnderOneDirectory covers the override that drops a
// finding rather than re-grading it.
func TestParseTurnsOffARuleUnderOneDirectory(t *testing.T) {
	loaded := parse(t, `
overrides:
  - path: legacy
    rules:
      L001: off
`, "legacy")

	if findings := loaded.Apply([]rules.Finding{{RuleID: rules.L001}}); len(findings) != 0 {
		t.Errorf("kept %+v, want nothing", findings)
	}
}

func TestParseRejectsWhatItCannotHonour(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{name: "malformed yaml", body: "rules: [", want: "parse policy"},
		{name: "an unknown setting", body: "severity: error", want: "field severity not found"},
		{name: "a rule this build does not have", body: "rules:\n  L999: error", want: "no rule this build has"},
		{name: "a severity that is not one", body: "rules:\n  L001: shout", want: "not off, info, warn or error"},
		{name: "an override without a path", body: "overrides:\n  - rules:\n      L001: off", want: "needs a path"},
		{
			name: "a misspelled rule under a directory this run is not linting",
			body: "overrides:\n  - path: elsewhere\n    rules:\n      L999: off",
			want: "no rule this build has",
		},
		{name: "a negative threshold", body: "thresholds:\n  big_rows: -1", want: "cannot be negative"},
		{name: "the linter's own rule", body: "rules:\n  L000: off", want: "L000 cannot be graded"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := policy.Parse([]byte(tc.body), "migrations")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestParseAcceptsAnEmptyPolicy covers the file that exists and says nothing,
// which is not a mistake.
func TestParseAcceptsAnEmptyPolicy(t *testing.T) {
	loaded := parse(t, "", "migrations")

	if loaded.TargetVersion() != 0 || loaded.Thresholds() != (rules.Thresholds{}) {
		t.Errorf("an empty policy came back with settings: %+v", loaded)
	}

	findings := []rules.Finding{{RuleID: rules.L001, Severity: rules.SeverityWarn}}
	if kept := loaded.Apply(findings); len(kept) != 1 || kept[0].Severity != rules.SeverityWarn {
		t.Errorf("an empty policy re-graded a finding: %+v", kept)
	}
}

// TestNoPolicyBehavesLikeAnEmptyOne covers the offline path, which passes
// nothing at all.
func TestNoPolicyBehavesLikeAnEmptyOne(t *testing.T) {
	var none *policy.Policy

	if none.TargetVersion() != 0 || none.Thresholds() != (rules.Thresholds{}) {
		t.Error("the absent policy claims settings")
	}

	findings := []rules.Finding{{RuleID: rules.L001, Severity: rules.SeverityWarn}}
	if kept := none.Apply(findings); len(kept) != 1 {
		t.Errorf("the absent policy dropped a finding: %+v", kept)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, policy.FileName)

	if err := os.WriteFile(path, []byte("target_version: 12\n"), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}

	loaded, err := policy.Load(path, "migrations")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if loaded.TargetVersion() != 12 {
		t.Errorf("target version = %d, want 12", loaded.TargetVersion())
	}

	// The caller tells a policy that is not there from one that will not
	// load, which is what makes the conventional file optional.
	if _, err := policy.Load(filepath.Join(dir, "absent.yaml"), "migrations"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing policy came back as %v, want a not-exist error", err)
	}

	if err := os.WriteFile(path, []byte("rules: ["), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}

	if _, err := policy.Load(path, "migrations"); err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("err = %v, want it to name the file", err)
	}
}
