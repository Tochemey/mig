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

package report

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/tochemey/mig/internal/lint/policy"
)

// day is the unit a suppression's age is reported in; anything finer is noise
// on a number this approximate.
const day = 24 * time.Hour

// The states a directive is reported in.
const (
	stateUsed   = "used"
	stateUnused = "unused"
	stateBroken = "broken"
)

// Suppressions renders the audit: every lint directive in the migrations,
// what it silences, how old it is and whether it is still doing anything.
//
// Age is the migration's, taken from the version in its file name, so a
// directive added to an old migration long after the fact reads older than it
// is, and one in a history adopted from another tool has no age at all. The
// clock is the caller's, so the report can be tested.
func Suppressions(w io.Writer, directives []policy.Directive, now time.Time) error {
	if len(directives) == 0 {
		if _, err := io.WriteString(w, "no lint directives\n"); err != nil {
			return fmt.Errorf("write report: %w", err)
		}

		return nil
	}

	// The rows are buffered until the columns are known, so the sink's own
	// failure surfaces at the flush and is reported there.
	out := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	_, _ = fmt.Fprintln(out, "FILE\tLINE\tRULE\tAGE\tSTATE\tREASON")

	for _, directive := range directives {
		reason := directive.Reason
		if directive.Problem != "" {
			reason = directive.Problem
		}

		_, _ = fmt.Fprintf(out, "%s\t%d\t%s\t%s\t%s\t%s\n",
			directive.File, directive.Span.Line, ruleOf(directive),
			age(directive.Written, now), state(directive), reason)
	}

	if err := out.Flush(); err != nil {
		return fmt.Errorf("write report: %w", err)
	}

	return nil
}

// ruleOf names the rule a directive silences.
func ruleOf(directive policy.Directive) string {
	if directive.RuleID == "" {
		return blank
	}

	return directive.RuleID
}

// state says whether the directive is still earning its place.
func state(directive policy.Directive) string {
	switch {
	case directive.Problem != "":
		return stateBroken
	case directive.Used:
		return stateUsed
	default:
		return stateUnused
	}
}

// age renders how long ago the migration was written.
func age(written, now time.Time) string {
	if written.IsZero() {
		return blank
	}

	days := int(now.Sub(written) / day)
	if days < 0 {
		days = 0
	}

	return fmt.Sprintf("%dd", days)
}
