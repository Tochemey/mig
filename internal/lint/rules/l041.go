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

// l041 flags a DELETE over a table the catalog says is large.
//
// Even bounded, the rows it removes stay on disk as dead tuples until vacuum
// reaches them, and on a large table that is a lot of bloat arriving at once
// behind one long transaction. Deleting in batches lets vacuum keep up.
//
// It needs the catalog: "large" is not something the offline pass knows, and
// the rule stays silent rather than guessing.
type l041 struct{}

func (l041) ID() string { return L041 }

func (l041) Check(ctx Context, stmt *pgquery.RawStmt) []Finding {
	if ctx.Step.Spec.Kind == step.KindBackfill {
		return nil
	}

	remove := stmt.GetStmt().GetDeleteStmt()
	if remove == nil {
		return nil
	}

	relation := remove.GetRelation()

	schema, name := relation.GetSchemaname(), relation.GetRelname()
	if !large(ctx, schema, name) || ctx.createdHere(schema, name) {
		return nil
	}

	return sized(ctx, schema, name, fmt.Sprintf(
		"DELETE over %s, which the catalog reports as large: the rows stay on disk as "+
			"dead tuples until vacuum reaches them, and one statement hands vacuum all "+
			"of them at once. Delete in batches",
		qualified(schema, name)))
}
