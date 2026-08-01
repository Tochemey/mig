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
)

// l009 flags an inline UNIQUE in ADD COLUMN, which builds its index under
// the ACCESS EXCLUSIVE the column addition already holds.
type l009 struct{}

func (l009) ID() string { return L009 }

func (l009) Check(ctx Context, stmt *pgquery.RawStmt) []Finding {
	alter, cmds := alterCommands(stmt)
	if alter == nil {
		return nil
	}

	schema, name := tableOf(alter)

	var findings []Finding

	for _, cmd := range cmds {
		if cmd.GetSubtype() != pgquery.AlterTableType_AT_AddColumn ||
			!hasInlineUnique(cmd) {
			continue
		}

		if ctx.createdHere(schema, name) {
			continue
		}

		findings = append(findings, finding(SeverityWarn, fmt.Sprintf(
			"an inline UNIQUE builds its index while blocking all access to %s; "+
				"add the column plain, then create a unique index concurrently in a notx step",
			qualified(schema, name)), ctx)...)
	}

	return findings
}

// hasInlineUnique reports a UNIQUE spelt on the column itself.
func hasInlineUnique(cmd *pgquery.AlterTableCmd) bool {
	for _, node := range cmd.GetDef().GetColumnDef().GetConstraints() {
		if node.GetConstraint().GetContype() == pgquery.ConstrType_CONSTR_UNIQUE {
			return true
		}
	}

	return false
}
