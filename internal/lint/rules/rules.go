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

// Package rules holds the lint rule catalog, one file per rule.
//
// A rule inspects one parsed statement, with the whole migration and the lock
// model's analysis in reach, and reports findings. Rule IDs are stable and
// never reused. A finding's message says what will happen in production; the
// detail line carries the lock mode and duration from the model.
//
// The credibility rule: a hazard whose cost is O(rows) is not reported
// against a table this same migration creates, because that table is empty
// when the statement runs. A linter that cries wolf gets disabled, and then
// it protects nothing.
package rules

import (
	"fmt"

	pgquery "github.com/pganalyze/pg_query_go/v6"

	"github.com/tochemey/mig/internal/lint/fix"
	"github.com/tochemey/mig/internal/lint/lockmodel"
	"github.com/tochemey/mig/internal/parse"
	"github.com/tochemey/mig/internal/plan"
)

// Severity grades a finding.
type Severity int

const (
	// SeverityInfo marks an observation worth knowing, not worth failing.
	SeverityInfo Severity = iota + 1

	// SeverityWarn marks a hazard whose cost depends on the table. Offline,
	// where the size is unknown, size-dependent hazards stay warnings.
	SeverityWarn

	// SeverityError marks a hazard that is wrong regardless of scale.
	SeverityError
)

// severityNames maps each severity to its rendered form.
var severityNames = map[Severity]string{
	SeverityInfo:  "info",
	SeverityWarn:  "warn",
	SeverityError: "error",
}

// String renders the severity.
func (s Severity) String() string {
	if name, ok := severityNames[s]; ok {
		return name
	}

	return "unknown"
}

// MarshalJSON renders the severity as its name rather than a number, so the
// JSON output and the golden fixtures read without a decoder ring.
func (s Severity) MarshalJSON() ([]byte, error) {
	return []byte(`"` + s.String() + `"`), nil
}

// Span is where a finding sits in its source file.
type Span struct {
	// Start and End are byte offsets into the file.
	Start int `json:"start"`
	End   int `json:"end"`

	// Line is 1-based, and 0 when the statement could not be located, which
	// is what a backfill's rewritten cursor placeholders produce.
	Line int `json:"line"`
}

// Finding is one hazard a rule reports. The rule fills severity, message and
// detail; the engine stamps the rest.
type Finding struct {
	RuleID   string   `json:"rule"`
	Severity Severity `json:"severity"`

	// Message says what will happen and what to do instead.
	Message string `json:"message"`

	// Detail carries the lock mode and duration behind the message.
	Detail string `json:"detail,omitempty"`

	// File and Step locate the finding for a reader; Span locates it for a
	// renderer.
	File string `json:"file"`
	Step string `json:"step"`
	Span Span   `json:"span"`

	// Fix is the replacement in mig-format annotated SQL, empty when no safe
	// rewrite exists. FixScaffold marks a plan rather than a migration: the
	// text is fully commented out, is inserted above the statement instead
	// of replacing it, and must be completed by hand.
	Fix         string `json:"fix,omitempty"`
	FixScaffold bool   `json:"fix_scaffold,omitempty"`
}

// Context is what a rule sees beyond the statement itself.
type Context struct {
	// TargetVersion is the Postgres major the migration is written for.
	TargetVersion int

	// Migration is the whole file, for a rule that needs to know what came
	// before its statement.
	Migration *plan.Migration

	// StepIndex says which step the statement belongs to, and StmtIndex
	// where it sits within that step.
	StepIndex int
	StmtIndex int

	// Analysis is the lock model's prediction for the statement.
	Analysis lockmodel.Analysis
}

// Rule inspects one statement.
type Rule interface {
	ID() string
	Check(ctx Context, stmt *pgquery.RawStmt) []Finding
}

// All returns the catalog in rule order.
func All() []Rule {
	return []Rule{
		l001{}, l002{}, l003{}, l004{}, l005{}, l006{},
		l007{}, l008{}, l009{}, l010{}, l011{},
	}
}

// createdHere reports whether the migration creates the relation before the
// statement under inspection runs. Such a table is empty at that point, so
// scans, rewrites and index builds over it cost nothing. A creation after
// the statement proves nothing: there the statement either fails, or the
// migration is recreating a table that holds rows right now.
func (c Context) createdHere(schema, name string) bool {
	for stepIndex, s := range c.Migration.Steps {
		if stepIndex > c.StepIndex {
			break
		}

		statements := s.Statements
		if stepIndex == c.StepIndex {
			statements = statements[:c.StmtIndex]
		}

		for _, statement := range statements {
			if statement.Kind != parse.KindCreateTable {
				continue
			}

			target := statement.Target
			if target.Name != name {
				continue
			}

			if schema == "" || target.Schema == "" || schema == target.Schema {
				return true
			}
		}
	}

	return false
}

// durations phrases each duration class for a detail line.
var durations = map[lockmodel.DurationClass]string{
	lockmodel.Instant:    "held for catalog work only",
	lockmodel.Scan:       "held for a full scan",
	lockmodel.Rewrite:    "held for a table rewrite",
	lockmodel.IndexBuild: "held for an index build",
}

// describe renders the analysis's strongest effect for a finding's detail
// line: the mode, the relation, how long it is held, and what that blocks.
func describe(analysis lockmodel.Analysis) string {
	effects := analysis.Effects
	if len(effects) == 0 {
		return ""
	}

	strongest := effects[0]

	for _, effect := range effects[1:] {
		if effect.Mode > strongest.Mode ||
			(effect.Mode == strongest.Mode && effect.Duration > strongest.Duration) {
			strongest = effect
		}
	}

	blocks := "blocks no traffic"

	switch profile := analysis.Blocks(); {
	case profile.Reads:
		blocks = "blocks reads and writes"
	case profile.Writes:
		blocks = "blocks writes"
	}

	return fmt.Sprintf("%s on %s, %s; %s",
		strongest.Mode, strongest.Relation, durations[strongest.Duration], blocks)
}

// finding builds the rule-owned half of a finding.
func finding(severity Severity, message string, ctx Context) []Finding {
	return []Finding{{
		Severity: severity,
		Message:  message,
		Detail:   describe(ctx.Analysis),
	}}
}

// withFix renders the replacement onto findings. A fix that fails to render
// keeps the finding and drops the fix: the hazard stands either way, and a
// rendering failure is a programming error the fix package's tests catch.
func withFix(findings []Finding, f *fix.Fix) []Finding {
	text, err := f.Render()
	if err != nil {
		return findings
	}

	for i := range findings {
		findings[i].Fix = text
		findings[i].FixScaffold = f.Scaffold
	}

	return findings
}

// alterCommands unwraps an ALTER TABLE statement into its actions, which is
// where most of the catalog starts.
func alterCommands(stmt *pgquery.RawStmt) (*pgquery.AlterTableStmt, []*pgquery.AlterTableCmd) {
	alter := stmt.GetStmt().GetAlterTableStmt()
	if alter == nil {
		return nil, nil
	}

	cmds := make([]*pgquery.AlterTableCmd, 0, len(alter.GetCmds()))

	for _, node := range alter.GetCmds() {
		cmds = append(cmds, node.GetAlterTableCmd())
	}

	return alter, cmds
}

// tableOf names the relation an ALTER TABLE statement works on.
func tableOf(alter *pgquery.AlterTableStmt) (schema, name string) {
	relation := alter.GetRelation()

	return relation.GetSchemaname(), relation.GetRelname()
}

// qualified renders a possibly schema-qualified name for a message.
func qualified(schema, name string) string {
	if schema == "" {
		return name
	}

	return schema + "." + name
}
