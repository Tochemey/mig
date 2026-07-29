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

// Package step defines the unit of migration work.
//
// The interface split encodes the reconciliation rule. A transactional step
// commits its ledger row with its DDL, so no window exists between the work and
// the record of it. A non-transactional step cannot, so it must instead be able
// to recognise its own completed work in the catalog.
package step

import (
	"context"
	"database/sql"
	"errors"
)

// ErrNoPredicate reports a non-transactional step with no way to recognise its
// own finished work.
//
// Such a step cannot converge: after a crash there is no way to tell work that
// completed from work that never started. Refusing it at plan time is the
// alternative to marking the database dirty later.
var ErrNoPredicate = errors.New("non-transactional step has no predicate; add a satisfied: annotation")

// Kind is how a step must be executed.
type Kind string

const (
	// KindDDLTx runs inside a transaction shared with its ledger write.
	KindDDLTx Kind = "ddl_tx"

	// KindDDLNoTx runs outside any transaction, as CONCURRENTLY requires.
	KindDDLNoTx Kind = "ddl_notx"

	// KindBackfill runs long and checkpoints as it goes.
	KindBackfill Kind = "backfill"
)

// Meta describes a step independently of how it runs.
type Meta struct {
	Name     string
	Kind     Kind
	Checksum []byte
	SQL      string
}

// Step is implemented by every kind of step.
type Step interface {
	// Meta describes the step.
	Meta() Meta

	// Satisfied reports whether the work is already done, judged only by the
	// catalog. It must never consult the ledger, and it must be safe to call at
	// any time, including before the step has ever run.
	Satisfied(ctx context.Context, conn *sql.Conn) (bool, error)

	// Repair returns the database from a known-partial state to one Apply can
	// run against. It must be idempotent and safe to interrupt, since the
	// recovery path can itself be killed. For steps that cannot be left
	// partial it is a no-op.
	Repair(ctx context.Context, conn *sql.Conn) error
}

// NoTxStep applies outside any transaction.
//
// A crash between Apply returning and the ledger write is expected, and is
// recovered by Satisfied on the next run.
type NoTxStep interface {
	Step

	Apply(ctx context.Context, conn *sql.Conn) error
}
