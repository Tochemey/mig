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
