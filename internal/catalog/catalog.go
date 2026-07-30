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

// Package catalog reads the database's own account of its schema.
//
// The catalog is the authority on whether a step's work is done. The ledger
// records what the catalog cannot know, and is never consulted to decide
// whether work is required.
package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Querier is the subset of database/sql shared by *sql.DB, *sql.Conn and
// *sql.Tx. Step predicates run on a pinned connection; the fingerprint runs on
// whatever the caller has.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Index is an index's state.
//
// Ready and Valid are separate because a concurrent build sets them at
// different times: an interrupted CREATE INDEX CONCURRENTLY leaves an index
// that exists and is neither, and the planner ignores it while every write to
// the table still maintains it.
type Index struct {
	Exists bool
	Valid  bool
	Ready  bool
}

// Usable reports whether the index is finished and available to the planner.
func (i Index) Usable() bool {
	return i.Exists && i.Valid && i.Ready
}

// indexQuery resolves a name through the search path, as the statement that
// created it did, and returns nothing when no such index exists.
const indexQuery = `
SELECT i.indisvalid, i.indisready
  FROM pg_index i
  JOIN pg_class c ON c.oid = i.indexrelid
 WHERE c.oid = to_regclass($1)`

// LookupIndex reads the state of an index. A name that resolves to nothing is
// reported as absent rather than as an error.
func LookupIndex(ctx context.Context, q Querier, schema, name string) (Index, error) {
	ident := QualifiedIdent(schema, name)

	var index Index

	err := q.QueryRowContext(ctx, indexQuery, ident).Scan(&index.Valid, &index.Ready)

	if errors.Is(err, sql.ErrNoRows) {
		return Index{}, nil
	}

	if err != nil {
		return Index{}, fmt.Errorf("look up index %s: %w", ident, err)
	}

	index.Exists = true

	return index, nil
}

// QualifiedIdent renders a possibly schema-qualified identifier, quoted so that
// a name needing quotes resolves to the object the migration meant.
func QualifiedIdent(schema, name string) string {
	if schema == "" {
		return quoteIdent(name)
	}

	return quoteIdent(schema) + "." + quoteIdent(name)
}

// quoteIdent quotes a single SQL identifier.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// columnQuery reports whether a column is present on a relation.
//
// attisdropped covers a column that was dropped: Postgres keeps the row and
// marks it, so a plain name match would report a dropped column as present.
// The attnum bound excludes the system columns every relation has.
const columnQuery = `
SELECT EXISTS (
  SELECT 1
    FROM pg_attribute
   WHERE attrelid = to_regclass($1)
     AND attname = $2
     AND attnum > 0
     AND NOT attisdropped)`

// LookupColumn reports whether a column exists on a relation. A relation that
// is not there has no columns rather than being an error.
func LookupColumn(ctx context.Context, q Querier, schema, table, column string) (bool, error) {
	ident := QualifiedIdent(schema, table)

	var exists bool

	if err := q.QueryRowContext(ctx, columnQuery, ident, column).Scan(&exists); err != nil {
		return false, fmt.Errorf("look up column %s.%s: %w", ident, column, err)
	}

	return exists, nil
}

// Constraint is a constraint's state.
//
// Existence and validation are separate because they are reached separately: a
// constraint added NOT VALID exists and is not validated, and validating it is
// a second step that runs without holding the table against writers.
type Constraint struct {
	Exists    bool
	Validated bool
}

const constraintQuery = `
SELECT convalidated
  FROM pg_constraint
 WHERE conrelid = to_regclass($1) AND conname = $2`

// LookupConstraint reads the state of a constraint on a relation.
func LookupConstraint(ctx context.Context, q Querier, schema, table, name string) (Constraint, error) {
	ident := QualifiedIdent(schema, table)

	var constraint Constraint

	err := q.QueryRowContext(ctx, constraintQuery, ident, name).Scan(&constraint.Validated)

	if errors.Is(err, sql.ErrNoRows) {
		return Constraint{}, nil
	}

	if err != nil {
		return Constraint{}, fmt.Errorf("look up constraint %s on %s: %w", name, ident, err)
	}

	constraint.Exists = true

	return constraint, nil
}

const relationQuery = `SELECT to_regclass($1) IS NOT NULL`

// LookupRelation reports whether a relation exists.
func LookupRelation(ctx context.Context, q Querier, schema, name string) (bool, error) {
	ident := QualifiedIdent(schema, name)

	var exists bool

	if err := q.QueryRowContext(ctx, relationQuery, ident).Scan(&exists); err != nil {
		return false, fmt.Errorf("look up relation %s: %w", ident, err)
	}

	return exists, nil
}

// enumLabelQuery reports whether an enum carries a label. to_regtype resolves
// the type through the search path, and yields nothing for a type that is not
// there, which is an absent label rather than an error.
const enumLabelQuery = `
SELECT EXISTS (
  SELECT 1
    FROM pg_enum
   WHERE enumtypid = to_regtype($1)
     AND enumlabel = $2)`

// LookupEnumLabel reports whether an enum type carries a label.
func LookupEnumLabel(ctx context.Context, q Querier, schema, name, label string) (bool, error) {
	ident := QualifiedIdent(schema, name)

	var exists bool

	if err := q.QueryRowContext(ctx, enumLabelQuery, ident, label).Scan(&exists); err != nil {
		return false, fmt.Errorf("look up label %q of enum %s: %w", label, ident, err)
	}

	return exists, nil
}
