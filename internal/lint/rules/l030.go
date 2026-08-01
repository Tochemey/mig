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

// l030 flags dropping a column or a table.
//
// The hazard is not the drop: it is the deploy. Code still holding a
// reference to the object fails the moment it commits, and a rollback of the
// migration cannot bring the data back. The safe order is to stop referencing
// it, ship that, and drop afterwards: a deploy boundary, which nothing here
// can see.
type l030 struct{}

func (l030) ID() string { return L030 }

func (l030) Check(ctx Context, stmt *pgquery.RawStmt) []Finding {
	if dropped := droppedTables(ctx, stmt); len(dropped) > 0 {
		return finding(SeverityWarn, fmt.Sprintf(
			"dropping %s: any code still reading it fails the moment this commits, and "+
				"the rows do not come back. Ship the release that stops referencing it "+
				"first", dropped[0]), ctx)
	}

	alter, cmds := alterCommands(stmt)
	if alter == nil {
		return nil
	}

	schema, name := tableOf(alter)

	var findings []Finding

	for _, cmd := range cmds {
		if cmd.GetSubtype() != pgquery.AlterTableType_AT_DropColumn || ctx.createdHere(schema, name) {
			continue
		}

		findings = append(findings, finding(SeverityWarn, fmt.Sprintf(
			"dropping %s.%s: any code still reading it fails the moment this commits, "+
				"and the values do not come back. Ship the release that stops "+
				"referencing it first", qualified(schema, name), cmd.GetName()), ctx)...)
	}

	return findings
}

// droppedTables names the tables a DROP TABLE names, ignoring one this
// migration created for itself.
func droppedTables(ctx Context, stmt *pgquery.RawStmt) []string {
	drop := stmt.GetStmt().GetDropStmt()
	if drop.GetRemoveType() != pgquery.ObjectType_OBJECT_TABLE {
		return nil
	}

	var dropped []string

	for _, object := range drop.GetObjects() {
		schema, name := nameOf(object)
		if !ctx.createdHere(schema, name) {
			dropped = append(dropped, qualified(schema, name))
		}
	}

	return dropped
}

// nameOf reads a possibly qualified name out of an object list: the last part
// is the name and the one before it is the schema, so each part in turn
// becomes the name and pushes its predecessor into the schema.
func nameOf(object *pgquery.Node) (schema, name string) {
	for _, part := range object.GetList().GetItems() {
		schema, name = name, part.GetString_().GetSval()
	}

	return schema, name
}
