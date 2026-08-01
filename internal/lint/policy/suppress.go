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

package policy

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/tochemey/mig/internal/lint/rules"
	"github.com/tochemey/mig/internal/plan"
)

// reasonKey opens the mandatory reason of a directive.
const reasonKey = "reason="

// versionLayout is the shape of the format's own migration version: a
// zero-padded timestamp. A history adopted from another tool carries a
// sequence number instead, which says nothing about when anything was
// written, and dates no directive.
const versionLayout = "20060102150405"

// Directive is one "-- +mig lint:ignore" line: the rule it silences, the
// reason it is required to give, and where it sits.
type Directive struct {
	// File and Span locate the directive.
	File string
	Span rules.Span

	// Step is the step the directive sits in. It is empty for one standing
	// above the first step annotation, which addresses the whole file.
	Step string

	// RuleID is the rule silenced, and is empty when the directive names no
	// rule at all.
	RuleID string

	// Reason is what the author gave for silencing it, and is mandatory: a
	// suppression nobody had to justify is one nobody can audit.
	Reason string

	// Written is when the migration holding the directive was written, and is
	// the zero time when the migration's version is a sequence number rather
	// than a timestamp. It ages the migration rather than the directive, so
	// one added to an old file long after the fact reads older than it is.
	Written time.Time

	// Problem says why the linter cannot honour the directive, and is empty
	// when it can. A directive with one silences nothing and is reported as a
	// finding of its own.
	Problem string

	// Used records whether the directive silenced anything on this run. One
	// that silenced nothing is what an audit is looking for: the statement it
	// was written for is gone, or the rule no longer fires on it.
	Used bool
}

// Silences reports whether the directive covers a finding.
func (d Directive) Silences(finding rules.Finding) bool {
	if d.Problem != "" || d.File != finding.File || d.RuleID != finding.RuleID {
		return false
	}

	return d.Step == "" || d.Step == finding.Step
}

// Scan collects the lint directives in one migration's text.
//
// It reads the file rather than the loaded plan because a directive is not
// part of any step's SQL, and because an audit of suppressions is an audit of
// lines: it has to say where each one sits and how old it is.
func Scan(migration *plan.Migration, content string) []Directive {
	var directives []Directive

	written := writtenAt(migration.Version)
	step := ""
	offset := 0
	number := 0

	for line := range strings.SplitSeq(content, "\n") {
		number++

		if name, isStep := plan.StepOf(line); isStep {
			step = name
		}

		if body, isDirective := plan.LintIgnoreOf(line); isDirective {
			ruleID, reason, problem := parseDirective(body)

			directives = append(directives, Directive{
				File:    migration.File,
				Span:    rules.Span{Start: offset, End: offset + len(line), Line: number},
				Step:    step,
				RuleID:  ruleID,
				Reason:  reason,
				Written: written,
				Problem: problem,
			})
		}

		offset += len(line) + 1
	}

	return directives
}

// parseDirective reads the rule and the reason out of a directive's body,
// and says what is wrong with it when something is.
func parseDirective(body string) (ruleID, reason, problem string) {
	ruleID, rest := field(body)

	switch {
	case ruleID == "":
		return "", "", `names no rule: write lint:ignore <rule> reason="..."`

	case rules.Describe(ruleID) == "":
		return ruleID, "", fmt.Sprintf("names %s, which is no rule this build has", ruleID)

	case ruleID == rules.L000:
		return ruleID, "", fmt.Sprintf("names %s, which cannot be silenced: %s", rules.L000, reasonL000)
	}

	quoted, ok := strings.CutPrefix(strings.TrimSpace(rest), reasonKey)
	if !ok {
		return ruleID, "", fmt.Sprintf(`gives no reason: write reason="..." after %s`, ruleID)
	}

	reason, ok = unquote(quoted)
	if !ok {
		return ruleID, "", `has a reason that is not a non-empty quoted string: reason="..."`
	}

	return ruleID, reason, ""
}

// field takes the first whitespace-separated word off a directive's body.
//
// The recogniser that let the line through accepts any whitespace after
// "lint:ignore", so what follows has to be read the same way: a directive
// written with tabs is the same directive.
func field(body string) (word, rest string) {
	at := strings.IndexFunc(body, unicode.IsSpace)
	if at < 0 {
		return body, ""
	}

	return body[:at], body[at:]
}

// unquote takes the text out of a double-quoted, non-empty string.
func unquote(text string) (string, bool) {
	if len(text) < 2 || text[0] != '"' || text[len(text)-1] != '"' {
		return "", false
	}

	inner := strings.TrimSpace(text[1 : len(text)-1])

	return inner, inner != ""
}

// writtenAt dates a migration by its version, and gives the zero time for a
// version that is not one of the format's timestamps.
func writtenAt(version int64) time.Time {
	text := strconv.FormatInt(version, 10)
	if len(text) != len(versionLayout) {
		return time.Time{}
	}

	at, err := time.Parse(versionLayout, text)
	if err != nil {
		return time.Time{}
	}

	return at
}
