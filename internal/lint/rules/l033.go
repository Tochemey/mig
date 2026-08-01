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

// l033 flags TRUNCATE.
//
// It takes ACCESS EXCLUSIVE, and what it removes is not recoverable by
// rolling the migration back. Emptying a table is a data change, and belongs
// wherever the rest of the data changes are reviewed.
type l033 struct{}

func (l033) ID() string { return L033 }

func (l033) Check(ctx Context, stmt *pgquery.RawStmt) []Finding {
	truncate := stmt.GetStmt().GetTruncateStmt()
	if truncate == nil {
		return nil
	}

	var tables []string

	for _, relation := range truncate.GetRelations() {
		rel := relation.GetRangeVar()
		if !ctx.createdHere(rel.GetSchemaname(), rel.GetRelname()) {
			tables = append(tables, qualified(rel.GetSchemaname(), rel.GetRelname()))
		}
	}

	if len(tables) == 0 {
		return nil
	}

	return finding(SeverityError, fmt.Sprintf(
		"TRUNCATE empties %s under ACCESS EXCLUSIVE, and rolling the migration back "+
			"does not bring the rows back. Emptying a table is a data change, not a "+
			"schema change", tables[0]), ctx)
}
