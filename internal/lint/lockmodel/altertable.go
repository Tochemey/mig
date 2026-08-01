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

package lockmodel

import (
	"slices"

	pgquery "github.com/pganalyze/pg_query_go/v6"
)

// serialTypes are the type names that mean "a sequence fills this column",
// which is what makes ADD COLUMN of one a rewrite.
var serialTypes = map[string]bool{
	"serial":      true,
	"bigserial":   true,
	"smallserial": true,
	"serial2":     true,
	"serial4":     true,
	"serial8":     true,
}

// nonVolatileFuncs are stable builtins the fast default path accepts,
// evaluated once and stored. Only names that reach the walk as a function
// call belong here; CURRENT_TIMESTAMP and its keyword siblings arrive as
// SQLValueFunction nodes instead. Any function not listed is assumed
// volatile, which errs towards predicting a rewrite.
var nonVolatileFuncs = map[string]bool{
	"now":                   true,
	"transaction_timestamp": true,
	"statement_timestamp":   true,
	"current_setting":       true,
	"current_schema":        true,
	"current_database":      true,
}

// alterTable maps each ALTER TABLE action to its effects. A statement may
// carry several actions; each contributes its own effect on the table, so a
// caller reads the strongest of them as the statement's cost.
//
// An action the model does not recognise is reported as ACCESS EXCLUSIVE held
// for catalog work, which is what nearly every remaining action costs. The
// mode errs strong and the duration errs short; a rule that needs better for
// a specific action adds it here, where test/lockmatrix holds it honest.
func alterTable(stmt *pgquery.AlterTableStmt, version int) Analysis {
	table := relationOf(stmt.GetRelation())
	analysis := Analysis{}

	for _, node := range stmt.GetCmds() {
		cmd := node.GetAlterTableCmd()
		effects, noTx := alterAction(table, cmd, version)
		analysis.Effects = append(analysis.Effects, effects...)
		analysis.NoTx = analysis.NoTx || noTx
	}

	return analysis
}

// alterAction maps one ALTER TABLE action.
func alterAction(table Relation, cmd *pgquery.AlterTableCmd, version int) ([]LockEffect, bool) {
	on := func(mode LockMode, duration DurationClass, reason string) []LockEffect {
		return []LockEffect{{Relation: table, Mode: mode, Duration: duration, Reason: reason}}
	}

	switch cmd.GetSubtype() {
	case pgquery.AlterTableType_AT_AddColumn:
		return addColumn(table, cmd.GetDef().GetColumnDef(), version), false

	case pgquery.AlterTableType_AT_DropColumn:
		return on(AccessExclusive, Instant, "catalog only: drop column"), false

	case pgquery.AlterTableType_AT_ColumnDefault:
		return on(AccessExclusive, Instant, "catalog only: change column default"), false

	case pgquery.AlterTableType_AT_DropNotNull:
		return on(AccessExclusive, Instant, "catalog only: drop not null"), false

	case pgquery.AlterTableType_AT_SetNotNull:
		return on(AccessExclusive, Scan, "full scan verifies no null exists"), false

	case pgquery.AlterTableType_AT_AlterColumnType:
		return on(AccessExclusive, Rewrite, "table rewrite: column type change"), false

	case pgquery.AlterTableType_AT_AddConstraint:
		return addConstraint(table, cmd.GetDef().GetConstraint()), false

	case pgquery.AlterTableType_AT_ValidateConstraint:
		return on(ShareUpdateExclusive, Scan, "validation scans without blocking traffic"), false

	case pgquery.AlterTableType_AT_DropConstraint:
		return on(AccessExclusive, Instant, "catalog only: drop constraint"), false

	case pgquery.AlterTableType_AT_SetStatistics,
		pgquery.AlterTableType_AT_SetOptions,
		pgquery.AlterTableType_AT_ResetOptions:
		return on(ShareUpdateExclusive, Instant, "catalog only: per-column setting"), false

	case pgquery.AlterTableType_AT_SetRelOptions,
		pgquery.AlterTableType_AT_ResetRelOptions:
		return on(ShareUpdateExclusive, Instant, "catalog only: storage parameter"), false

	case pgquery.AlterTableType_AT_AttachPartition:
		return attachPartition(table, cmd.GetDef().GetPartitionCmd(), version), false

	case pgquery.AlterTableType_AT_DetachPartition:
		return detachPartition(table, cmd.GetDef().GetPartitionCmd())

	default:
		return on(AccessExclusive, Instant, "alter table action, assumed catalog only"), false
	}
}

// AnalyzeAddColumn classifies one ADD COLUMN action in isolation. A
// multi-action ALTER TABLE mixes every action's effects in one analysis; a
// caller attributing a cost to the right action asks per column.
func AnalyzeAddColumn(table Relation, column *pgquery.ColumnDef, version int) LockEffect {
	return addColumn(table, column, version)[0]
}

// addColumn classifies ADD COLUMN by what fills the new column. A column
// backed by nothing, or by a default the server can store as a missing value,
// is catalog work; one backed by a volatile default, a sequence or a stored
// expression rewrites the table.
func addColumn(table Relation, column *pgquery.ColumnDef, version int) []LockEffect {
	effect := LockEffect{
		Relation: table,
		Mode:     AccessExclusive,
		Duration: Instant,
		Reason:   "catalog only: add column",
	}

	if serialType(column.GetTypeName()) {
		effect.Duration = Rewrite
		effect.Reason = "table rewrite: serial column backfills from a sequence"
	}

	for _, node := range column.GetConstraints() {
		constraint := node.GetConstraint()

		switch constraint.GetContype() {
		case pgquery.ConstrType_CONSTR_DEFAULT:
			switch {
			case volatileExpr(constraint.GetRawExpr()):
				effect.Duration = Rewrite
				effect.Reason = "table rewrite: volatile default evaluated per row"

			case version < 11:
				effect.Duration = Rewrite
				effect.Reason = "table rewrite: add column with default before Postgres 11"

			case effect.Duration == Instant:
				// Only claim catalog-only work when nothing else on the
				// column, such as an inline UNIQUE, already costs more.
				effect.Reason = "catalog only: non-volatile default stored as a missing value"
			}

		case pgquery.ConstrType_CONSTR_IDENTITY:
			effect.Duration = Rewrite
			effect.Reason = "table rewrite: identity column backfills from a sequence"

		case pgquery.ConstrType_CONSTR_GENERATED:
			effect.Duration = Rewrite
			effect.Reason = "table rewrite: stored generated column"

		case pgquery.ConstrType_CONSTR_UNIQUE, pgquery.ConstrType_CONSTR_PRIMARY:
			if effect.Duration != Rewrite {
				effect.Duration = IndexBuild
				effect.Reason = "index build under the table lock"
			}
		}
	}

	return []LockEffect{effect}
}

// serialType reports the serial pseudo-types, which expand to a sequence
// default and backfill every existing row.
func serialType(name *pgquery.TypeName) bool {
	last := ""

	for _, part := range name.GetNames() {
		last = part.GetString_().GetSval()
	}

	return serialTypes[last]
}

// addConstraint classifies ADD CONSTRAINT by constraint type. NOT VALID
// defers the scan; USING INDEX skips the build.
func addConstraint(table Relation, constraint *pgquery.Constraint) []LockEffect {
	on := func(mode LockMode, duration DurationClass, reason string) []LockEffect {
		return []LockEffect{{Relation: table, Mode: mode, Duration: duration, Reason: reason}}
	}

	switch constraint.GetContype() {
	case pgquery.ConstrType_CONSTR_FOREIGN:
		duration, reason := Scan, "validation scans the table"
		if constraint.GetSkipValidation() {
			duration, reason = Instant, "not valid: validation deferred"
		}

		return []LockEffect{
			{Relation: table, Mode: ShareRowExclusive, Duration: duration,
				Reason: "foreign key locks both tables; " + reason},
			{Relation: relationOf(constraint.GetPktable()), Mode: ShareRowExclusive,
				Duration: duration, Implicit: true,
				Reason: "foreign key locks the referenced table"},
		}

	case pgquery.ConstrType_CONSTR_CHECK:
		if constraint.GetSkipValidation() {
			return on(AccessExclusive, Instant, "not valid: validation deferred")
		}

		return on(AccessExclusive, Scan, "validation scans the table")

	case pgquery.ConstrType_CONSTR_PRIMARY, pgquery.ConstrType_CONSTR_UNIQUE:
		if constraint.GetIndexname() != "" {
			return on(AccessExclusive, Instant, "adopts an existing index")
		}

		return on(AccessExclusive, IndexBuild, "index build under the table lock")

	default:
		// Exclusion constraints land here, and anything the model has not
		// met errs strong and long rather than silent.
		return on(AccessExclusive, IndexBuild, "index build under the table lock")
	}
}

// attachPartition covers ATTACH PARTITION, which validates the incoming
// partition against its bound. Postgres 12 lowered the parent's lock to
// SHARE UPDATE EXCLUSIVE.
func attachPartition(parent Relation, cmd *pgquery.PartitionCmd, version int) []LockEffect {
	parentMode := ShareUpdateExclusive
	if version < 12 {
		parentMode = AccessExclusive
	}

	return []LockEffect{
		{Relation: parent, Mode: parentMode, Duration: Instant,
			Reason: "catalog only: attach partition"},
		{Relation: relationOf(cmd.GetName()), Mode: AccessExclusive, Duration: Scan,
			Reason: "scan verifies the partition bound"},
	}
}

// detachPartition covers DETACH PARTITION. The concurrent form waits out
// open transactions instead of taking ACCESS EXCLUSIVE, and refuses
// transaction blocks.
func detachPartition(parent Relation, cmd *pgquery.PartitionCmd) ([]LockEffect, bool) {
	child := relationOf(cmd.GetName())

	if cmd.GetConcurrent() {
		return []LockEffect{
			{Relation: parent, Mode: ShareUpdateExclusive, Duration: Instant,
				Reason: "concurrent detach waits out open transactions"},
			{Relation: child, Mode: ShareUpdateExclusive, Duration: Instant,
				Reason: "concurrent detach waits out open transactions"},
		}, true
	}

	return []LockEffect{
		{Relation: parent, Mode: AccessExclusive, Duration: Instant,
			Reason: "catalog only: detach partition"},
		{Relation: child, Mode: AccessExclusive, Duration: Instant,
			Reason: "catalog only: detach partition"},
	}, false
}

// volatileExpr reports whether a default expression forces a per-row
// evaluation. Anything the walk does not recognise counts as volatile.
func volatileExpr(node *pgquery.Node) bool {
	switch {
	case node == nil, node.GetAConst() != nil, node.GetSqlvalueFunction() != nil:
		return false

	case node.GetTypeCast() != nil:
		return volatileExpr(node.GetTypeCast().GetArg())

	case node.GetAExpr() != nil:
		expr := node.GetAExpr()
		return volatileExpr(expr.GetLexpr()) || volatileExpr(expr.GetRexpr())

	case node.GetFuncCall() != nil:
		call := node.GetFuncCall()

		// The parser downcases unquoted names, so a quoted "NOW" stays
		// distinct from the builtin, as the server would treat it.
		name := ""
		for _, part := range call.GetFuncname() {
			name = part.GetString_().GetSval()
		}

		if !nonVolatileFuncs[name] {
			return true
		}

		return slices.ContainsFunc(call.GetArgs(), volatileExpr)

	default:
		return true
	}
}
