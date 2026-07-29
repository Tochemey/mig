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

package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrFenced reports that the caller's lease token is stale: another runner has
// taken the lease. It is neither retryable nor recoverable.
var ErrFenced = errors.New("lease fenced: another runner holds the lease")

// guardQuery is the first statement of every ledger mutation. FOR UPDATE holds
// the lease row for the rest of the transaction, so the caller cannot be
// superseded between passing the check and committing.
const guardQuery = `
SELECT 1
  FROM mig.lease
 WHERE id = 1 AND owner = $1 AND fence = $2
   FOR UPDATE`

// TxOptions pins the isolation level of every transaction carrying a ledger
// write, including one a step opens for its own DDL.
//
// Read Committed is required, and nil would inherit
// default_transaction_isolation from the cluster. Under a snapshot isolation
// level, SELECT ... FOR UPDATE against a row a successor has already updated
// aborts with a serialisation failure instead of returning no rows, which
// would report a final [ErrFenced] as a retryable error.
func TxOptions() *sql.TxOptions {
	return &sql.TxOptions{Isolation: sql.LevelReadCommitted}
}

// Fence identifies a lease holder. The token is monotonic across acquisitions,
// so a stale holder is recognisable even with the right owner string.
type Fence struct {
	Owner string
	Token int64
}

// String renders a fence for logs and errors.
func (f Fence) String() string {
	return fmt.Sprintf("%s#%d", f.Owner, f.Token)
}

// Guard asserts that f still holds the lease, inside a transaction the caller
// already owns.
//
// It is separate from [Write] for the writes that must ride in another
// transaction: a transactional DDL step commits its ledger row with its DDL,
// and a backfill batch commits its cursor with the rows it covers.
func Guard(ctx context.Context, tx *sql.Tx, f Fence) error {
	var held int

	err := tx.QueryRowContext(ctx, guardQuery, f.Owner, f.Token).Scan(&held)

	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w (held fence %s)", ErrFenced, f)
	}

	if err != nil {
		return fmt.Errorf("check lease fence %s: %w", f, err)
	}

	return nil
}

// Write runs fn inside a transaction guarded by f, and commits it.
//
// Every ledger mutation goes through here or through [Guard]. A write by any
// other route lets a runner that was frozen past its lease expiry overwrite
// the state of the runner that replaced it.
func Write(ctx context.Context, db *sql.DB, f Fence, fn func(context.Context, *sql.Tx) error) (err error) {
	tx, err := db.BeginTx(ctx, TxOptions())
	if err != nil {
		return fmt.Errorf("begin fenced write: %w", err)
	}

	defer func() {
		err = errors.Join(err, rollback(tx))
	}()

	if err := Guard(ctx, tx, f); err != nil {
		return err
	}

	if err := fn(ctx, tx); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit fenced write: %w", err)
	}

	return nil
}

// rollback undoes a transaction that was not committed.
//
// A rollback after a successful commit reports [sql.ErrTxDone] and is
// discarded. Any other failure leaves the transaction's outcome unknown and is
// returned, so the caller does not assume the ledger was left untouched.
func rollback(tx *sql.Tx) error {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return fmt.Errorf("roll back ledger transaction: %w", err)
	}

	return nil
}
