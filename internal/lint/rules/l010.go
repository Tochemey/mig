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
	pgquery "github.com/pganalyze/pg_query_go/v6"
)

// l010 flags VACUUM FULL and CLUSTER, which rewrite the table under ACCESS
// EXCLUSIVE. Neither belongs in a migration at any table size, so the rule
// does not soften for tables created here.
type l010 struct{}

func (l010) ID() string { return "L010" }

func (l010) Check(ctx Context, stmt *pgquery.RawStmt) []Finding {
	if stmt.GetStmt().GetClusterStmt() != nil {
		return finding(SeverityError,
			"CLUSTER rewrites the table under ACCESS EXCLUSIVE and belongs in a "+
				"maintenance window, not a migration", ctx)
	}

	vacuum := stmt.GetStmt().GetVacuumStmt()
	if vacuum == nil {
		return nil
	}

	for _, option := range vacuum.GetOptions() {
		if option.GetDefElem().GetDefname() == "full" {
			return finding(SeverityError,
				"VACUUM FULL rewrites the table under ACCESS EXCLUSIVE and belongs in a "+
					"maintenance window, not a migration", ctx)
		}
	}

	return nil
}
