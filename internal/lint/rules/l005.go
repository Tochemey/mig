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

	"github.com/tochemey/mig/internal/parse"
)

// l005 flags SET NOT NULL, which verifies every row under ACCESS EXCLUSIVE.
// From Postgres 12 the scan is skipped when a validated CHECK already proves
// the column, so a migration that follows the safe pattern, adding the check
// NOT VALID and validating it first, is not flagged.
type l005 struct{}

func (l005) ID() string { return "L005" }

func (l005) Check(ctx Context, stmt *pgquery.RawStmt) []Finding {
	alter, cmds := alterCommands(stmt)
	if alter == nil {
		return nil
	}

	var findings []Finding

	schema, name := tableOf(alter)

	for _, cmd := range cmds {
		if cmd.GetSubtype() != pgquery.AlterTableType_AT_SetNotNull {
			continue
		}

		if ctx.createdHere(schema, name) {
			continue
		}

		column := cmd.GetName()
		if ctx.TargetVersion >= 12 && validatedNotNullCheck(ctx, name, column) {
			continue
		}

		findings = append(findings, finding(SeverityWarn, fmt.Sprintf(
			"SET NOT NULL scans %s under ACCESS EXCLUSIVE to verify %q; "+
				"add CHECK (%s IS NOT NULL) NOT VALID, validate it, then set NOT NULL",
			qualified(schema, name), column, column), ctx)...)
	}

	return findings
}

// validatedNotNullCheck reports whether the migration adds and validates a
// CHECK on this table proving the column is not null, which is what lets the
// server skip the verification scan. Tables are matched by bare name: a
// migration mixing two same-named tables from different schemas is beyond
// what the offline pass can distinguish.
func validatedNotNullCheck(ctx Context, table, column string) bool {
	validated := make(map[string]bool)

	for _, s := range ctx.Migration.Steps {
		for _, statement := range s.Statements {
			if statement.Kind == parse.KindValidateConstraint &&
				statement.Target.Name == table {
				validated[statement.Target.Member] = true
			}
		}
	}

	for _, s := range ctx.Migration.Steps {
		for _, statement := range s.Statements {
			if statement.Kind != parse.KindAddConstraint ||
				statement.Target.Name != table {
				continue
			}

			name, proven, immediate := notNullCheck(statement.SQL, column)
			if proven && (immediate || validated[name]) {
				return true
			}
		}
	}

	return false
}

// notNullCheck reads an ADD CONSTRAINT statement and reports whether it is a
// CHECK (column IS NOT NULL), under what name, and whether it was validated
// at creation rather than deferred with NOT VALID.
func notNullCheck(sql, column string) (name string, proven, immediate bool) {
	tree, err := pgquery.Parse(sql)
	if err != nil {
		// The plan already parsed this statement, so a failure here cannot
		// name a constraint that proves anything.
		return "", false, false
	}

	for _, raw := range tree.Stmts {
		alter := raw.GetStmt().GetAlterTableStmt()

		for _, node := range alter.GetCmds() {
			constraint := node.GetAlterTableCmd().GetDef().GetConstraint()
			if constraint.GetContype() != pgquery.ConstrType_CONSTR_CHECK {
				continue
			}

			if !isNotNullTest(constraint.GetRawExpr(), column) {
				continue
			}

			return constraint.GetConname(), true, !constraint.GetSkipValidation()
		}
	}

	return "", false, false
}

// isNotNullTest reports whether an expression is exactly column IS NOT NULL.
func isNotNullTest(expr *pgquery.Node, column string) bool {
	test := expr.GetNullTest()
	if test == nil || test.GetNulltesttype() != pgquery.NullTestType_IS_NOT_NULL {
		return false
	}

	fields := test.GetArg().GetColumnRef().GetFields()
	if len(fields) == 0 {
		return false
	}

	return fields[len(fields)-1].GetString_().GetSval() == column
}
