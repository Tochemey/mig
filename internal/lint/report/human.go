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

// Package report renders lint findings: human for a terminal, JSON for
// scripting.
package report

import (
	"fmt"
	"io"
	"strings"

	"github.com/tochemey/mig/internal/lint/rules"
)

// Human renders findings for a terminal: position, severity, rule and
// message, the offending source line with a caret under it, and the lock
// detail. Sources maps each file to its contents; a finding whose file or
// position is unknown is rendered without the source line.
func Human(w io.Writer, findings []rules.Finding, sources map[string]string) error {
	if len(findings) == 0 {
		return nil
	}

	out := &strings.Builder{}
	errors, warnings := 0, 0

	for _, f := range findings {
		switch f.Severity {
		case rules.SeverityError:
			errors++
		case rules.SeverityWarn:
			warnings++
		}

		_, _ = fmt.Fprintf(out, "%s:%d: %s %s: %s\n", f.File, f.Span.Line, f.Severity, f.RuleID, f.Message)

		if source, ok := sources[f.File]; ok && f.Span.Line > 0 {
			line, column := lineAt(source, f.Span.Start)
			_, _ = fmt.Fprintf(out, "    %s\n    %s^\n", line, strings.Repeat(" ", column))
		}

		if f.Detail != "" {
			_, _ = fmt.Fprintf(out, "    %s\n", f.Detail)
		}

		if f.Fix != "" {
			_, _ = fmt.Fprintln(out, "    a rewrite is available: mig lint --fix")
		}

		_, _ = fmt.Fprintln(out)
	}

	_, _ = fmt.Fprintf(out, "%d finding(s): %d error(s), %d warning(s)\n", len(findings), errors, warnings)

	if _, err := io.WriteString(w, out.String()); err != nil {
		return fmt.Errorf("write report: %w", err)
	}

	return nil
}

// lineAt returns the line holding a byte offset and the offset's column
// within it.
func lineAt(source string, offset int) (string, int) {
	start := strings.LastIndexByte(source[:offset], '\n') + 1

	end := strings.IndexByte(source[start:], '\n')
	if end < 0 {
		end = len(source) - start
	}

	return source[start : start+end], offset - start
}
