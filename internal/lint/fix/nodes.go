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

package fix

import (
	pgquery "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/proto"
)

// The constructors below assemble the handful of parse-tree shapes the
// builders emit. Assembling trees and deparsing them is what keeps generated
// SQL correctly quoted without this package owning any quoting rules.

// dup deep-copies a node, so a builder never mutates the caller's tree.
func dup[T proto.Message](m T) T {
	return proto.Clone(m).(T)
}

// alterTable wraps commands into an ALTER TABLE statement.
func alterTable(rel *pgquery.RangeVar, cmds ...*pgquery.Node) *pgquery.Node {
	return &pgquery.Node{Node: &pgquery.Node_AlterTableStmt{AlterTableStmt: &pgquery.AlterTableStmt{
		Relation: dup(rel),
		Objtype:  pgquery.ObjectType_OBJECT_TABLE,
		Cmds:     cmds,
	}}}
}

// alterCmd is one ALTER TABLE action.
func alterCmd(subtype pgquery.AlterTableType, name string, def *pgquery.Node) *pgquery.Node {
	return &pgquery.Node{Node: &pgquery.Node_AlterTableCmd{AlterTableCmd: &pgquery.AlterTableCmd{
		Subtype:  subtype,
		Name:     name,
		Def:      def,
		Behavior: pgquery.DropBehavior_DROP_RESTRICT,
	}}}
}

// constraint wraps a constraint definition for an ADD CONSTRAINT action.
func constraint(c *pgquery.Constraint) *pgquery.Node {
	return &pgquery.Node{Node: &pgquery.Node_Constraint{Constraint: c}}
}

// stringNode is a bare identifier part.
func stringNode(value string) *pgquery.Node {
	return &pgquery.Node{Node: &pgquery.Node_String_{String_: &pgquery.String{Sval: value}}}
}

// columnRef names a column.
func columnRef(name string) *pgquery.Node {
	return &pgquery.Node{Node: &pgquery.Node_ColumnRef{ColumnRef: &pgquery.ColumnRef{
		Fields: []*pgquery.Node{stringNode(name)},
	}}}
}

// paramRef is a bound parameter, which render rewrites into a cursor
// placeholder.
func paramRef(number int32) *pgquery.Node {
	return &pgquery.Node{Node: &pgquery.Node_ParamRef{ParamRef: &pgquery.ParamRef{Number: number}}}
}

// intConst is a literal integer.
func intConst(value int32) *pgquery.Node {
	return &pgquery.Node{Node: &pgquery.Node_AConst{AConst: &pgquery.A_Const{
		Val: &pgquery.A_Const_Ival{Ival: &pgquery.Integer{Ival: value}},
	}}}
}

// nullTest is column IS NULL or IS NOT NULL.
func nullTest(arg *pgquery.Node, notNull bool) *pgquery.Node {
	kind := pgquery.NullTestType_IS_NULL
	if notNull {
		kind = pgquery.NullTestType_IS_NOT_NULL
	}

	return &pgquery.Node{Node: &pgquery.Node_NullTest{NullTest: &pgquery.NullTest{
		Arg:          arg,
		Nulltesttype: kind,
	}}}
}

// binaryOp is left <op> right.
func binaryOp(op string, left, right *pgquery.Node) *pgquery.Node {
	return &pgquery.Node{Node: &pgquery.Node_AExpr{AExpr: &pgquery.A_Expr{
		Kind:  pgquery.A_Expr_Kind_AEXPR_OP,
		Name:  []*pgquery.Node{stringNode(op)},
		Lexpr: left,
		Rexpr: right,
	}}}
}

// boolAnd conjoins expressions.
func boolAnd(args ...*pgquery.Node) *pgquery.Node {
	return &pgquery.Node{Node: &pgquery.Node_BoolExpr{BoolExpr: &pgquery.BoolExpr{
		Boolop: pgquery.BoolExprType_AND_EXPR,
		Args:   args,
	}}}
}

// boolNot negates an expression.
func boolNot(arg *pgquery.Node) *pgquery.Node {
	return &pgquery.Node{Node: &pgquery.Node_BoolExpr{BoolExpr: &pgquery.BoolExpr{
		Boolop: pgquery.BoolExprType_NOT_EXPR,
		Args:   []*pgquery.Node{arg},
	}}}
}

// resTarget is one item of a select or update list.
func resTarget(name string, value *pgquery.Node) *pgquery.Node {
	return &pgquery.Node{Node: &pgquery.Node_ResTarget{ResTarget: &pgquery.ResTarget{
		Name: name,
		Val:  value,
	}}}
}

// rangeVarNode wraps a relation for a FROM clause.
func rangeVarNode(rel *pgquery.RangeVar) *pgquery.Node {
	return &pgquery.Node{Node: &pgquery.Node_RangeVar{RangeVar: dup(rel)}}
}

// columnDefNode wraps a column definition for an ADD COLUMN action.
func columnDefNode(column *pgquery.ColumnDef) *pgquery.Node {
	return &pgquery.Node{Node: &pgquery.Node_ColumnDef{ColumnDef: column}}
}

// typeCast is expression::type.
func typeCast(arg *pgquery.Node, typeName *pgquery.TypeName) *pgquery.Node {
	return &pgquery.Node{Node: &pgquery.Node_TypeCast{TypeCast: &pgquery.TypeCast{
		Arg:      arg,
		TypeName: dup(typeName),
	}}}
}

// renameColumn is ALTER TABLE ... RENAME COLUMN from TO to.
func renameColumn(rel *pgquery.RangeVar, from, to string) *pgquery.Node {
	return &pgquery.Node{Node: &pgquery.Node_RenameStmt{RenameStmt: &pgquery.RenameStmt{
		RenameType:   pgquery.ObjectType_OBJECT_COLUMN,
		RelationType: pgquery.ObjectType_OBJECT_TABLE,
		Relation:     dup(rel),
		Subname:      from,
		Newname:      to,
		Behavior:     pgquery.DropBehavior_DROP_RESTRICT,
	}}}
}

// backfillUpdate is the batched backfill statement: set the column from its
// expression over one cursor range, touching only rows still unset.
func backfillUpdate(rel *pgquery.RangeVar, column string, value *pgquery.Node, key string) *pgquery.Node {
	return &pgquery.Node{Node: &pgquery.Node_UpdateStmt{UpdateStmt: &pgquery.UpdateStmt{
		Relation:   dup(rel),
		TargetList: []*pgquery.Node{resTarget(column, value)},
		WhereClause: boolAnd(
			binaryOp(">", columnRef(key), paramRef(1)),
			binaryOp("<=", columnRef(key), paramRef(2)),
			nullTest(columnRef(column), false),
		),
	}}}
}

// noNullsRemain is the backfill's convergence predicate: no row of the table
// still holds NULL in the column.
func noNullsRemain(rel *pgquery.RangeVar, column string) *pgquery.Node {
	exists := &pgquery.Node{Node: &pgquery.Node_SubLink{SubLink: &pgquery.SubLink{
		SubLinkType: pgquery.SubLinkType_EXISTS_SUBLINK,
		Subselect: &pgquery.Node{Node: &pgquery.Node_SelectStmt{SelectStmt: &pgquery.SelectStmt{
			TargetList:  []*pgquery.Node{resTarget("", intConst(1))},
			FromClause:  []*pgquery.Node{rangeVarNode(rel)},
			WhereClause: nullTest(columnRef(column), false),
		}}},
	}}}

	return &pgquery.Node{Node: &pgquery.Node_SelectStmt{SelectStmt: &pgquery.SelectStmt{
		TargetList: []*pgquery.Node{resTarget("", boolNot(exists))},
	}}}
}

// checkNotNullConstraint is CHECK (column IS NOT NULL) NOT VALID under name.
func checkNotNullConstraint(name, column string) *pgquery.Constraint {
	return &pgquery.Constraint{
		Contype:        pgquery.ConstrType_CONSTR_CHECK,
		Conname:        name,
		RawExpr:        nullTest(columnRef(column), true),
		SkipValidation: true,
	}
}

// uniqueIndexConcurrently builds CREATE UNIQUE INDEX CONCURRENTLY over the
// key columns.
func uniqueIndexConcurrently(name string, rel *pgquery.RangeVar, columns []string) *pgquery.Node {
	params := make([]*pgquery.Node, 0, len(columns))

	for _, column := range columns {
		params = append(params, &pgquery.Node{Node: &pgquery.Node_IndexElem{IndexElem: &pgquery.IndexElem{
			Name:          column,
			Ordering:      pgquery.SortByDir_SORTBY_DEFAULT,
			NullsOrdering: pgquery.SortByNulls_SORTBY_NULLS_DEFAULT,
		}}})
	}

	return &pgquery.Node{Node: &pgquery.Node_IndexStmt{IndexStmt: &pgquery.IndexStmt{
		Idxname:      name,
		Relation:     dup(rel),
		AccessMethod: "btree",
		Unique:       true,
		Concurrent:   true,
		IndexParams:  params,
	}}}
}
