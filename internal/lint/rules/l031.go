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

// renamed names what each renameable object is called in a finding.
var renamed = map[pgquery.ObjectType]string{
	pgquery.ObjectType_OBJECT_TABLE:  "table",
	pgquery.ObjectType_OBJECT_COLUMN: "column",
}

// l031 flags renaming a table or a column.
//
// A rename is atomic in the database and anything but atomic in the fleet:
// the moment it commits, every process still using the old name is broken,
// and the ones already using the new name were broken until then. There is no
// deploy order that makes it safe. What is safe is expand-contract: add the
// new name, write both, move readers, drop the old one, each its own release.
type l031 struct{}

func (l031) ID() string { return L031 }

func (l031) Check(ctx Context, stmt *pgquery.RawStmt) []Finding {
	rename := stmt.GetStmt().GetRenameStmt()

	what, ok := renamed[rename.GetRenameType()]
	if !ok {
		return nil
	}

	relation := rename.GetRelation()

	schema, name := relation.GetSchemaname(), relation.GetRelname()
	if ctx.createdHere(schema, name) {
		return nil
	}

	from := qualified(schema, name)
	if what == renamed[pgquery.ObjectType_OBJECT_COLUMN] {
		from += "." + rename.GetSubname()
	}

	return finding(SeverityError, fmt.Sprintf(
		"renaming the %s %s to %q breaks every process still using the old name at the "+
			"moment it commits, and no deploy order avoids it. Add the new name "+
			"alongside, move readers and writers to it, then drop the old one",
		what, from, rename.GetNewname()), ctx)
}
