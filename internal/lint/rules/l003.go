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

package rules

import (
	"fmt"

	pgquery "github.com/pganalyze/pg_query_go/v6"

	"github.com/tochemey/mig/internal/lint/fix"
	"github.com/tochemey/mig/internal/lint/lockmodel"
)

// l003 flags ADD COLUMN forms that rewrite the table: a volatile default, a
// serial or identity column, a stored generated expression, or any default at
// all before Postgres 11. The lock model decides; the rule reports.
type l003 struct{}

func (l003) ID() string { return "L003" }

func (l003) Check(ctx Context, stmt *pgquery.RawStmt) []Finding {
	alter, cmds := alterCommands(stmt)
	if alter == nil {
		return nil
	}

	schema, name := tableOf(alter)
	if ctx.createdHere(schema, name) {
		return nil
	}

	table := lockmodel.Relation{Schema: schema, Name: name}

	var findings []Finding

	// Each action is classified on its own, so a rewrite caused by a
	// sibling action in the same statement is not pinned on the column.
	for _, cmd := range cmds {
		if cmd.GetSubtype() != pgquery.AlterTableType_AT_AddColumn {
			continue
		}

		column := cmd.GetDef().GetColumnDef()

		effect := lockmodel.AnalyzeAddColumn(table, column, ctx.TargetVersion)
		if effect.Duration != lockmodel.Rewrite {
			continue
		}

		found := finding(SeverityWarn, fmt.Sprintf(
			"ADD COLUMN rewrites %s under ACCESS EXCLUSIVE (%s); "+
				"add the column nullable, backfill in batches, then constrain it",
			qualified(schema, name), effect.Reason), ctx)

		if def, notNull, ok := fixableAddColumn(column); ok && len(cmds) == 1 {
			found = withFix(found, fix.AddColumnWithDefault(alter.GetRelation(), column, def, notNull))
		}

		findings = append(findings, found...)
	}

	return findings
}

// fixableAddColumn reports whether the expand route replaces the column
// faithfully: the rewrite must come from a default, and nothing beyond NOT
// NULL may ride on the column. A serial, identity, generated or constrained
// column needs a fix this rule cannot write.
func fixableAddColumn(column *pgquery.ColumnDef) (def *pgquery.Node, notNull, ok bool) {
	for _, node := range column.GetConstraints() {
		switch c := node.GetConstraint(); c.GetContype() {
		case pgquery.ConstrType_CONSTR_DEFAULT:
			def = c.GetRawExpr()

		case pgquery.ConstrType_CONSTR_NOTNULL:
			notNull = true

		default:
			return nil, false, false
		}
	}

	return def, notNull, def != nil
}
