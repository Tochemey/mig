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

// Package history reads the whole plan once and answers what the rules cannot
// see from one statement: which relations the plan itself creates, which
// index belongs to which table, what a view reads from, what type a column
// had before a statement changes it, and whether the lineage grants at all.
//
// Relations are matched by bare name, as the cross-statement rules match
// them. Every lookup is nil-safe, answering as it would for a plan that
// created nothing, so a caller without a history degrades to the
// conservative answer rather than a special case.
package history

import (
	"strings"

	pgquery "github.com/pganalyze/pg_query_go/v6"

	"github.com/tochemey/mig/internal/plan"
)

// pos orders every statement of the plan: by migration, then step, then
// statement.
type pos struct {
	migration, step, stmt int
}

// before reports strict statement order.
func (p pos) before(q pos) bool {
	if p.migration != q.migration {
		return p.migration < q.migration
	}

	if p.step != q.step {
		return p.step < q.step
	}

	return p.stmt < q.stmt
}

// relEvent is one creation or drop of a relation.
type relEvent struct {
	at      pos
	created bool
}

// typeEvent is one point where a column's type became known: a CREATE TABLE
// column, an ADD COLUMN, or an ALTER COLUMN TYPE.
type typeEvent struct {
	at   pos
	spec TypeSpec
}

// TypeSpec is a column type as a statement spells it: the bare type name and
// its modifiers, which for varchar(255) are "varchar" and [255].
type TypeSpec struct {
	Name string
	Mods []int32
}

// History is what one pass over the plan recorded.
type History struct {
	files     map[string]int
	relations map[string][]relEvent
	parents   map[string]string
	views     map[string][]string
	types     map[string][]typeEvent
	grants    bool
}

// Build walks every statement of the plan in order. A statement that does not
// parse records nothing: the plan parsed each one before, so a failure here is
// a hand-built plan, and the conservative answer is the empty one.
func Build(p *plan.Plan) *History {
	h := &History{
		files:     make(map[string]int, len(p.Migrations)),
		relations: make(map[string][]relEvent),
		parents:   make(map[string]string),
		views:     make(map[string][]string),
		types:     make(map[string][]typeEvent),
	}

	for migIndex := range p.Migrations {
		migration := &p.Migrations[migIndex]
		h.files[migration.File] = migIndex

		for stepIndex, s := range migration.Steps {
			for stmtIndex, statement := range s.Statements {
				tree, err := pgquery.Parse(statement.SQL)
				if err != nil {
					continue
				}

				at := pos{migration: migIndex, step: stepIndex, stmt: stmtIndex}

				for _, raw := range tree.GetStmts() {
					h.record(raw.GetStmt(), at)
				}
			}
		}
	}

	return h
}

// FreshAt reports whether, at the given statement, the relation is this
// migration's own fresh creation: the latest create or drop of it in the same
// file, at or before the statement, is a create. Such a relation is empty and
// invisible to traffic, so a lock on it blocks nobody. A drop followed by a
// recreation splits the name in two: the drop acts on the old incarnation,
// everything after the create on the new one.
func (h *History) FreshAt(relation, file string, step, stmt int) bool {
	if h == nil {
		return false
	}

	at := pos{migration: h.files[file], step: step, stmt: stmt}
	fresh := false

	// A create counts at its own statement, so the creation's own lock is on
	// the fresh relation. A drop counts only before the statement asking: the
	// drop's own lock lands on the incarnation that existed until then.
	for _, event := range h.relations[strings.ToLower(relation)] {
		if event.at.migration != at.migration {
			continue
		}

		if event.created && !at.before(event.at) {
			fresh = true
		} else if !event.created && event.at.before(at) {
			fresh = false
		}
	}

	return fresh
}

// EverCreated reports whether any migration of the plan creates the relation.
// A relation dropped IF EXISTS and never created anywhere is reconciliation
// of a schema this plan never built, expected absent on a database the plan
// did build.
func (h *History) EverCreated(relation string) bool {
	if h == nil {
		return false
	}

	for _, event := range h.relations[strings.ToLower(relation)] {
		if event.created {
			return true
		}
	}

	return false
}

// Parent names the table whose traffic a lock on the relation stops: the
// table an index of the plan was built on, or the one whose column defaults
// to a sequence of the plan.
func (h *History) Parent(relation string) (string, bool) {
	if h == nil {
		return "", false
	}

	parent, ok := h.parents[strings.ToLower(relation)]

	return parent, ok
}

// ViewBases names the relations a view of the plan reads from, and nothing
// for a view the plan never defines.
func (h *History) ViewBases(view string) []string {
	if h == nil {
		return nil
	}

	return h.views[strings.ToLower(view)]
}

// HasGrants reports whether any statement of the plan grants anything. A
// lineage that never grants is not managing a restricted application role in
// its migrations, and a rule about that role has nothing to protect.
func (h *History) HasGrants() bool {
	return h != nil && h.grants
}

// ColumnTypeBefore reports the column's type as of just before the given
// statement, from the plan's own DDL, and false when the plan never spelled
// it out.
func (h *History) ColumnTypeBefore(table, column, file string, step, stmt int) (TypeSpec, bool) {
	if h == nil {
		return TypeSpec{}, false
	}

	at := pos{migration: h.files[file], step: step, stmt: stmt}

	var (
		spec  TypeSpec
		known bool
	)

	for _, event := range h.types[columnKey(table, column)] {
		if event.at.before(at) {
			spec, known = event.spec, true
		}
	}

	return spec, known
}

// ColumnAddedInFile reports whether the same migration gives the column to
// the table before the given statement, by creating the table with it or by
// adding it. A rename of such a column changes a name nothing outside this
// migration has ever seen.
func (h *History) ColumnAddedInFile(table, column, file string, step, stmt int) bool {
	if h == nil {
		return false
	}

	at := pos{migration: h.files[file], step: step, stmt: stmt}

	for _, event := range h.types[columnKey(table, column)] {
		if event.at.migration == at.migration && event.at.before(at) {
			return true
		}
	}

	return false
}

// record files whatever the statement tells the history.
func (h *History) record(stmt *pgquery.Node, at pos) {
	switch {
	case stmt.GetCreateStmt() != nil:
		create := stmt.GetCreateStmt()
		table := strings.ToLower(create.GetRelation().GetRelname())
		h.creation(table, at)

		for _, element := range create.GetTableElts() {
			if def := element.GetColumnDef(); def != nil {
				h.typed(table, def, at)
			}
		}

	case stmt.GetIndexStmt() != nil:
		index := stmt.GetIndexStmt()
		name := strings.ToLower(index.GetIdxname())
		h.creation(name, at)
		h.parents[name] = strings.ToLower(index.GetRelation().GetRelname())

	case stmt.GetViewStmt() != nil:
		view := stmt.GetViewStmt()
		name := strings.ToLower(view.GetView().GetRelname())
		h.creation(name, at)
		h.views[name] = bases(view.GetQuery().GetSelectStmt())

	case stmt.GetCreateSeqStmt() != nil:
		h.creation(strings.ToLower(stmt.GetCreateSeqStmt().GetSequence().GetRelname()), at)

	case stmt.GetCreateTableAsStmt() != nil:
		h.creation(strings.ToLower(stmt.GetCreateTableAsStmt().GetInto().GetRel().GetRelname()), at)

	case stmt.GetDropStmt() != nil:
		drop := stmt.GetDropStmt()
		if !droppable[drop.GetRemoveType()] {
			return
		}

		for _, object := range drop.GetObjects() {
			name := lastName(object)
			h.relations[name] = append(h.relations[name], relEvent{at: at})
		}

	case stmt.GetAlterTableStmt() != nil:
		alter := stmt.GetAlterTableStmt()
		table := strings.ToLower(alter.GetRelation().GetRelname())

		for _, node := range alter.GetCmds() {
			cmd := node.GetAlterTableCmd()

			switch cmd.GetSubtype() {
			case pgquery.AlterTableType_AT_AddColumn, pgquery.AlterTableType_AT_AlterColumnType:
				if def := cmd.GetDef().GetColumnDef(); def != nil {
					h.typed(table, def, at, cmd.GetName())
				}
			}
		}

	case stmt.GetGrantStmt() != nil:
		if stmt.GetGrantStmt().GetIsGrant() {
			h.grants = true
		}
	}
}

// creation files one create event.
func (h *History) creation(name string, at pos) {
	h.relations[name] = append(h.relations[name], relEvent{at: at, created: true})
}

// typed files a column's type, and the sequence its default draws from, if
// any: a table owning a sequence through nextval is the traffic a lock on
// that sequence stops. ADD COLUMN names the column on its definition; ALTER
// COLUMN TYPE names it on the command and carries only the type, which is
// what the override argument is for.
func (h *History) typed(table string, def *pgquery.ColumnDef, at pos, override ...string) {
	if sequence := defaultSequence(def); sequence != "" {
		h.parents[sequence] = table
	}

	column := def.GetColname()
	if column == "" {
		column = override[0]
	}

	spec, _ := SpecOf(def.GetTypeName())
	key := columnKey(table, column)
	h.types[key] = append(h.types[key], typeEvent{at: at, spec: spec})
}

// columnKey joins a table and column into one lookup key.
func columnKey(table, column string) string {
	return strings.ToLower(table) + "." + strings.ToLower(column)
}

// SpecOf reads a TypeName into a spec: the last name part, since the parser
// qualifies the built-ins as pg_catalog, and the integer modifiers. It is
// exported because a rule comparing a statement's new type against the
// history must spell both the same way.
func SpecOf(name *pgquery.TypeName) (TypeSpec, bool) {
	parts := name.GetNames()
	if len(parts) == 0 {
		return TypeSpec{}, false
	}

	spec := TypeSpec{Name: strings.ToLower(parts[len(parts)-1].GetString_().GetSval())}

	for _, mod := range name.GetTypmods() {
		spec.Mods = append(spec.Mods, mod.GetAConst().GetIval().GetIval())
	}

	return spec, true
}

// droppable is the set of DROP targets that are relations. A trigger or a
// policy dropped by name must not be mistaken for the table sharing it.
var droppable = map[pgquery.ObjectType]bool{
	pgquery.ObjectType_OBJECT_TABLE:    true,
	pgquery.ObjectType_OBJECT_INDEX:    true,
	pgquery.ObjectType_OBJECT_VIEW:     true,
	pgquery.ObjectType_OBJECT_MATVIEW:  true,
	pgquery.ObjectType_OBJECT_SEQUENCE: true,
}

// lastName reads the bare name off a DROP target's name list, which the
// grammar guarantees is not empty.
func lastName(object *pgquery.Node) string {
	items := object.GetList().GetItems()

	return strings.ToLower(items[len(items)-1].GetString_().GetSval())
}

// bases collects the relations a SELECT reads from, joins and WITH-clause
// queries included. A CTE's name lands in the list too, harmlessly: it names
// no real relation, so nothing ever matches it.
func bases(sel *pgquery.SelectStmt) []string {
	var found []string

	for _, node := range sel.GetFromClause() {
		found = append(found, fromNode(node)...)
	}

	for _, cte := range sel.GetWithClause().GetCtes() {
		if inner := cte.GetCommonTableExpr().GetCtequery().GetSelectStmt(); inner != nil {
			found = append(found, bases(inner)...)
		}
	}

	return found
}

// defaultSequence names the sequence a column's DEFAULT nextval draws from,
// and nothing when the default is anything else.
func defaultSequence(def *pgquery.ColumnDef) string {
	for _, node := range def.GetConstraints() {
		constraint := node.GetConstraint()
		if constraint.GetContype() != pgquery.ConstrType_CONSTR_DEFAULT {
			continue
		}

		call := constraint.GetRawExpr().GetFuncCall()
		if call == nil || len(call.GetArgs()) == 0 {
			continue
		}

		name := call.GetFuncname()[len(call.GetFuncname())-1].GetString_().GetSval()
		if !strings.EqualFold(name, "nextval") {
			continue
		}

		// The argument is a string, usually cast to regclass on top.
		arg := call.GetArgs()[0]
		if cast := arg.GetTypeCast(); cast != nil {
			arg = cast.GetArg()
		}

		if sequence := arg.GetAConst().GetSval().GetSval(); sequence != "" {
			return strings.ToLower(sequence)
		}
	}

	return ""
}

// fromNode walks one FROM item down to its range variables.
func fromNode(node *pgquery.Node) []string {
	switch {
	case node.GetRangeVar() != nil:
		return []string{strings.ToLower(node.GetRangeVar().GetRelname())}

	case node.GetJoinExpr() != nil:
		join := node.GetJoinExpr()

		return append(fromNode(join.GetLarg()), fromNode(join.GetRarg())...)

	default:
		return nil
	}
}
