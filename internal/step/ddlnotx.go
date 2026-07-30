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

package step

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/tochemey/mig/internal/catalog"
	"github.com/tochemey/mig/internal/crash"
	"github.com/tochemey/mig/internal/parse"
)

// Predicate reports whether a step's work is already present in the catalog.
type Predicate func(ctx context.Context, q catalog.Querier) (bool, error)

// DDLNoTx is a step that runs outside a transaction.
//
// Its ledger row cannot commit with its work, so it carries a predicate that
// recognises the finished work in the catalog and a repair that clears a
// partial one.
type DDLNoTx struct {
	meta       Meta
	statements []parse.Statement
	satisfied  Predicate
}

// NewDDLNoTx builds a non-transactional step from its classified statements.
//
// The predicate is required: without one, a step that cannot be wrapped in a
// transaction has no way to tell finished work from unstarted work, and cannot
// converge. Refusing here is the point.
func NewDDLNoTx(meta Meta, statements []parse.Statement, satisfied Predicate) (*DDLNoTx, error) {
	if satisfied == nil {
		return nil, fmt.Errorf("step %q: %w", meta.Name, ErrNoPredicate)
	}

	return &DDLNoTx{meta: meta, statements: statements, satisfied: satisfied}, nil
}

// Meta describes the step.
func (s *DDLNoTx) Meta() Meta {
	return s.meta
}

// Satisfied consults the catalog, and only the catalog.
func (s *DDLNoTx) Satisfied(ctx context.Context, conn *sql.Conn) (bool, error) {
	return s.satisfied(ctx, conn)
}

// Apply runs the statements on the pinned connection.
func (s *DDLNoTx) Apply(ctx context.Context, conn *sql.Conn) error {
	for _, stmt := range s.statements {
		if _, err := conn.ExecContext(ctx, stmt.SQL); err != nil {
			return fmt.Errorf("step %q: %w", s.meta.Name, err)
		}
	}

	return nil
}

// Repair clears the partial state a killed statement can leave behind.
//
// Only a concurrent index build leaves one: an index that exists and is not
// valid, which the planner ignores while every write to the table still
// maintains it. Postgres will not resume such a build, so it has to be dropped
// before the step can run again.
//
// The repair is itself interruptible, so every statement it issues is
// idempotent — IF EXISTS rather than a bare DROP — and it re-reads the catalog
// rather than trusting what a previous call believed.
func (s *DDLNoTx) Repair(ctx context.Context, conn *sql.Conn) error {
	for _, stmt := range s.statements {
		if stmt.Kind != parse.KindCreateIndex {
			continue
		}

		if err := dropInvalidIndex(ctx, conn, stmt.Target); err != nil {
			return fmt.Errorf("repair step %q: %w", s.meta.Name, err)
		}
	}

	return nil
}

// dropInvalidIndex removes an index left behind by an interrupted build. A
// valid index is left alone: it is the finished work.
func dropInvalidIndex(ctx context.Context, conn *sql.Conn, target parse.Target) error {
	index, err := catalog.LookupIndex(ctx, conn, target.Schema, target.Name)
	if err != nil {
		return err
	}

	if !index.Exists || index.Usable() {
		return nil
	}

	crash.At(crash.DuringRepair)

	ident := catalog.QualifiedIdent(target.Schema, target.Name)

	concurrently := ""
	if target.Concurrent {
		concurrently = "CONCURRENTLY "
	}

	//nolint:gosec // G201: identifiers cannot be bound as parameters; QualifiedIdent quotes them.
	if _, err := conn.ExecContext(ctx, "DROP INDEX "+concurrently+"IF EXISTS "+ident); err != nil {
		return fmt.Errorf("drop invalid index %s: %w", ident, err)
	}

	return nil
}
