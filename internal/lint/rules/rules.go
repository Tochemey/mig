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
	"time"

	pgquery "github.com/pganalyze/pg_query_go/v6"

	"github.com/tochemey/mig/internal/lint/fix"
	"github.com/tochemey/mig/internal/lint/lockmodel"
	"github.com/tochemey/mig/internal/lint/stats"
	"github.com/tochemey/mig/internal/parse"
	"github.com/tochemey/mig/internal/plan"
)

// The rule IDs, spelled once here rather than at each rule and each test
// that names one. They are stable and never reused.
const (
	L001 = "L001" // CREATE INDEX without CONCURRENTLY
	L002 = "L002" // CONCURRENTLY inside a transactional step
	L003 = "L003" // ADD COLUMN whose default rewrites the table
	L004 = "L004" // ALTER COLUMN TYPE causing a rewrite
	L005 = "L005" // SET NOT NULL without a proving validated CHECK
	L006 = "L006" // ADD FOREIGN KEY without NOT VALID
	L007 = "L007" // ADD CHECK without NOT VALID
	L008 = "L008" // ADD PRIMARY KEY without USING INDEX
	L009 = "L009" // inline UNIQUE in ADD COLUMN
	L010 = "L010" // VACUUM FULL or CLUSTER in a migration
	L011 = "L011" // REFRESH MATERIALIZED VIEW without CONCURRENTLY
)

// The sizes at which a size-dependent hazard changes grade. The upper pair is
// the design's: a million rows or a gigabyte is where a rewrite stops being a
// deploy and starts being an outage. The lower pair is this package's, and
// exists so that the lookup tables every schema has do not train the reader
// to ignore warnings.
const (
	bigRows    = 1_000_000
	bigBytes   = 1 << 30
	smallRows  = 10_000
	smallBytes = 8 << 20
)

// severityNames maps each severity to its rendered form.
var severityNames = map[Severity]string{
	SeverityInfo:  "info",
	SeverityWarn:  "warn",
	SeverityError: "error",
}

// durations phrases each duration class for a detail line.
var durations = map[lockmodel.DurationClass]string{
	lockmodel.Instant:    "held for catalog work only",
	lockmodel.Scan:       "held for a full scan",
	lockmodel.Rewrite:    "held for a table rewrite",
	lockmodel.IndexBuild: "held for an index build",
}

// The units a size is reported in, largest first.
var (
	rowUnits = []struct {
		limit  int64
		suffix string
	}{{1e9, "B"}, {1e6, "M"}, {1e3, "k"}}

	byteUnits = []struct {
		limit  int64
		suffix string
	}{{1 << 40, "TB"}, {1 << 30, "GB"}, {1 << 20, "MB"}, {1 << 10, "kB"}}
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

	// Detail carries the lock mode and duration behind the message, with the
	// table's size when the catalog was there to say.
	Detail string `json:"detail,omitempty"`

	// Estimate is how long the work is likely to hold that lock, empty
	// offline and whenever the server was not measured.
	Estimate string `json:"estimate,omitempty"`

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

	// Stats is what the catalog said about the relations this plan names,
	// and is nil offline. A hazard whose cost is the table's size is graded
	// by it: without it, every such hazard stays a warning, because a linter
	// that calls a 12-row lookup table an outage gets muted.
	Stats *stats.Snapshot
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

// describe renders the analysis's strongest effect for a finding's detail
// line: the mode, the relation and its size, how long the lock is held, and
// what that blocks.
func describe(ctx Context) string {
	effects := ctx.Analysis.Effects
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

	switch profile := ctx.Analysis.Blocks(); {
	case profile.Reads:
		blocks = "blocks reads and writes"
	case profile.Writes:
		blocks = "blocks writes"
	}

	return fmt.Sprintf("%s on %s%s, %s; %s",
		strongest.Mode, strongest.Relation, size(ctx, strongest.Relation),
		durations[strongest.Duration], blocks)
}

// size renders what the catalog said a relation holds, in parentheses, and
// nothing at all offline or for a relation the catalog does not have.
func size(ctx Context, relation lockmodel.Relation) string {
	table := ctx.Stats.Table(relation)
	if !table.Exists {
		return ""
	}

	if table.Rows == 0 {
		// A table nobody has analysed has no row estimate, and its size on
		// disk is the only honest thing to report.
		return fmt.Sprintf(" (%s)", bytesOf(table.Bytes))
	}

	return fmt.Sprintf(" (%s rows, %s)", rowsOf(table.Rows), bytesOf(table.Bytes))
}

// rowsOf renders a row estimate the way a reader says it out loud.
func rowsOf(rows int64) string {
	for _, unit := range rowUnits {
		if rows >= unit.limit {
			return fmt.Sprintf("%.1f%s", float64(rows)/float64(unit.limit), unit.suffix)
		}
	}

	return fmt.Sprint(rows)
}

// bytesOf renders a size on disk.
func bytesOf(bytes int64) string {
	for _, unit := range byteUnits {
		if bytes >= unit.limit {
			return fmt.Sprintf("%.1f %s", float64(bytes)/float64(unit.limit), unit.suffix)
		}
	}

	return fmt.Sprintf("%d B", bytes)
}

// finding builds the rule-owned half of a finding.
func finding(severity Severity, message string, ctx Context) []Finding {
	return []Finding{{
		Severity: severity,
		Message:  message,
		Detail:   describe(ctx),
	}}
}

// sized reports a hazard whose cost is the size of the table it names.
//
// Offline it is a warning, because nothing here knows whether the table holds
// twelve rows or forty million. Connected, the catalog decides: an error on a
// table big enough for the hazard to be an outage, an observation on one too
// small for it to matter, and a warning in between or when the table is not
// there to ask about.
func sized(ctx Context, schema, name, message string) []Finding {
	relation := lockmodel.Relation{Schema: schema, Name: name}
	found := finding(grade(ctx, relation), message, ctx)
	found[0].Estimate = estimate(ctx, relation)

	return found
}

// pagingKey names the column a generated backfill should page by: the
// table's primary key when the catalog was there to say and the key is one
// column, and nothing otherwise, which leaves the fix to assume and say so.
//
// A composite key is left alone deliberately. The format pages by a single
// column, and paging by the first column of a composite key gives batches
// whose size nobody can predict.
func pagingKey(ctx Context, schema, name string) string {
	key := ctx.Stats.Table(lockmodel.Relation{Schema: schema, Name: name}).PrimaryKey
	if len(key) != 1 {
		return ""
	}

	return key[0]
}

// grade picks the severity the table's size calls for.
func grade(ctx Context, relation lockmodel.Relation) Severity {
	table := ctx.Stats.Table(relation)
	if !table.Exists {
		return SeverityWarn
	}

	switch {
	case table.Rows >= bigRows || table.Bytes >= bigBytes:
		return SeverityError
	case table.Rows < smallRows && table.Bytes < smallBytes:
		return SeverityInfo
	default:
		return SeverityWarn
	}
}

// estimate renders how long the statement is likely to hold its lock on the
// relation, empty unless the server was measured and the work is the kind
// whose cost is the table's size.
func estimate(ctx Context, relation lockmodel.Relation) string {
	table := ctx.Stats.Table(relation)
	if !table.Exists {
		return ""
	}

	low, high, ok := ctx.Stats.Throughput().
		Estimate(durationFor(ctx.Analysis, relation), table.Rows, table.Bytes)

	// Work that is over before anyone notices does not need an estimate, and
	// "estimated 0s to 0s" is the kind of line that teaches a reader to skip
	// the rest of them.
	if !ok || high < time.Second {
		return ""
	}

	return fmt.Sprintf("estimated %s to %s, from this server's measured throughput; "+
		"a cold or busy table is slower", round(low), round(high))
}

// durationFor picks the analysis's heaviest duration for one relation, and
// catalog work when the analysis says nothing about it: work nobody predicted
// is not work anybody should be handed a number for.
func durationFor(analysis lockmodel.Analysis, relation lockmodel.Relation) lockmodel.DurationClass {
	heaviest := lockmodel.Instant

	for _, effect := range analysis.Effects {
		if effect.Relation == relation && effect.Duration > heaviest {
			heaviest = effect.Duration
		}
	}

	return heaviest
}

// round trims an estimate to a figure a reader can act on. Sub-second
// precision on a number that carries a factor of three in it is noise.
func round(d time.Duration) time.Duration {
	switch {
	case d >= time.Hour:
		return d.Round(time.Minute)
	case d >= time.Minute:
		return d.Round(time.Second)
	default:
		return d.Round(100 * time.Millisecond)
	}
}

// withFix renders the replacement onto findings.
//
// Rendering fails only on a tree the builders never produce, and the empty
// text it returns then is already the answer that failure calls for: the
// finding stands, without a fix. The error itself is the fix package's to
// test, and its tests do.
func withFix(findings []Finding, f *fix.Fix) []Finding {
	text, _ := f.Render()

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
