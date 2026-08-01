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

	"github.com/tochemey/mig/internal/step"
)

// l040 flags an UPDATE or DELETE with no WHERE clause outside a backfill.
//
// Every row in one transaction: the locks are held to the end, the dead
// tuples all become garbage at once, and a failure two thirds through rolls
// the whole thing back to do again. A backfill step does the same work in
// batches that commit, checkpoint and resume.
//
// A statement that has a WHERE is left alone. Nothing here can tell how many
// rows a predicate matches, and warning about every predicated write is how a
// linter teaches its reader to skip warnings.
type l040 struct{}

func (l040) ID() string { return L040 }

func (l040) Check(ctx Context, stmt *pgquery.RawStmt) []Finding {
	// A backfill is the shape being asked for, so it is never the finding.
	if ctx.Step.Spec.Kind == step.KindBackfill {
		return nil
	}

	what, relation, bounded := unboundedWrite(stmt)
	if relation == nil || bounded {
		return nil
	}

	schema, name := relation.GetSchemaname(), relation.GetRelname()
	if ctx.createdHere(schema, name) {
		return nil
	}

	return sized(ctx, schema, name, fmt.Sprintf(
		"%s over every row of %s in one transaction: the locks are held to the end "+
			"and a failure part-way rolls all of it back. Make it a backfill step, "+
			"which commits and checkpoints as it goes",
		what, qualified(schema, name)))
}

// unboundedWrite reports the statement's kind, the table it writes and
// whether the author bounded it with a WHERE clause.
func unboundedWrite(stmt *pgquery.RawStmt) (what string, relation *pgquery.RangeVar, bounded bool) {
	if update := stmt.GetStmt().GetUpdateStmt(); update != nil {
		return "UPDATE", update.GetRelation(), update.GetWhereClause() != nil
	}

	if remove := stmt.GetStmt().GetDeleteStmt(); remove != nil {
		return "DELETE", remove.GetRelation(), remove.GetWhereClause() != nil
	}

	return "", nil, false
}
