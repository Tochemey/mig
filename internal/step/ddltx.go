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

	"github.com/tochemey/mig/internal/parse"
)

// DDLTx is a step that runs inside a transaction shared with its ledger write.
//
// Unlike a non-transactional step it needs no predicate, because there is no
// window to reconcile: either the DDL and the ledger row both committed or
// neither did.
type DDLTx struct {
	meta       Meta
	statements []parse.Statement
	satisfied  Predicate
}

// NewDDLTx builds a transactional step. The predicate may be nil.
func NewDDLTx(meta Meta, statements []parse.Statement, satisfied Predicate) *DDLTx {
	return &DDLTx{meta: meta, statements: statements, satisfied: satisfied}
}

// Meta describes the step.
func (s *DDLTx) Meta() Meta {
	return s.meta
}

// Satisfied consults the catalog when a predicate could be derived, and
// otherwise reports false.
//
// Reporting false for work that is in fact done is safe here and nowhere else:
// the executor falls back to the ledger for transactional steps, and a ledger
// row that says succeeded committed with the DDL it describes.
func (s *DDLTx) Satisfied(ctx context.Context, conn *sql.Conn) (bool, error) {
	if s.satisfied == nil {
		return false, nil
	}

	return s.satisfied(ctx, conn)
}

// Repair does nothing. A transactional step cannot be left part-way through.
func (s *DDLTx) Repair(context.Context, *sql.Conn) error {
	return nil
}

// ApplyTx executes the statements in the caller's transaction.
func (s *DDLTx) ApplyTx(ctx context.Context, tx *sql.Tx) error {
	for _, stmt := range s.statements {
		if _, err := tx.ExecContext(ctx, stmt.SQL); err != nil {
			return fmt.Errorf("step %q: %w", s.meta.Name, err)
		}
	}

	return nil
}
