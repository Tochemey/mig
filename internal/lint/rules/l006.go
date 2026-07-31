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

// l006 flags ADD FOREIGN KEY without NOT VALID, which validates every
// existing row while SHARE ROW EXCLUSIVE is held on both tables.
type l006 struct{}

func (l006) ID() string { return "L006" }

func (l006) Check(ctx Context, stmt *pgquery.RawStmt) []Finding {
	alter, cmds := alterCommands(stmt)
	if alter == nil {
		return nil
	}

	schema, name := tableOf(alter)

	var findings []Finding

	for _, cmd := range cmds {
		constraint := cmd.GetDef().GetConstraint()
		if constraint.GetContype() != pgquery.ConstrType_CONSTR_FOREIGN ||
			constraint.GetSkipValidation() {
			continue
		}

		// A table this migration creates has nothing to validate, so the key
		// is checked in catalog time regardless of spelling.
		if ctx.createdHere(schema, name) {
			continue
		}

		parent := constraint.GetPktable()

		findings = append(findings, finding(SeverityWarn, fmt.Sprintf(
			"ADD FOREIGN KEY validates every row of %s while blocking writes to both it and %s; "+
				"add the key NOT VALID, then VALIDATE CONSTRAINT in its own step",
			qualified(schema, name),
			qualified(parent.GetSchemaname(), parent.GetRelname())), ctx)...)
	}

	return findings
}
