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
)

// l008 flags ADD PRIMARY KEY without USING INDEX, whether as a constraint or
// inline on a new column. The index is built while ACCESS EXCLUSIVE is held;
// built concurrently first, it is adopted in catalog time.
type l008 struct{}

func (l008) ID() string { return L008 }

func (l008) Check(ctx Context, stmt *pgquery.RawStmt) []Finding {
	alter, cmds := alterCommands(stmt)
	if alter == nil {
		return nil
	}

	schema, name := tableOf(alter)

	var findings []Finding

	for _, cmd := range cmds {
		if !addsPrimaryKey(cmd) {
			continue
		}

		if ctx.createdHere(schema, name) {
			continue
		}

		found := sized(ctx, schema, name, fmt.Sprintf(
			"ADD PRIMARY KEY builds its index while blocking all access to %s; "+
				"build a unique index concurrently first and adopt it with USING INDEX",
			qualified(schema, name)))

		// Only the constraint form is replaced; a key inline on a new column
		// would need the column split out first.
		constraint := cmd.GetDef().GetConstraint()
		if cmd.GetSubtype() == pgquery.AlterTableType_AT_AddConstraint && len(cmds) == 1 {
			found = withFix(found, fix.PrimaryKeyViaIndex(alter.GetRelation(), constraint))
		}

		findings = append(findings, found...)
	}

	return findings
}

// addsPrimaryKey reports a primary key built by this action rather than
// adopted from an existing index.
func addsPrimaryKey(cmd *pgquery.AlterTableCmd) bool {
	switch cmd.GetSubtype() {
	case pgquery.AlterTableType_AT_AddConstraint:
		constraint := cmd.GetDef().GetConstraint()

		return constraint.GetContype() == pgquery.ConstrType_CONSTR_PRIMARY &&
			constraint.GetIndexname() == ""

	case pgquery.AlterTableType_AT_AddColumn:
		for _, node := range cmd.GetDef().GetColumnDef().GetConstraints() {
			if node.GetConstraint().GetContype() == pgquery.ConstrType_CONSTR_PRIMARY {
				return true
			}
		}

		return false

	default:
		return false
	}
}
