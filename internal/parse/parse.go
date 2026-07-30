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

// Package parse classifies migration SQL using the real Postgres grammar.
//
// Classification drives predicate inference, so it has to agree with the server
// about what a statement does. A regular expression would disagree on quoted
// identifiers, comments, dollar quoting and multi-line statements, and would do
// so silently.
package parse

import (
	"crypto/sha256"
	"fmt"
	"strings"

	pgquery "github.com/pganalyze/pg_query_go/v6"
)

// Kind classifies a statement.
type Kind string

const (
	// KindCreateIndex is CREATE INDEX, with or without CONCURRENTLY.
	KindCreateIndex Kind = "create_index"

	// KindDropIndex is DROP INDEX, with or without CONCURRENTLY.
	KindDropIndex Kind = "drop_index"

	// KindAddColumn is ALTER TABLE ... ADD COLUMN.
	KindAddColumn Kind = "add_column"

	// KindDropColumn is ALTER TABLE ... DROP COLUMN.
	KindDropColumn Kind = "drop_column"

	// KindAddConstraint is ALTER TABLE ... ADD CONSTRAINT.
	KindAddConstraint Kind = "add_constraint"

	// KindValidateConstraint is ALTER TABLE ... VALIDATE CONSTRAINT.
	KindValidateConstraint Kind = "validate_constraint"

	// KindCreateTable is CREATE TABLE.
	KindCreateTable Kind = "create_table"

	// KindAddEnumValue is ALTER TYPE ... ADD VALUE.
	KindAddEnumValue Kind = "add_enum_value"

	// KindOther is anything not classified. It carries no inferred predicate.
	KindOther Kind = "other"
)

// Target names the object a statement acts on.
//
// Schema is empty when the statement did not qualify the name, in which case it
// is resolved through the search path exactly as the statement itself would
// resolve it.
type Target struct {
	Schema string

	// Name is the object the statement names: an index, a table or a type.
	Name string

	// Member is the column, constraint or enum label within Name, and is empty
	// when the statement acts on the object as a whole.
	Member string

	// On is the table an index covers. It plays no part in the predicate and is
	// carried for the sake of readable output.
	On string

	// Concurrent reports CONCURRENTLY.
	Concurrent bool
}

// Qualified renders the name for output and errors, schema-qualified when the
// statement said so.
func (t Target) Qualified() string {
	name := t.Name
	if t.Schema != "" {
		name = t.Schema + "." + t.Name
	}

	if t.Member == "" {
		return name
	}

	return name + "." + t.Member
}

// Statement is one classified SQL statement.
type Statement struct {
	SQL  string
	Kind Kind

	// Target is populated for every kind except [KindOther].
	Target Target
}

// Split breaks SQL into individual statements.
func Split(sql string) ([]string, error) {
	stmts, err := pgquery.SplitWithParser(sql, true)
	if err != nil {
		return nil, fmt.Errorf("split sql: %w", err)
	}

	out := make([]string, 0, len(stmts))

	for _, stmt := range stmts {
		if trimmed := strings.TrimSpace(stmt); trimmed != "" {
			out = append(out, trimmed)
		}
	}

	return out, nil
}

// Parse splits and classifies every statement in sql.
func Parse(sql string) ([]Statement, error) {
	texts, err := Split(sql)
	if err != nil {
		return nil, err
	}

	statements := make([]Statement, 0, len(texts))

	for _, text := range texts {
		stmt, err := classify(text)
		if err != nil {
			return nil, err
		}

		statements = append(statements, stmt)
	}

	return statements, nil
}

// classify parses a single statement and identifies what it does.
func classify(sql string) (Statement, error) {
	tree, err := pgquery.Parse(sql)
	if err != nil {
		return Statement{}, fmt.Errorf("parse %q: %w", truncate(sql), err)
	}

	other := Statement{SQL: sql, Kind: KindOther}

	// A single statement went in, so at most one comes out. Anything else is a
	// split that disagreed with the parser.
	if len(tree.Stmts) != 1 {
		return other, nil
	}

	node := tree.Stmts[0].GetStmt()

	switch {
	case node.GetIndexStmt() != nil:
		return createIndex(sql, node.GetIndexStmt()), nil

	case node.GetDropStmt() != nil:
		return dropIndex(sql, node.GetDropStmt())

	case node.GetAlterTableStmt() != nil:
		return alterTable(sql, node.GetAlterTableStmt()), nil

	case node.GetCreateStmt() != nil:
		return createTable(sql, node.GetCreateStmt()), nil

	case node.GetAlterEnumStmt() != nil:
		return addEnumValue(sql, node.GetAlterEnumStmt())

	default:
		return other, nil
	}
}

// createIndex describes a CREATE INDEX statement.
func createIndex(sql string, stmt *pgquery.IndexStmt) Statement {
	target := Target{
		Name:       stmt.GetIdxname(),
		Concurrent: stmt.GetConcurrent(),
	}

	if relation := stmt.GetRelation(); relation != nil {
		target.Schema = relation.GetSchemaname()
		target.On = relation.GetRelname()
	}

	return Statement{SQL: sql, Kind: KindCreateIndex, Target: target}
}

// dropIndex describes a DROP INDEX statement.
//
// DROP INDEX accepts several names at once, and the caller needs one object to
// reason about, so a multi-target drop stays unclassified.
func dropIndex(sql string, stmt *pgquery.DropStmt) (Statement, error) {
	other := Statement{SQL: sql, Kind: KindOther}

	if stmt.GetRemoveType() != pgquery.ObjectType_OBJECT_INDEX {
		return other, nil
	}

	objects := stmt.GetObjects()
	if len(objects) != 1 {
		return other, nil
	}

	parts, err := nameParts(objects[0])
	if err != nil {
		return Statement{}, fmt.Errorf("read index name in %q: %w", truncate(sql), err)
	}

	target := Target{Concurrent: stmt.GetConcurrent()}

	switch len(parts) {
	case 1:
		target.Name = parts[0]
	case 2:
		target.Schema, target.Name = parts[0], parts[1]
	default:
		return other, nil
	}

	return Statement{SQL: sql, Kind: KindDropIndex, Target: target}, nil
}

// alterTable describes the ALTER TABLE actions that carry a predicate.
//
// Only a single-action statement is classified. ALTER TABLE accepts a list, and
// a statement whose actions differ in kind has no single predicate to infer.
func alterTable(sql string, stmt *pgquery.AlterTableStmt) Statement {
	other := Statement{SQL: sql, Kind: KindOther}

	cmds := stmt.GetCmds()
	if len(cmds) != 1 {
		return other
	}

	cmd := cmds[0].GetAlterTableCmd()
	if cmd == nil {
		return other
	}

	target := Target{}

	if relation := stmt.GetRelation(); relation != nil {
		target.Schema = relation.GetSchemaname()
		target.Name = relation.GetRelname()
	}

	switch cmd.GetSubtype() {
	case pgquery.AlterTableType_AT_AddColumn:
		column := cmd.GetDef().GetColumnDef()
		if column == nil {
			return other
		}

		target.Member = column.GetColname()

		return Statement{SQL: sql, Kind: KindAddColumn, Target: target}

	case pgquery.AlterTableType_AT_DropColumn:
		target.Member = cmd.GetName()

		return Statement{SQL: sql, Kind: KindDropColumn, Target: target}

	case pgquery.AlterTableType_AT_AddConstraint:
		constraint := cmd.GetDef().GetConstraint()
		if constraint == nil || constraint.GetConname() == "" {
			// An unnamed constraint is named by the server, so there is nothing
			// to look for afterwards.
			return other
		}

		target.Member = constraint.GetConname()

		return Statement{SQL: sql, Kind: KindAddConstraint, Target: target}

	case pgquery.AlterTableType_AT_ValidateConstraint:
		target.Member = cmd.GetName()

		return Statement{SQL: sql, Kind: KindValidateConstraint, Target: target}

	default:
		return other
	}
}

// createTable describes a CREATE TABLE statement.
func createTable(sql string, stmt *pgquery.CreateStmt) Statement {
	relation := stmt.GetRelation()
	if relation == nil {
		return Statement{SQL: sql, Kind: KindOther}
	}

	target := Target{Schema: relation.GetSchemaname(), Name: relation.GetRelname()}

	return Statement{SQL: sql, Kind: KindCreateTable, Target: target}
}

// addEnumValue describes an ALTER TYPE ... ADD VALUE statement.
func addEnumValue(sql string, stmt *pgquery.AlterEnumStmt) (Statement, error) {
	other := Statement{SQL: sql, Kind: KindOther}

	// A statement that renames a label rather than adding one carries no
	// existence check worth inferring.
	if stmt.GetNewVal() == "" || stmt.GetOldVal() != "" {
		return other, nil
	}

	parts, err := nodeNames(stmt.GetTypeName())
	if err != nil {
		return Statement{}, fmt.Errorf("read type name in %q: %w", truncate(sql), err)
	}

	target := Target{Member: stmt.GetNewVal()}

	switch len(parts) {
	case 1:
		target.Name = parts[0]
	case 2:
		target.Schema, target.Name = parts[0], parts[1]
	default:
		return other, nil
	}

	return Statement{SQL: sql, Kind: KindAddEnumValue, Target: target}, nil
}

// nameParts reads a possibly schema-qualified object name.
func nameParts(object *pgquery.Node) ([]string, error) {
	list := object.GetList()
	if list == nil {
		return nil, fmt.Errorf("object name is %T, not a name list", object.GetNode())
	}

	return nodeNames(list.GetItems())
}

// nodeNames reads a list of string nodes.
func nodeNames(items []*pgquery.Node) ([]string, error) {
	parts := make([]string, 0, len(items))

	for _, item := range items {
		str := item.GetString_()
		if str == nil {
			return nil, fmt.Errorf("name part is %T, not a string", item.GetNode())
		}

		parts = append(parts, str.GetSval())
	}

	return parts, nil
}

// Checksum hashes the fingerprint of sql rather than its text, so reformatting
// a migration or editing its comments does not read as drift.
func Checksum(sql string) ([]byte, error) {
	fingerprint, err := pgquery.Fingerprint(sql)
	if err != nil {
		return nil, fmt.Errorf("fingerprint %q: %w", truncate(sql), err)
	}

	sum := sha256.Sum256([]byte(fingerprint))

	return sum[:], nil
}

// truncate shortens SQL for an error message.
func truncate(sql string) string {
	const limit = 60

	flat := strings.Join(strings.Fields(sql), " ")
	if len(flat) <= limit {
		return flat
	}

	return flat[:limit] + "..."
}
