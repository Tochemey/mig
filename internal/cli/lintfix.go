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

package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tochemey/mig/internal/plan"
	"github.com/tochemey/mig/pkg/mig"
)

// The answers --fix takes for yes. Anything else leaves the files alone.
const (
	answerY   = "y"
	answerYes = "yes"
)

// edit is one replacement inside a file, by byte range.
type edit struct {
	start, end int
	text       string
}

// applyFixes rewrites the flagged statements that carry a fix. Nothing is
// written before the whole diff has been shown and confirmed.
func applyFixes(dir string, linted *mig.LintReport, yes bool, stdout io.Writer, stdin io.Reader) error {
	edits := planEdits(linted)
	if len(edits) == 0 {
		if _, err := io.WriteString(stdout, "no applicable fixes\n"); err != nil {
			return fmt.Errorf("write report: %w", err)
		}

		return nil
	}

	files := make([]string, 0, len(edits))
	for file := range edits {
		files = append(files, file)
	}

	sort.Strings(files)

	rewritten := make(map[string]string, len(files))
	diff := &strings.Builder{}
	total := 0

	for _, file := range files {
		source := linted.Sources[file]
		rewritten[file] = splice(source, edits[file])
		total += len(edits[file])

		printDiff(diff, file, source, edits[file])
	}

	if _, err := io.WriteString(stdout, diff.String()); err != nil {
		return fmt.Errorf("write diff: %w", err)
	}

	if !yes {
		if _, err := fmt.Fprintf(stdout,
			"apply %d fix(es) to %d file(s)? [y/N]: ", total, len(files)); err != nil {
			return fmt.Errorf("write prompt: %w", err)
		}

		if !confirmed(stdin) {
			if _, err := io.WriteString(stdout, "nothing written\n"); err != nil {
				return fmt.Errorf("write report: %w", err)
			}

			return nil
		}
	}

	for _, file := range files {
		path := filepath.Join(dir, file)

		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("stat %q: %w", path, err)
		}

		if err := os.WriteFile(path, []byte(rewritten[file]), info.Mode().Perm()); err != nil {
			return fmt.Errorf("write %q: %w", path, err)
		}
	}

	if _, err := fmt.Fprintf(stdout,
		"applied %d fix(es) to %d file(s)\n", total, len(files)); err != nil {
		return fmt.Errorf("write report: %w", err)
	}

	return nil
}

// planEdits turns findings into byte-range replacements, one list per file,
// ordered by position.
func planEdits(linted *mig.LintReport) map[string][]edit {
	edits := make(map[string][]edit)
	claimed := make(map[string]map[int]bool)

	for _, finding := range linted.Findings {
		if finding.Fix == "" || finding.Span.Line == 0 {
			continue
		}

		source := linted.Sources[finding.File]

		if claimed[finding.File] == nil {
			claimed[finding.File] = make(map[int]bool)
		}

		// One replacement per statement: a second finding on the same spot
		// keeps its say in the report, not in the file.
		if claimed[finding.File][finding.Span.Start] {
			continue
		}

		claimed[finding.File][finding.Span.Start] = true

		// The loader attaches leading comments to the statement, so the span
		// of an implicit step covers the file's banner. An annotated step is
		// claimed from its annotation down, comments and all: they describe
		// the statement being replaced. An implicit step starts after any
		// leading comments, so the banner survives.
		start, annotated := stepStart(source, finding.Span.Start)
		if !annotated {
			start = afterLeadingComments(source, finding.Span.Start)
		}

		text := strings.TrimRight(finding.Fix, "\n") + "\n"

		if finding.FixScaffold {
			// A scaffold keeps the statement it comments on: the plan goes
			// above the step, and nothing working is deleted.
			edits[finding.File] = append(edits[finding.File],
				edit{start: start, end: start, text: text + "\n"})
			continue
		}

		end := pastSemicolon(source, finding.Span.End)

		edits[finding.File] = append(edits[finding.File],
			edit{start: start, end: end, text: text})
	}

	for file := range edits {
		sort.Slice(edits[file], func(i, j int) bool {
			return edits[file][i].start < edits[file][j].start
		})
	}

	return edits
}

// stepStart walks back from a statement to its "-- +mig step:" line and
// reports whether it found one. The walk crosses comments, annotations and
// blank lines, so a blank between the annotation and its statement does not
// orphan the step header; it stops at anything else, so it never claims a
// previous step's statements.
func stepStart(source string, offset int) (int, bool) {
	start := strings.LastIndexByte(source[:offset], '\n') + 1
	line := start

	for line > 0 {
		previous := strings.LastIndexByte(source[:line-1], '\n') + 1

		text := strings.TrimSpace(source[previous : line-1])
		if text != "" && !strings.HasPrefix(text, "--") {
			break
		}

		line = previous

		if isStepLine(text) {
			return line, true
		}
	}

	return start, false
}

// afterLeadingComments advances over the whole-line comments and blanks a
// span opens with, which is where a file banner sits when the loader has
// attached it to the statement of an implicit step.
func afterLeadingComments(source string, offset int) int {
	for offset < len(source) {
		end := strings.IndexByte(source[offset:], '\n')
		if end < 0 {
			break
		}

		text := strings.TrimSpace(source[offset : offset+end])
		if text != "" && !strings.HasPrefix(text, "--") {
			break
		}

		offset += end + 1
	}

	return offset
}

// isStepLine recognises a step annotation the way the loader does, so a
// spacing variation the loader accepts is not misread as a plain comment.
func isStepLine(text string) bool {
	body, ok := strings.CutPrefix(text, strings.TrimSpace(plan.AnnotationPrefix))
	if !ok {
		return false
	}

	return strings.HasPrefix(strings.TrimSpace(body), plan.AnnotationStep)
}

// pastSemicolon extends a statement span over its terminator, which the
// parser's split leaves behind.
func pastSemicolon(source string, offset int) int {
	for offset < len(source) {
		switch source[offset] {
		case ' ', '\t', '\n':
			offset++
			continue
		case ';':
			return offset + 1
		}

		break
	}

	return offset
}

// splice applies ordered, non-overlapping edits from the back, so earlier
// offsets stay valid.
func splice(source string, edits []edit) string {
	out := source

	for i := len(edits) - 1; i >= 0; i-- {
		out = out[:edits[i].start] + edits[i].text + out[edits[i].end:]
	}

	return out
}

// printDiff renders what an edit removes and adds, line by line. The sink is
// a builder, whose writes cannot fail; the caller flushes it once, checked.
func printDiff(out *strings.Builder, file, source string, edits []edit) {
	_, _ = fmt.Fprintf(out, "--- %s\n", file)

	for _, e := range edits {
		for line := range strings.SplitSeq(strings.TrimRight(source[e.start:e.end], "\n"), "\n") {
			if e.start != e.end {
				_, _ = fmt.Fprintf(out, "- %s\n", line)
			}
		}

		for line := range strings.SplitSeq(strings.TrimRight(e.text, "\n"), "\n") {
			_, _ = fmt.Fprintf(out, "+ %s\n", line)
		}
	}
}

// confirmed reads one line and accepts an explicit yes.
func confirmed(stdin io.Reader) bool {
	scanner := bufio.NewScanner(stdin)
	if !scanner.Scan() {
		return false
	}

	answer := strings.ToLower(strings.TrimSpace(scanner.Text()))

	return answer == answerY || answer == answerYes
}
