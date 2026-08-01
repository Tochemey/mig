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

// l032 flags a table this migration creates and never grants access to.
//
// The migration runs as a role that owns what it creates. The application
// runs as a restricted role, which is granted nothing by default, so the table
// exists and is invisible to the only process that needs it. Nothing in CI
// catches it, because CI usually migrates and queries as the same role.
type l032 struct{}

func (l032) ID() string { return L032 }

func (l032) Check(ctx Context, stmt *pgquery.RawStmt) []Finding {
	create := stmt.GetStmt().GetCreateStmt()
	if create == nil {
		return nil
	}

	relation := create.GetRelation()

	schema, name := relation.GetSchemaname(), relation.GetRelname()

	// A table that does not outlive the migration needs no grant: scaffolding
	// created and dropped here is never seen by the application at all.
	if droppedHere(ctx, name) || grantedAfter(ctx, name) {
		return nil
	}

	return finding(SeverityWarn, fmt.Sprintf(
		"nothing in this migration grants access to %s. A table created by the "+
			"migration role is invisible to a restricted application role, and the "+
			"failure surfaces in production rather than in CI",
		qualified(schema, name)), ctx)
}

// droppedHere reports whether the migration drops the table anywhere. Like
// the grants, this reads the parse trees: a drop is not a kind the executor
// distinguishes either.
func droppedHere(ctx Context, table string) bool {
	for _, s := range ctx.Migration.Steps {
		for _, statement := range s.Statements {
			tree, _ := pgquery.Parse(statement.SQL)

			for _, raw := range tree.GetStmts() {
				drop := raw.GetStmt().GetDropStmt()
				if drop.GetRemoveType() != pgquery.ObjectType_OBJECT_TABLE {
					continue
				}

				for _, object := range drop.GetObjects() {
					if _, name := nameOf(object); name == table {
						return true
					}
				}
			}
		}
	}

	return false
}

// grantedAfter reports whether a later statement of the migration grants on
// the table.
//
// Later is the whole point: GRANT ON ALL TABLES IN SCHEMA covers the tables
// that exist when it runs, so one written above the CREATE does not reach it.
// A grant naming the table could not run before it either.
//
// The statements are parsed here rather than read from the plan, because the
// executor classifies what it has to run and a grant is not a kind it
// distinguishes. Tables are matched by bare name, as the other
// cross-statement rules match them.
func grantedAfter(ctx Context, table string) bool {
	for stepIndex, s := range ctx.Migration.Steps {
		if stepIndex < ctx.StepIndex {
			continue
		}

		statements := s.Statements
		if stepIndex == ctx.StepIndex {
			statements = statements[ctx.StmtIndex+1:]
		}

		for _, statement := range statements {
			// The plan already parsed this statement, so an unparseable one
			// here grants nothing, which is what the empty tree says.
			tree, _ := pgquery.Parse(statement.SQL)

			for _, raw := range tree.GetStmts() {
				if grants(raw.GetStmt().GetGrantStmt(), table) {
					return true
				}
			}
		}
	}

	return false
}

// grants reports whether one GRANT covers the table, by naming it or by
// covering every table in the schema it runs against.
func grants(grant *pgquery.GrantStmt, table string) bool {
	if grant == nil || !grant.GetIsGrant() {
		return false
	}

	if grant.GetTargtype() == pgquery.GrantTargetType_ACL_TARGET_ALL_IN_SCHEMA {
		return true
	}

	for _, object := range grant.GetObjects() {
		if object.GetRangeVar().GetRelname() == table {
			return true
		}
	}

	return false
}
