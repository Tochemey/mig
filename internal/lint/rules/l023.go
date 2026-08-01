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

// l023 flags a foreign key added before the backfill that populates the
// column it constrains.
//
// The key is validated against rows that do not have their values yet, and
// then every row the backfill writes is checked against the referenced table
// one at a time. Added after the backfill, it is validated once, over data
// that is already right.
type l023 struct{}

func (l023) ID() string { return L023 }

func (l023) Check(ctx Context, stmt *pgquery.RawStmt) []Finding {
	alter, cmds := alterCommands(stmt)
	if alter == nil {
		return nil
	}

	schema, name := tableOf(alter)
	if !backfilledAfter(ctx, name) {
		return nil
	}

	var findings []Finding

	for _, cmd := range cmds {
		constraint := cmd.GetDef().GetConstraint()
		if constraint.GetContype() != pgquery.ConstrType_CONSTR_FOREIGN {
			continue
		}

		parent := constraint.GetPktable()

		findings = append(findings, finding(SeverityWarn, fmt.Sprintf(
			"this key on %s is added before the backfill that populates it, so every "+
				"row the backfill writes is checked against %s one at a time; "+
				"add the key after the backfill",
			qualified(schema, name),
			qualified(parent.GetSchemaname(), parent.GetRelname())), ctx)...)
	}

	return findings
}
