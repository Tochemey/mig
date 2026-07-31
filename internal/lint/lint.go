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

	"github.com/tochemey/mig/internal/lint/lockmodel"
	"github.com/tochemey/mig/internal/lint/rules"
	"github.com/tochemey/mig/internal/plan"
)

// Run lints every migration in the plan. It returns the findings ordered by
// file and position, and the raw contents of each linted file, which is what
// a renderer needs to show the offending line.
func Run(fsys fs.FS, p *plan.Plan, targetVersion int) ([]rules.Finding, map[string]string, error) {
	var findings []rules.Finding

	sources := make(map[string]string, len(p.Migrations))

	for i := range p.Migrations {
		migration := &p.Migrations[i]

		content, err := fs.ReadFile(fsys, migration.File)
		if err != nil {
			return nil, nil, fmt.Errorf("read migration: %w", err)
		}

		sources[migration.File] = string(content)

		found, err := lintMigration(migration, string(content), targetVersion)
		if err != nil {
			return nil, nil, err
		}

		findings = append(findings, found...)
	}

	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}

		if findings[i].Span.Start != findings[j].Span.Start {
			return findings[i].Span.Start < findings[j].Span.Start
		}

		return findings[i].RuleID < findings[j].RuleID
	})

	return findings, sources, nil
}

// lintMigration runs every rule over every statement of one migration.
func lintMigration(migration *plan.Migration, content string, version int) ([]rules.Finding, error) {
	var findings []rules.Finding

	cursor := 0

	for stepIndex, s := range migration.Steps {
		for stmtIndex, statement := range s.Statements {
			span, next := locate(content, cursor, statement.SQL)
			cursor = next

			parsed, err := parseOne(statement.SQL)
			if err != nil {
				return nil, fmt.Errorf("migration %q step %q: %w", migration.File, s.Name, err)
			}

			ctx := rules.Context{
				TargetVersion: version,
				Migration:     migration,
				StepIndex:     stepIndex,
				StmtIndex:     stmtIndex,
				Analysis:      lockmodel.AnalyzeStatement(parsed, version),
			}

			for _, rule := range rules.All() {
				for _, found := range rule.Check(ctx, parsed) {
					found.RuleID = rule.ID()
					found.File = migration.File
					found.Step = s.Name
					found.Span = span

					findings = append(findings, found)
				}
			}
		}
	}

	return findings, nil
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
