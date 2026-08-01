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

// Package lint runs the rule catalog over a loaded plan.
//
// It reuses the executor's parsing rather than its own: a migration is linted
// exactly as it will be executed, step by step, statement by statement. Each
// statement is parsed once, analysed by the lock model once, and offered to
// every rule with the whole migration in reach.
package lint

import (
	"fmt"
	"io/fs"
	"sort"
	"strings"

	pgquery "github.com/pganalyze/pg_query_go/v6"

	"github.com/tochemey/mig/internal/lint/history"
	"github.com/tochemey/mig/internal/lint/lockmodel"
	"github.com/tochemey/mig/internal/lint/policy"
	"github.com/tochemey/mig/internal/lint/rules"
	"github.com/tochemey/mig/internal/lint/stats"
	"github.com/tochemey/mig/internal/plan"
)

// Relations lists every relation the plan's statements touch, the implicit
// ones included, which is what connected mode reads the catalog about before
// any rule runs.
func Relations(p *plan.Plan, targetVersion int) ([]lockmodel.Relation, error) {
	var relations []lockmodel.Relation

	seen := make(map[lockmodel.Relation]bool)

	for i := range p.Migrations {
		migration := &p.Migrations[i]

		for _, s := range migration.Steps {
			for _, statement := range s.Statements {
				parsed, err := parseOne(statement.SQL)
				if err != nil {
					return nil, fmt.Errorf("migration %q step %q: %w", migration.File, s.Name, err)
				}

				for _, effect := range lockmodel.AnalyzeStatement(parsed, targetVersion).Effects {
					if !seen[effect.Relation] {
						seen[effect.Relation] = true
						relations = append(relations, effect.Relation)
					}
				}
			}
		}
	}

	return relations, nil
}

// Result is what linting a plan produces.
type Result struct {
	// Findings is ordered by file, position and rule.
	Findings []rules.Finding

	// Sources maps each linted file to its contents, which is what a renderer
	// needs to show the offending line.
	Sources map[string]string

	// Suppressions is every lint directive the files carry, in the order they
	// appear, each marked with whether it silenced anything.
	Suppressions []policy.Directive
}

// Run lints every migration in the plan.
//
// The snapshot is what the catalog said about the relations, and is nil
// offline: without it every size-dependent hazard stays a warning. The policy
// is what the team decided about the catalog, and may be nil.
func Run(fsys fs.FS, p *plan.Plan, targetVersion int, snapshot *stats.Snapshot,
	pol *policy.Policy) (*Result, error) {
	result := &Result{Sources: make(map[string]string, len(p.Migrations))}
	past := history.Build(p)

	for i := range p.Migrations {
		migration := &p.Migrations[i]

		content, err := fs.ReadFile(fsys, migration.File)
		if err != nil {
			return nil, fmt.Errorf("read migration: %w", err)
		}

		source := string(content)

		result.Sources[migration.File] = source
		result.Suppressions = append(result.Suppressions, policy.Scan(migration, source)...)

		found, err := lintMigration(migration, source, targetVersion, pol.Thresholds(), snapshot, past)
		if err != nil {
			return nil, err
		}

		result.Findings = append(result.Findings, found...)
	}

	result.Findings = suppress(pol.Apply(result.Findings), result.Suppressions)

	// A directive the linter cannot honour is a finding of its own, and is
	// added after the suppressions have run: a broken suppression that could
	// silence the complaint about itself would never be fixed.
	result.Findings = append(result.Findings, problems(result.Suppressions)...)

	sortFindings(result.Findings)

	return result, nil
}

// suppress drops the findings the directives silence, and marks the
// directives that silenced something.
func suppress(findings []rules.Finding, directives []policy.Directive) []rules.Finding {
	if len(directives) == 0 {
		return findings
	}

	kept := make([]rules.Finding, 0, len(findings))

	for _, finding := range findings {
		silenced := false

		// Every directive is offered the finding rather than the first match
		// taken, so that two covering one finding are both counted as used
		// and neither is reported as dead weight it is not.
		for i := range directives {
			if directives[i].Silences(finding) {
				directives[i].Used = true
				silenced = true
			}
		}

		if !silenced {
			kept = append(kept, finding)
		}
	}

	return kept
}

// problems reports the directives the linter cannot honour: the ones naming
// no rule, naming one this build does not have, or giving no reason.
func problems(directives []policy.Directive) []rules.Finding {
	var findings []rules.Finding

	for _, directive := range directives {
		if directive.Problem == "" {
			continue
		}

		findings = append(findings, rules.Finding{
			RuleID:   rules.L000,
			Severity: rules.SeverityError,
			Message:  fmt.Sprintf("this lint:ignore directive %s", directive.Problem),
			File:     directive.File,
			Step:     directive.Step,
			Span:     directive.Span,
		})
	}

	return findings
}

// sortFindings orders findings by file, position and rule.
func sortFindings(findings []rules.Finding) {
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}

		if findings[i].Span.Start != findings[j].Span.Start {
			return findings[i].Span.Start < findings[j].Span.Start
		}

		return findings[i].RuleID < findings[j].RuleID
	})
}

// lintMigration runs every rule over every statement of one migration.
func lintMigration(migration *plan.Migration, content string, version int,
	thresholds rules.Thresholds, snapshot *stats.Snapshot, past *history.History) ([]rules.Finding, error) {
	var findings []rules.Finding

	cursor := 0

	for stepIndex, s := range migration.Steps {
		// The whole step is parsed and analysed before any rule sees any of
		// it: a transactional step holds its locks to the commit, so the
		// cross-statement rules need its statements together.
		whole, err := analyseStep(s, version)
		if err != nil {
			return nil, fmt.Errorf("migration %q step %q: %w", migration.File, s.Name, err)
		}

		for stmtIndex, statement := range s.Statements {
			span, next := locate(content, cursor, statement.SQL)
			cursor = next

			ctx := rules.Context{
				TargetVersion: version,
				Migration:     migration,
				StepIndex:     stepIndex,
				StmtIndex:     stmtIndex,
				Analysis:      whole.Analysed[stmtIndex],
				Step:          whole,
				Thresholds:    thresholds,
				Stats:         snapshot,
				History:       past,
			}

			parsed := whole.Parsed[stmtIndex]

			for _, rule := range rules.All() {
				for _, found := range rule.Check(ctx, parsed) {
					found.RuleID = rule.ID()
					found.File = migration.File
					found.Step = s.Name
					found.Span = span

					// A fix replaces its whole step. A statement sharing a
					// step keeps the finding and loses the fix; splitting
					// the step is the author's call.
					if len(s.Statements) != 1 {
						found.Fix = ""
					}

					findings = append(findings, found)
				}
			}
		}
	}

	return findings, nil
}

// analyseStep parses and analyses every statement of one step, once.
func analyseStep(s plan.Step, version int) (rules.Step, error) {
	whole := rules.Step{
		Spec:     s,
		Parsed:   make([]*pgquery.RawStmt, len(s.Statements)),
		Analysed: make([]lockmodel.Analysis, len(s.Statements)),
	}

	for i, statement := range s.Statements {
		parsed, err := parseOne(statement.SQL)
		if err != nil {
			return rules.Step{}, err
		}

		whole.Parsed[i] = parsed
		whole.Analysed[i] = lockmodel.AnalyzeStatement(parsed, version)
	}

	return whole, nil
}

// parseOne re-parses a statement the plan already split, for the tree the
// rules and the lock model read. The plan parsed it once before, so a failure
// here means the caller built the plan by hand.
func parseOne(sql string) (*pgquery.RawStmt, error) {
	tree, err := pgquery.Parse(sql)
	if err != nil {
		return nil, fmt.Errorf("parse statement: %w", err)
	}

	if len(tree.Stmts) != 1 {
		return nil, fmt.Errorf("expected one statement, got %d", len(tree.Stmts))
	}

	return tree.Stmts[0], nil
}

// locate finds the statement's span in the file, scanning forward from the
// previous statement so a repeated statement lands on its own occurrence. A
// statement whose text was rewritten after loading, which is what a
// backfill's cursor placeholders go through, gets a zero span.
func locate(content string, cursor int, sql string) (rules.Span, int) {
	offset := strings.Index(content[cursor:], sql)
	if offset < 0 {
		return rules.Span{}, cursor
	}

	start := cursor + offset
	end := start + len(sql)
	line := 1 + strings.Count(content[:start], "\n")

	return rules.Span{Start: start, End: end, Line: line}, end
}
