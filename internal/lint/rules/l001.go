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

// l001 flags CREATE INDEX without CONCURRENTLY, which holds SHARE on the
// table and blocks every write for the whole build.
type l001 struct{}

func (l001) ID() string { return L001 }

func (l001) Check(ctx Context, stmt *pgquery.RawStmt) []Finding {
	index := stmt.GetStmt().GetIndexStmt()
	if index == nil || index.GetConcurrent() {
		return nil
	}

	relation := index.GetRelation()
	if ctx.createdHere(relation.GetSchemaname(), relation.GetRelname()) {
		return nil
	}

	table := qualified(relation.GetSchemaname(), relation.GetRelname())

	return sized(ctx, relation.GetSchemaname(), relation.GetRelname(), fmt.Sprintf(
		"CREATE INDEX without CONCURRENTLY blocks writes to %s for the whole build; "+
			"add CONCURRENTLY and mark the step notx", table))
}
