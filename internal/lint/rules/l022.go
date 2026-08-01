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
	"strings"

	pgquery "github.com/pganalyze/pg_query_go/v6"

	"github.com/tochemey/mig/internal/plan"
	"github.com/tochemey/mig/internal/step"
)

// l022 flags an index built before the backfill that fills the rows it will
// hold.
//
// Every batch of the backfill then maintains the index as it writes, which is
// slower than building the index once over the finished data, and leaves the
// index bloated by the churn. Built afterwards, it is one pass over rows that
// have stopped moving.
type l022 struct{}

func (l022) ID() string { return L022 }

func (l022) Check(ctx Context, stmt *pgquery.RawStmt) []Finding {
	index := stmt.GetStmt().GetIndexStmt()
	if index == nil {
		return nil
	}

	relation := index.GetRelation()

	table := qualified(relation.GetSchemaname(), relation.GetRelname())
	if !backfilledAfter(ctx, relation.GetRelname()) {
		return nil
	}

	return finding(SeverityInfo, fmt.Sprintf(
		"this index on %s is built before the backfill that fills it, so every batch "+
			"maintains it as it writes; move the index after the backfill",
		table), ctx)
}

// backfilledAfter reports whether a later step backfills the table.
//
// Tables are matched by bare name: the backfill annotation names its table as
// the author wrote it, and a migration mixing two same-named tables from
// different schemas is beyond what this pass can distinguish.
func backfilledAfter(ctx Context, table string) bool {
	for _, s := range ctx.Migration.Steps[ctx.StepIndex+1:] {
		if backfills(s, table) {
			return true
		}
	}

	return false
}

// backfills reports whether the step is a backfill over the table.
func backfills(s plan.Step, table string) bool {
	return s.Kind == step.KindBackfill && bareName(s.Backfill.Table) == table
}

// bareName drops a schema qualifier, so "app.users" and "users" compare as
// the same table.
func bareName(name string) string {
	if dot := strings.LastIndex(name, "."); dot >= 0 {
		return name[dot+1:]
	}

	return name
}
