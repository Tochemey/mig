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
	"strings"

	"github.com/tochemey/mig/internal/lint/rules"
)

// blank is what an empty cell holds in a rendered table, since a column that
// is truly empty reads as a rendering fault rather than as nothing to report.
const blank = "-"

// Markdown renders one pull-request comment: what these migrations lock, for
// how long, and what that blocks.
//
// It is a summary rather than a list of annotations. Those are [SARIF]'s, and
// this is the shape of the risk in one place, for a reviewer deciding whether
// to approve.
func Markdown(w io.Writer, findings []rules.Finding) error {
	out := &strings.Builder{}

	_, _ = fmt.Fprintf(out, "### %s\n\n", toolName)

	if len(findings) == 0 {
		_, _ = fmt.Fprintln(out, "No lock hazards found.")

		return flush(w, out.String())
	}

	errors, warnings, fixable := 0, 0, 0

	for _, finding := range findings {
		switch finding.Severity {
		case rules.SeverityError:
			errors++
		case rules.SeverityWarn:
			warnings++
		}

		if finding.Fix != "" {
			fixable++
		}
	}

	_, _ = fmt.Fprintf(out, "%s: %d error(s), %d warning(s).\n\n",
		count(len(findings), "finding"), errors, warnings)

	_, _ = fmt.Fprintln(out, "| file | rule | severity | what happens | lock | estimate |")
	_, _ = fmt.Fprintln(out, "| --- | --- | --- | --- | --- | --- |")

	for _, finding := range findings {
		_, _ = fmt.Fprintf(out, "| %s | %s | %s | %s | %s | %s |\n",
			cell(where(finding)), cell(finding.RuleID), cell(finding.Severity.String()),
			cell(finding.Message), cell(finding.Detail), cell(finding.Estimate))
	}

	if fixable > 0 {
		_, _ = fmt.Fprintf(out, "\n%s a rewrite available: `mig lint --fix`.\n",
			have(fixable))
	}

	return flush(w, out.String())
}

// where names the file and, when it is known, the line.
func where(finding rules.Finding) string {
	if finding.Span.Line == 0 {
		return fmt.Sprintf("`%s`", finding.File)
	}

	return fmt.Sprintf("`%s:%d`", finding.File, finding.Span.Line)
}

// have phrases the fix count as the sentence it opens.
func have(fixable int) string {
	if fixable == 1 {
		return "1 finding has"
	}

	return fmt.Sprintf("%d findings have", fixable)
}

// count renders a quantity with its noun, pluralised.
func count(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}

	return fmt.Sprintf("%d %ss", n, noun)
}

// cell escapes the text of one table cell: a pipe would end the column early,
// and a newline the row.
func cell(value string) string {
	if value == "" {
		return blank
	}

	value = strings.ReplaceAll(value, "|", `\|`)

	return strings.Join(strings.Fields(value), " ")
}

// flush writes the rendered comment.
func flush(w io.Writer, rendered string) error {
	if _, err := io.WriteString(w, rendered); err != nil {
		return fmt.Errorf("write report: %w", err)
	}

	return nil
}
