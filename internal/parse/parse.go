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

	// KindOther is anything not yet classified. It carries no inferred
	// predicate.
	KindOther Kind = "other"
)

// Index names an index and, for creation, the table it covers.
type Index struct {
	Schema     string
	Name       string
	Table      string
	Concurrent bool
}

// Qualified renders the index name for an error message, schema-qualified when
// the statement said so.
func (i Index) Qualified() string {
	if i.Schema == "" {
		return i.Name
	}

	return i.Schema + "." + i.Name
}

// Statement is one classified SQL statement.
type Statement struct {
	SQL  string
	Kind Kind

	// Index is populated for [KindCreateIndex] and [KindDropIndex].
	Index Index
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

	stmt := Statement{SQL: sql, Kind: KindOther}

	// A single statement went in, so at most one comes out. Anything else is a
	// split that disagreed with the parser.
	if len(tree.Stmts) != 1 {
		return stmt, nil
	}

	node := tree.Stmts[0].GetStmt()

	if index := node.GetIndexStmt(); index != nil {
		return createIndex(sql, index), nil
	}

	if drop := node.GetDropStmt(); drop != nil && drop.GetRemoveType() == pgquery.ObjectType_OBJECT_INDEX {
		return dropIndex(sql, drop)
	}

	return stmt, nil
}

// createIndex describes a CREATE INDEX statement.
func createIndex(sql string, stmt *pgquery.IndexStmt) Statement {
	index := Index{
		Name:       stmt.GetIdxname(),
		Concurrent: stmt.GetConcurrent(),
	}

	if relation := stmt.GetRelation(); relation != nil {
		index.Schema = relation.GetSchemaname()
		index.Table = relation.GetRelname()
	}

	return Statement{SQL: sql, Kind: KindCreateIndex, Index: index}
}

// dropIndex describes a DROP INDEX statement.
//
// DROP INDEX accepts several names at once, and the caller needs one index to
// reason about, so a multi-target drop stays unclassified.
func dropIndex(sql string, stmt *pgquery.DropStmt) (Statement, error) {
	objects := stmt.GetObjects()
	if len(objects) != 1 {
		return Statement{SQL: sql, Kind: KindOther}, nil
	}

	parts, err := nameParts(objects[0])
	if err != nil {
		return Statement{}, fmt.Errorf("read index name in %q: %w", truncate(sql), err)
	}

	index := Index{Concurrent: stmt.GetConcurrent()}

	switch len(parts) {
	case 1:
		index.Name = parts[0]
	case 2:
		index.Schema, index.Name = parts[0], parts[1]
	default:
		return Statement{SQL: sql, Kind: KindOther}, nil
	}

	return Statement{SQL: sql, Kind: KindDropIndex, Index: index}, nil
}

// nameParts reads a possibly schema-qualified object name.
func nameParts(object *pgquery.Node) ([]string, error) {
	list := object.GetList()
	if list == nil {
		return nil, fmt.Errorf("object name is %T, not a name list", object.GetNode())
	}

	parts := make([]string, 0, len(list.GetItems()))

	for _, item := range list.GetItems() {
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
