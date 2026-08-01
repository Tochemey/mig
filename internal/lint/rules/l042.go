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
	"github.com/tochemey/mig/internal/plan"
	"github.com/tochemey/mig/internal/step"
)

// validColumn is the catalog column that separates a finished concurrent
// index from the wreckage of an interrupted one. A hand-written predicate
// that names it has thought about the case; one that does not has not.
const validColumn = "indisvalid"

// l042 flags a concurrent index build whose reconciling predicate was written
// by hand.
//
// An author-supplied satisfied: replaces the inferred one, and the inferred
// one for this statement is "the index exists and is valid and ready". That
// wording is not decoration: an interrupted CREATE INDEX CONCURRENTLY leaves
// an index behind that exists and is invalid, which the planner ignores and
// every write to the table still maintains. A predicate that only asks
// whether the index is there reports that wreckage as done, and the next run
// skips the rebuild.
type l042 struct{}

func (l042) ID() string { return L042 }

func (l042) Check(ctx Context, stmt *pgquery.RawStmt) []Finding {
	index := stmt.GetStmt().GetIndexStmt()
	if index == nil || !index.GetConcurrent() {
		return nil
	}

	// No predicate of the author's own means the inferred one, which is the
	// one that knows about invalid indexes.
	written := ctx.Step.Spec.Satisfied
	if written == "" || mentionsIdent(written, validColumn) {
		return nil
	}

	found := finding(SeverityInfo, fmt.Sprintf(
		"this step replaces the inferred predicate, which checks that %s is valid and "+
			"ready, with one that does not mention %s: an interrupted build leaves an "+
			"invalid index, and a predicate that only finds it reports the wreckage as "+
			"done. Drop the %s%s line and let it be inferred",
		index.GetIdxname(), validColumn,
		plan.AnnotationPrefix, plan.AnnotationSatisfied), ctx)

	if len(ctx.Step.Parsed) == 1 {
		found = withFix(found, fix.WithoutSatisfied(ctx.Step.Spec.Name,
			ctx.Step.Spec.Kind == step.KindDDLNoTx, stmt.GetStmt()))
	}

	return found
}
