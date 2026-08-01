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

// Package stats reads what only a live catalog knows: how big a table is and
// what its primary key is.
//
// It is read once, before any rule runs, so that a rule is a pure function of
// the statement and this snapshot. Nothing here is consulted offline, where
// the snapshot is nil and every size-dependent hazard stays a warning.
package stats

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/tochemey/mig/internal/catalog"
	"github.com/tochemey/mig/internal/lint/lockmodel"
)

// sizeQuery reads the planner's own estimates. reltuples is -1 on a table
// that has never been analysed, which is reported as unknown rather than as
// an empty table.
const sizeQuery = `
SELECT GREATEST(c.reltuples, 0)::bigint, pg_total_relation_size(c.oid)
  FROM pg_class c
 WHERE c.oid = to_regclass($1)`

// keyQuery reads the primary key's columns in key order.
const keyQuery = `
SELECT a.attname
  FROM pg_constraint con
  JOIN LATERAL unnest(con.conkey) WITH ORDINALITY AS k(attnum, ord) ON true
  JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = k.attnum
 WHERE con.conrelid = to_regclass($1) AND con.contype = 'p'
 ORDER BY k.ord`

// versionQuery reads the running server's version number.
const versionQuery = `SHOW server_version_num`

// Table is the catalog's account of one relation.
type Table struct {
	// Exists is false for a relation the catalog does not have, which is what
	// a table the migration is about to create looks like.
	Exists bool

	// Rows is the planner's own row estimate and Bytes the total size,
	// indexes and toast included. Both are estimates, and Rows is zero on a
	// table that has never been analysed.
	Rows  int64
	Bytes int64

	// PrimaryKey is the key's columns in order, empty when there is no
	// primary key. A generated backfill pages by it.
	PrimaryKey []string
}

// Snapshot is what the catalog said about the relations one plan names.
type Snapshot struct {
	tables map[lockmodel.Relation]Table

	// throughput is what this server was measured doing, and is zero when no
	// probe ran.
	throughput Throughput
}

// Table returns what was read about a relation. A relation nobody asked about
// and a relation the catalog does not have look the same on purpose: neither
// supports a claim about size.
func (s *Snapshot) Table(relation lockmodel.Relation) Table {
	if s == nil {
		return Table{}
	}

	return s.tables[relation]
}

// Throughput returns what the server was measured doing, zero when no probe
// ran.
func (s *Snapshot) Throughput() Throughput {
	if s == nil {
		return Throughput{}
	}

	return s.throughput
}

// Of builds a snapshot from sizes already in hand, for a caller whose figures
// come from somewhere other than this connection, and for the tests of
// everything downstream, which are about what is done with a size rather than
// about reading one.
func Of(tables map[lockmodel.Relation]Table) *Snapshot {
	return &Snapshot{tables: tables}
}

// WithThroughput returns the snapshot carrying a calibration result, so that
// the caller decides whether measuring the server was appropriate.
func (s *Snapshot) WithThroughput(t Throughput) *Snapshot {
	s.throughput = t

	return s
}

// Collect reads every named relation in one pass. A relation the catalog does
// not have is recorded as absent rather than dropped, so a later lookup can
// tell "not in the database" from "never asked".
func Collect(ctx context.Context, q catalog.Querier, relations []lockmodel.Relation) (*Snapshot, error) {
	snapshot := &Snapshot{tables: make(map[lockmodel.Relation]Table, len(relations))}

	for _, relation := range relations {
		if _, done := snapshot.tables[relation]; done {
			continue
		}

		table, err := readTable(ctx, q, relation)
		if err != nil {
			return nil, err
		}

		snapshot.tables[relation] = table
	}

	return snapshot, nil
}

// readTable reads one relation, or reports it absent.
func readTable(ctx context.Context, q catalog.Querier, relation lockmodel.Relation) (Table, error) {
	ident := catalog.QualifiedIdent(relation.Schema, relation.Name)

	var table Table

	err := q.QueryRowContext(ctx, sizeQuery, ident).Scan(&table.Rows, &table.Bytes)

	if errors.Is(err, sql.ErrNoRows) {
		return Table{}, nil
	}

	if err != nil {
		return Table{}, fmt.Errorf("read size of %s: %w", ident, err)
	}

	table.Exists = true

	if table.PrimaryKey, err = readKey(ctx, q, ident); err != nil {
		return Table{}, err
	}

	return table, nil
}

// readKey reads the primary key's columns.
//
// The close is joined onto whatever the read produced rather than deferred
// and dropped: it is the one place that reports what the iteration could not,
// and it runs on the failing paths as well as the finishing one.
func readKey(ctx context.Context, q catalog.Querier, ident string) ([]string, error) {
	rows, err := q.QueryContext(ctx, keyQuery, ident)
	if err != nil {
		return nil, fmt.Errorf("read primary key of %s: %w", ident, err)
	}

	key, err := scanKey(rows)

	if err := errors.Join(err, rows.Close()); err != nil {
		return nil, fmt.Errorf("read primary key of %s: %w", ident, err)
	}

	return key, nil
}

// scanKey reads the column names out of one answer to [keyQuery].
func scanKey(rows *sql.Rows) ([]string, error) {
	var key []string

	for rows.Next() {
		var column string

		if err := rows.Scan(&column); err != nil {
			return nil, err
		}

		key = append(key, column)
	}

	return key, rows.Err()
}

// ServerMajor reads the major version of the connected server, which is what
// connected mode lints against instead of a flag.
func ServerMajor(ctx context.Context, q catalog.Querier) (int, error) {
	var version int

	if err := q.QueryRowContext(ctx, versionQuery).Scan(&version); err != nil {
		return 0, fmt.Errorf("read server version: %w", err)
	}

	return version / 10000, nil
}
