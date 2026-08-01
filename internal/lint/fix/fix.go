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

// Package fix builds executable replacements for flagged statements.
//
// A fix is ordered mig-format steps, not prose. Statements are carried as
// parse trees and deparsed at render time, so identifier quoting is the
// server's own. Every step carries a comment saying why it exists: the
// moment the output looks like machine noise, nobody reviews it, and the
// reviewability is the point.
//
// A fix that cannot be completed safely as pure SQL is a scaffold: the same
// steps rendered commented out, under a TODO naming what remains. Replacing
// a working statement with half a fix would be worse than no fix.
package fix

import (
	"fmt"
	"strings"

	pgquery "github.com/pganalyze/pg_query_go/v6"

	"github.com/tochemey/mig/internal/plan"
)

// Backfill marks a step as a batched backfill.
type Backfill struct {
	// Table and Key feed the annotation. The key is assumed, not known:
	// offline nothing says which column is the primary key.
	Table string
	Key   string
	Batch int

	// Satisfied is the predicate deciding whether work remains, which the
	// format requires: the cursor lives in the ledger, and the ledger may
	// not decide that.
	Satisfied *pgquery.Node
}

// Step is one generated migration step.
type Step struct {
	Name     string
	Comment  string
	NoTx     bool
	Backfill *Backfill

	// Satisfied overrides the executor's inferred predicate, as SQL text. A
	// generated pattern needs it when a later step undoes what an earlier
	// step's inferred predicate would look for, which is what dropping a
	// scaffolding constraint does.
	Satisfied string

	// Stmts is the step's SQL. More than one statement means the step is a
	// single transaction doing all of them.
	Stmts []*pgquery.Node
}

// Fix is the ordered replacement for one flagged statement.
type Fix struct {
	Steps []Step

	// Scaffold marks a plan that must not run as written. It renders fully
	// commented out, and TODO says what has to happen first.
	Scaffold bool
	TODO     string
}

// The cursor placeholders of the backfill format. Deparsing renders the
// bound parameters $1 and $2; the format wants these spellings.
const (
	cursorLow  = ":cursor_lo"
	cursorHigh = ":cursor_hi"
)

// Render turns the fix into mig-format annotated SQL.
func (f *Fix) Render() (string, error) {
	out := &strings.Builder{}

	if f.Scaffold {
		_, _ = fmt.Fprintf(out, "-- TODO: %s\n", f.TODO)
		_, _ = out.WriteString("-- The steps below are a plan, not a migration; uncomment them once the\n")
		_, _ = out.WriteString("-- TODO above is resolved.\n--\n")
	}

	for i, step := range f.Steps {
		if i > 0 {
			_, _ = out.WriteString("\n")
		}

		block, err := renderStep(step)
		if err != nil {
			return "", err
		}

		if f.Scaffold {
			block = commentOut(block)
		}

		_, _ = out.WriteString(block)
	}

	return out.String(), nil
}

// renderStep renders one step: annotations, comment, statements. The comment
// sits inside the step body; a comment above the step annotation would open
// an implicit step of its own and refuse to load.
func renderStep(step Step) (string, error) {
	out := &strings.Builder{}

	_, _ = fmt.Fprintf(out, "%s%s %s\n", plan.AnnotationPrefix, plan.AnnotationStep, step.Name)

	if step.NoTx {
		_, _ = fmt.Fprintf(out, "%s%s\n", plan.AnnotationPrefix, plan.AnnotationNoTx)
	}

	if step.Backfill != nil {
		_, _ = fmt.Fprintf(out, "%s%s %s=%s %s=%s %s=%d\n",
			plan.AnnotationPrefix, plan.AnnotationBackfill,
			plan.BackfillTable, step.Backfill.Table,
			plan.BackfillKey, step.Backfill.Key,
			plan.BackfillBatch, step.Backfill.Batch)

		satisfied, err := deparse(step.Backfill.Satisfied)
		if err != nil {
			return "", fmt.Errorf("step %q: render satisfied predicate: %w", step.Name, err)
		}

		_, _ = fmt.Fprintf(out, "%s%s sql(%s)\n",
			plan.AnnotationPrefix, plan.AnnotationSatisfied, satisfied)
	}

	if step.Satisfied != "" {
		_, _ = fmt.Fprintf(out, "%s%s sql(%s)\n",
			plan.AnnotationPrefix, plan.AnnotationSatisfied, step.Satisfied)
	}

	_, _ = fmt.Fprintf(out, "-- %s\n", step.Comment)

	for _, stmt := range step.Stmts {
		sql, err := deparse(stmt)
		if err != nil {
			return "", fmt.Errorf("step %q: render statement: %w", step.Name, err)
		}

		if step.Backfill != nil {
			sql = strings.ReplaceAll(sql, "$1", cursorLow)
			sql = strings.ReplaceAll(sql, "$2", cursorHigh)
		}

		_, _ = fmt.Fprintf(out, "%s;\n", sql)
	}

	return out.String(), nil
}

// commentOut prefixes every line of a rendered block.
func commentOut(block string) string {
	lines := strings.Split(strings.TrimRight(block, "\n"), "\n")

	for i, line := range lines {
		if line == "" {
			lines[i] = "--"
			continue
		}

		lines[i] = "-- " + line
	}

	return strings.Join(lines, "\n") + "\n"
}

// deparse renders one statement through the server's own grammar.
func deparse(stmt *pgquery.Node) (string, error) {
	return pgquery.Deparse(&pgquery.ParseResult{Stmts: []*pgquery.RawStmt{{Stmt: stmt}}})
}
