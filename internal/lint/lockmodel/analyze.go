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

package lockmodel

import (
	"fmt"

	pgquery "github.com/pganalyze/pg_query_go/v6"
)

// Analyze predicts the locks one statement takes on a server of the given
// major version.
//
// A statement the model does not recognise returns an empty analysis rather
// than an error: most unrecognised statements, such as CREATE FUNCTION, take
// no table locks worth reporting. The known exception is ALTER TABLE, whose
// unmodelled actions fall back to ACCESS EXCLUSIVE held for catalog work.
func Analyze(sql string, version int) (Analysis, error) {
	tree, err := pgquery.Parse(sql)
	if err != nil {
		return Analysis{}, fmt.Errorf("parse statement: %w", err)
	}

	if len(tree.Stmts) != 1 {
		return Analysis{}, fmt.Errorf("expected one statement, got %d", len(tree.Stmts))
	}

	return analyze(tree.Stmts[0].GetStmt(), version), nil
}

// AnalyzeStatement predicts the locks of an already parsed statement, for a
// caller that holds the tree and should not pay for a second parse.
func AnalyzeStatement(stmt *pgquery.RawStmt, version int) Analysis {
	return analyze(stmt.GetStmt(), version)
}

// analyze dispatches one parsed statement to its handler.
func analyze(node *pgquery.Node, version int) Analysis {
	switch {
	case node.GetIndexStmt() != nil:
		return createIndex(node.GetIndexStmt())

	case node.GetDropStmt() != nil:
		return dropObjects(node.GetDropStmt())

	case node.GetAlterTableStmt() != nil:
		return alterTable(node.GetAlterTableStmt(), version)

	case node.GetRenameStmt() != nil:
		return rename(node.GetRenameStmt(), version)

	case node.GetTruncateStmt() != nil:
		return truncate(node.GetTruncateStmt())

	case node.GetVacuumStmt() != nil:
		return vacuum(node.GetVacuumStmt())

	case node.GetClusterStmt() != nil:
		return cluster(node.GetClusterStmt())

	case node.GetReindexStmt() != nil:
		return reindex(node.GetReindexStmt())

	case node.GetRefreshMatViewStmt() != nil:
		return refreshMatView(node.GetRefreshMatViewStmt())

	case node.GetLockStmt() != nil:
		return lockTable(node.GetLockStmt())

	case node.GetCreateStmt() != nil:
		return createTable(node.GetCreateStmt())

	case node.GetAlterEnumStmt() != nil:
		// The locks are on the type, not on a relation. Before Postgres 12 a
		// new enum value could not be added inside a transaction block.
		return Analysis{NoTx: version < 12}

	case node.GetSelectStmt() != nil:
		return Analysis{Effects: selectEffects(node.GetSelectStmt())}

	case node.GetInsertStmt() != nil:
		return insert(node.GetInsertStmt())

	case node.GetUpdateStmt() != nil:
		return update(node.GetUpdateStmt())

	case node.GetDeleteStmt() != nil:
		return deleteFrom(node.GetDeleteStmt())

	default:
		return Analysis{}
	}
}

// relationOf reads a range variable into a relation name.
func relationOf(rv *pgquery.RangeVar) Relation {
	return Relation{Schema: rv.GetSchemaname(), Name: rv.GetRelname()}
}

// createIndex covers CREATE [UNIQUE] INDEX [CONCURRENTLY].
func createIndex(stmt *pgquery.IndexStmt) Analysis {
	table := relationOf(stmt.GetRelation())

	if stmt.GetConcurrent() {
		effect := LockEffect{
			Relation: table,
			Mode:     ShareUpdateExclusive,
			Duration: IndexBuild,
			Reason:   "concurrent index build: two scans, waits out open transactions",
		}

		return Analysis{Effects: []LockEffect{effect}, NoTx: true}
	}

	effect := LockEffect{
		Relation: table,
		Mode:     Share,
		Duration: IndexBuild,
		Reason:   "index build blocks writes for its whole duration",
	}

	return Analysis{Effects: []LockEffect{effect}}
}

// droppable maps the object classes whose DROP locks a relation.
var droppable = map[pgquery.ObjectType]string{
	pgquery.ObjectType_OBJECT_TABLE:    "drop table",
	pgquery.ObjectType_OBJECT_INDEX:    "drop index",
	pgquery.ObjectType_OBJECT_VIEW:     "drop view",
	pgquery.ObjectType_OBJECT_MATVIEW:  "drop materialized view",
	pgquery.ObjectType_OBJECT_SEQUENCE: "drop sequence",
}

// dropObjects covers DROP TABLE, INDEX, VIEW, MATERIALIZED VIEW and SEQUENCE.
// Only an index may be dropped concurrently.
func dropObjects(stmt *pgquery.DropStmt) Analysis {
	what, ok := droppable[stmt.GetRemoveType()]
	if !ok {
		return Analysis{}
	}

	analysis := Analysis{}

	for _, object := range stmt.GetObjects() {
		rel, ok := relationFromNameList(object)
		if !ok {
			continue
		}

		effect := LockEffect{
			Relation: rel,
			Mode:     AccessExclusive,
			Duration: Instant,
			Reason:   "catalog only: " + what,
		}

		if stmt.GetConcurrent() {
			effect.Mode = ShareUpdateExclusive
			effect.Reason = what + " waits for every transaction using the index"
			analysis.NoTx = true
		}

		analysis.Effects = append(analysis.Effects, effect)
	}

	return analysis
}

// relationFromNameList reads a possibly schema-qualified name list, which is
// how DROP spells its targets.
func relationFromNameList(object *pgquery.Node) (Relation, bool) {
	items := object.GetList().GetItems()
	parts := make([]string, 0, len(items))

	for _, item := range items {
		parts = append(parts, item.GetString_().GetSval())
	}

	switch len(parts) {
	case 1:
		return Relation{Name: parts[0]}, true
	case 2:
		return Relation{Schema: parts[0], Name: parts[1]}, true
	default:
		return Relation{}, false
	}
}

// renameable maps the object classes whose RENAME locks a relation.
var renameable = map[pgquery.ObjectType]string{
	pgquery.ObjectType_OBJECT_TABLE:         "rename table",
	pgquery.ObjectType_OBJECT_COLUMN:        "rename column",
	pgquery.ObjectType_OBJECT_TABCONSTRAINT: "rename constraint",
	pgquery.ObjectType_OBJECT_VIEW:          "rename view",
	pgquery.ObjectType_OBJECT_MATVIEW:       "rename materialized view",
	pgquery.ObjectType_OBJECT_INDEX:         "rename index",
}

// rename covers ALTER ... RENAME. Renaming an index needs only SHARE UPDATE
// EXCLUSIVE from Postgres 12 on; every other rename takes ACCESS EXCLUSIVE.
func rename(stmt *pgquery.RenameStmt, version int) Analysis {
	what, ok := renameable[stmt.GetRenameType()]
	if !ok {
		return Analysis{}
	}

	effect := LockEffect{
		Relation: relationOf(stmt.GetRelation()),
		Mode:     AccessExclusive,
		Duration: Instant,
		Reason:   "catalog only: " + what,
	}

	if stmt.GetRenameType() == pgquery.ObjectType_OBJECT_INDEX && version >= 12 {
		effect.Mode = ShareUpdateExclusive
	}

	return Analysis{Effects: []LockEffect{effect}}
}

// truncate covers TRUNCATE, which swaps the storage out under ACCESS
// EXCLUSIVE without visiting the rows.
func truncate(stmt *pgquery.TruncateStmt) Analysis {
	analysis := Analysis{}

	for _, relation := range stmt.GetRelations() {
		analysis.Effects = append(analysis.Effects, LockEffect{
			Relation: relationOf(relation.GetRangeVar()),
			Mode:     AccessExclusive,
			Duration: Instant,
			Reason:   "truncate replaces the storage without scanning it",
		})
	}

	return analysis
}

// vacuum covers VACUUM [FULL] and ANALYZE. Neither VACUUM form runs inside a
// transaction block; ANALYZE does.
func vacuum(stmt *pgquery.VacuumStmt) Analysis {
	full := false

	for _, option := range stmt.GetOptions() {
		if option.GetDefElem().GetDefname() == "full" {
			full = true
		}
	}

	analysis := Analysis{NoTx: stmt.GetIsVacuumcmd()}

	for _, rel := range stmt.GetRels() {
		relation := relationOf(rel.GetVacuumRelation().GetRelation())

		switch {
		case full:
			analysis.Effects = append(analysis.Effects, LockEffect{
				Relation: relation,
				Mode:     AccessExclusive,
				Duration: Rewrite,
				Reason:   "table rewrite: vacuum full copies the table",
			})

		case stmt.GetIsVacuumcmd():
			analysis.Effects = append(analysis.Effects, LockEffect{
				Relation: relation,
				Mode:     ShareUpdateExclusive,
				Duration: Scan,
				Reason:   "vacuum scans the table without blocking traffic",
			})

		default:
			analysis.Effects = append(analysis.Effects, LockEffect{
				Relation: relation,
				Mode:     ShareUpdateExclusive,
				Duration: Scan,
				Reason:   "analyze samples the table without blocking traffic",
			})
		}
	}

	return analysis
}

// cluster covers CLUSTER. The single-table form runs inside a transaction;
// the whole-database form does not, and names nothing the model can predict.
func cluster(stmt *pgquery.ClusterStmt) Analysis {
	if stmt.GetRelation() == nil {
		return Analysis{NoTx: true}
	}

	table := relationOf(stmt.GetRelation())

	effects := []LockEffect{{
		Relation: table,
		Mode:     AccessExclusive,
		Duration: Rewrite,
		Reason:   "table rewrite: cluster copies the table in index order",
	}}

	if name := stmt.GetIndexname(); name != "" {
		effects = append(effects, LockEffect{
			Relation: Relation{Schema: table.Schema, Name: name},
			Mode:     AccessExclusive,
			Duration: Rewrite,
			Implicit: true,
			Reason:   "cluster rebuilds the ordering index",
		})
	}

	return Analysis{Effects: effects}
}

// reindex covers REINDEX INDEX and REINDEX TABLE. The wider forms lock
// relations only the catalog can name, and refuse transaction blocks.
func reindex(stmt *pgquery.ReindexStmt) Analysis {
	concurrent := false

	for _, param := range stmt.GetParams() {
		if param.GetDefElem().GetDefname() == "concurrently" {
			concurrent = true
		}
	}

	switch stmt.GetKind() {
	case pgquery.ReindexObjectType_REINDEX_OBJECT_INDEX:
		effect := LockEffect{
			Relation: relationOf(stmt.GetRelation()),
			Mode:     AccessExclusive,
			Duration: IndexBuild,
			Reason:   "index rebuild blocks every use of the index",
		}

		if concurrent {
			effect.Mode = ShareUpdateExclusive
			effect.Reason = "concurrent index rebuild: two scans, waits out open transactions"
		}

		return Analysis{Effects: []LockEffect{effect}, NoTx: concurrent}

	case pgquery.ReindexObjectType_REINDEX_OBJECT_TABLE:
		effect := LockEffect{
			Relation: relationOf(stmt.GetRelation()),
			Mode:     Share,
			Duration: IndexBuild,
			Reason:   "rebuilding every index blocks writes for the whole build",
		}

		if concurrent {
			effect.Mode = ShareUpdateExclusive
			effect.Reason = "concurrent index rebuild: two scans, waits out open transactions"
		}

		return Analysis{Effects: []LockEffect{effect}, NoTx: concurrent}

	default:
		return Analysis{NoTx: true}
	}
}

// refreshMatView covers REFRESH MATERIALIZED VIEW. The plain form swaps the
// contents out under ACCESS EXCLUSIVE; the concurrent form merges by DML
// under EXCLUSIVE, which keeps reads flowing. Both run inside a transaction
// block, which the lock matrix confirmed against a live server.
func refreshMatView(stmt *pgquery.RefreshMatViewStmt) Analysis {
	view := relationOf(stmt.GetRelation())

	if stmt.GetConcurrent() {
		effect := LockEffect{
			Relation: view,
			Mode:     Exclusive,
			Duration: Scan,
			Reason:   "concurrent refresh diffs and merges, blocking writes only",
		}

		return Analysis{Effects: []LockEffect{effect}}
	}

	effect := LockEffect{
		Relation: view,
		Mode:     AccessExclusive,
		Duration: Rewrite,
		Reason:   "refresh replaces the contents, blocking reads until done",
	}

	return Analysis{Effects: []LockEffect{effect}}
}

// lockTable covers LOCK TABLE. The parse tree carries the server's numeric
// lock level, which is the same numbering LockMode uses.
func lockTable(stmt *pgquery.LockStmt) Analysis {
	analysis := Analysis{}

	for _, relation := range stmt.GetRelations() {
		analysis.Effects = append(analysis.Effects, LockEffect{
			Relation: relationOf(relation.GetRangeVar()),
			Mode:     LockMode(stmt.GetMode()),
			Duration: Instant,
			Reason:   "explicit lock, held to the end of the transaction",
		})
	}

	return analysis
}

// createTable covers CREATE TABLE. The new table is of no interest, but an
// inline foreign key locks the referenced table, and creating a partition
// locks the parent.
func createTable(stmt *pgquery.CreateStmt) Analysis {
	analysis := Analysis{}

	if stmt.GetPartbound() != nil {
		for _, parent := range stmt.GetInhRelations() {
			analysis.Effects = append(analysis.Effects, LockEffect{
				Relation: relationOf(parent.GetRangeVar()),
				Mode:     AccessExclusive,
				Duration: Instant,
				Implicit: true,
				Reason:   "creating a partition locks the parent",
			})
		}
	}

	for _, element := range stmt.GetTableElts() {
		for _, constraint := range foreignKeys(element) {
			analysis.Effects = append(analysis.Effects, LockEffect{
				Relation: relationOf(constraint.GetPktable()),
				Mode:     ShareRowExclusive,
				Duration: Instant,
				Implicit: true,
				Reason:   "foreign key locks the referenced table",
			})
		}
	}

	return analysis
}

// foreignKeys collects the foreign key constraints of one table element,
// whether spelt at table level or inline on a column.
func foreignKeys(element *pgquery.Node) []*pgquery.Constraint {
	if constraint := element.GetConstraint(); constraint != nil {
		if constraint.GetContype() == pgquery.ConstrType_CONSTR_FOREIGN {
			return []*pgquery.Constraint{constraint}
		}

		return nil
	}

	column := element.GetColumnDef()
	if column == nil {
		return nil
	}

	keys := make([]*pgquery.Constraint, 0, len(column.GetConstraints()))

	for _, node := range column.GetConstraints() {
		constraint := node.GetConstraint()
		if constraint.GetContype() == pgquery.ConstrType_CONSTR_FOREIGN {
			keys = append(keys, constraint)
		}
	}

	return keys
}

// selectEffects covers SELECT, including set operations and locking clauses.
func selectEffects(sel *pgquery.SelectStmt) []LockEffect {
	if sel == nil {
		return nil
	}

	if sel.GetOp() != pgquery.SetOperation_SETOP_NONE {
		return append(selectEffects(sel.GetLarg()), selectEffects(sel.GetRarg())...)
	}

	effects := readEffects(sel.GetFromClause())

	for _, node := range sel.GetLockingClause() {
		effects = append(effects, lockingEffects(node.GetLockingClause(), sel.GetFromClause())...)
	}

	return effects
}

// readEffects turns a FROM clause into ACCESS SHARE effects.
func readEffects(from []*pgquery.Node) []LockEffect {
	effects := make([]LockEffect, 0, len(from))

	for _, rel := range fromRelations(from) {
		effects = append(effects, LockEffect{
			Relation: rel,
			Mode:     AccessShare,
			Duration: Scan,
			Reason:   "read",
		})
	}

	return effects
}

// lockingEffects turns FOR UPDATE and its variants into ROW SHARE effects on
// the named relations, or on every FROM relation when none is named.
func lockingEffects(clause *pgquery.LockingClause, from []*pgquery.Node) []LockEffect {
	locked := clause.GetLockedRels()

	var relations []Relation
	if len(locked) == 0 {
		relations = fromRelations(from)
	}

	for _, node := range locked {
		relations = append(relations, relationOf(node.GetRangeVar()))
	}

	effects := make([]LockEffect, 0, len(relations))

	for _, rel := range relations {
		effects = append(effects, LockEffect{
			Relation: rel,
			Mode:     RowShare,
			Duration: Scan,
			Reason:   "row locking read",
		})
	}

	return effects
}

// fromRelations walks a FROM clause down to its named relations.
func fromRelations(items []*pgquery.Node) []Relation {
	relations := make([]Relation, 0, len(items))

	for _, item := range items {
		switch {
		case item.GetRangeVar() != nil:
			relations = append(relations, relationOf(item.GetRangeVar()))

		case item.GetJoinExpr() != nil:
			join := item.GetJoinExpr()
			relations = append(relations,
				fromRelations([]*pgquery.Node{join.GetLarg(), join.GetRarg()})...)

		case item.GetRangeSubselect() != nil:
			sub := item.GetRangeSubselect().GetSubquery().GetSelectStmt()
			relations = append(relations, fromRelations(sub.GetFromClause())...)
		}
	}

	return relations
}

// insert covers INSERT, including the read side of INSERT ... SELECT.
func insert(stmt *pgquery.InsertStmt) Analysis {
	effects := []LockEffect{{
		Relation: relationOf(stmt.GetRelation()),
		Mode:     RowExclusive,
		Duration: Instant,
		Reason:   "write",
	}}

	effects = append(effects, selectEffects(stmt.GetSelectStmt().GetSelectStmt())...)

	return Analysis{Effects: effects}
}

// update covers UPDATE, including the tables it joins against.
func update(stmt *pgquery.UpdateStmt) Analysis {
	effects := []LockEffect{{
		Relation: relationOf(stmt.GetRelation()),
		Mode:     RowExclusive,
		Duration: Scan,
		Reason:   "write, scanning for matching rows",
	}}

	effects = append(effects, readEffects(stmt.GetFromClause())...)

	return Analysis{Effects: effects}
}

// deleteFrom covers DELETE, including the tables in its USING clause.
func deleteFrom(stmt *pgquery.DeleteStmt) Analysis {
	effects := []LockEffect{{
		Relation: relationOf(stmt.GetRelation()),
		Mode:     RowExclusive,
		Duration: Scan,
		Reason:   "write, scanning for matching rows",
	}}

	effects = append(effects, readEffects(stmt.GetUsingClause())...)

	return Analysis{Effects: effects}
}
