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

// l007 flags ADD CHECK without NOT VALID, which scans the table under ACCESS
// EXCLUSIVE instead of validating later without blocking anything.
type l007 struct{}

func (l007) ID() string { return L007 }

func (l007) Check(ctx Context, stmt *pgquery.RawStmt) []Finding {
	alter, cmds := alterCommands(stmt)
	if alter == nil {
		return nil
	}

	schema, name := tableOf(alter)

	var findings []Finding

	for _, cmd := range cmds {
		constraint := cmd.GetDef().GetConstraint()
		if constraint.GetContype() != pgquery.ConstrType_CONSTR_CHECK ||
			constraint.GetSkipValidation() {
			continue
		}

		if ctx.createdHere(schema, name) {
			continue
		}

		found := sized(ctx, schema, name, fmt.Sprintf(
			"ADD CHECK scans %s under ACCESS EXCLUSIVE; "+
				"add it NOT VALID, then VALIDATE CONSTRAINT in its own step",
			qualified(schema, name)))

		// An anonymous check gets its name from the server, so the validation
		// step would have nothing to name; that fix stays unwritten.
		if constraint.GetConname() != "" && len(cmds) == 1 {
			found = withFix(found, fix.CheckTwoStep(alter.GetRelation(), constraint))
		}

		findings = append(findings, found...)
	}

	return findings
}
