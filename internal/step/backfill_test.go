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

package step_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"testing"

	"github.com/tochemey/mig/internal/catalog"
	"github.com/tochemey/mig/internal/step"
	"github.com/tochemey/mig/internal/throttle"
	"github.com/tochemey/mig/test/harness"
)

// fillEmail is the shape a backfill takes: a keyset range and a predicate that
// keeps it from rewriting what it has already done.
const fillEmail = `UPDATE users SET email = legacy_email
 WHERE id > $1 AND id <= $2 AND email IS NULL`

// TestNewBackfillRequiresAPredicate is the refusal that keeps the ledger from
// deciding whether a backfill is finished. The cursor lives there, and a cursor
// cannot say whether rows appeared behind it.
func TestNewBackfillRequiresAPredicate(t *testing.T) {
	_, err := step.NewBackfill(step.Meta{Name: "fill"}, fillEmail, step.BackfillConfig{}, nil)

	if !errors.Is(err, step.ErrNoBackfillPredicate) {
		t.Fatalf("construction returned %v, want ErrNoBackfillPredicate", err)
	}
}

// TestBackfillWalksTheWholeTable covers the ordinary run: every row filled, in
// batches, with the cursor ending past the highest key.
func TestBackfillWalksTheWholeTable(t *testing.T) {
	db, conn := newFilledTable(t, 500)

	work := newBackfill(t, step.BackfillConfig{Table: "users", Key: "id", Batch: 120})
	env, state := newEnv(t, db)

	if done := satisfied(t, work, conn); done {
		t.Fatal("satisfied before anything was filled")
	}

	if err := work.Run(t.Context(), env); err != nil {
		t.Fatalf("run: %v", err)
	}

	if !satisfied(t, work, conn) {
		t.Fatal("not satisfied once the table was walked")
	}

	if remaining := count(t, db, "SELECT count(*) FROM users WHERE email IS NULL"); remaining != 0 {
		t.Fatalf("%d rows were left unfilled", remaining)
	}

	cursor := decodeCursor(t, *state)

	if cursor.Position < 500 {
		t.Fatalf("cursor stopped at %d, short of the highest key", cursor.Position)
	}

	if cursor.Rows != 500 {
		t.Fatalf("cursor counted %d rows, want 500", cursor.Rows)
	}
}

// TestBackfillResumesFromItsCursor covers a second run picking up where the
// first stopped, rather than starting again.
func TestBackfillResumesFromItsCursor(t *testing.T) {
	db, _ := newFilledTable(t, 300)

	work := newBackfill(t, step.BackfillConfig{Table: "users", Key: "id", Batch: 100})
	env, state := newEnv(t, db)

	// Half the table is already done, and recorded as such.
	execDB(t, db, "UPDATE users SET email = legacy_email WHERE id <= 150")

	recorded, err := json.Marshal(step.Cursor{Position: 150, Rows: 150})
	if err != nil {
		t.Fatalf("encode cursor: %v", err)
	}

	*state = recorded

	if err := work.Run(t.Context(), env); err != nil {
		t.Fatalf("run: %v", err)
	}

	cursor := decodeCursor(t, *state)

	// Only the rows above the cursor were touched, so the count grew by exactly
	// the remainder rather than by the whole table.
	if cursor.Rows != 300 {
		t.Fatalf("cursor counted %d rows, want 300: the run did not resume", cursor.Rows)
	}
}

// TestBackfillResumesFromACursorAtZero covers a key range that reaches zero.
//
// Treating a cursor's value rather than its presence as "never started" would
// rewind the backfill here, and a statement that is not idempotent would then
// be applied twice.
func TestBackfillResumesFromACursorAtZero(t *testing.T) {
	db, _ := newFilledTable(t, 0)

	// Keys from -5 to 5, so a cursor at zero is halfway rather than absent.
	execDB(t, db, `INSERT INTO users (id, name, legacy_email)
                   SELECT g, 'user_' || g, 'user_' || g || '@example.test'
                     FROM generate_series(-5, 5) AS g`)
	execDB(t, db, "UPDATE users SET email = legacy_email WHERE id <= 0")

	work := newBackfill(t, step.BackfillConfig{Table: "users", Key: "id", Batch: 100})
	env, state := newEnv(t, db)

	recorded, err := json.Marshal(step.Cursor{Position: 0, Rows: 6})
	if err != nil {
		t.Fatalf("encode cursor: %v", err)
	}

	*state = recorded

	if err := work.Run(t.Context(), env); err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := decodeCursor(t, *state).Rows; got != 11 {
		t.Fatalf("cursor counted %d rows, want 11: the run rewound past a cursor at zero", got)
	}
}

// TestBackfillOnAnEmptyTable covers a range with nothing in it, which must do
// no work rather than loop.
func TestBackfillOnAnEmptyTable(t *testing.T) {
	db, _ := newFilledTable(t, 0)

	work := newBackfill(t, step.BackfillConfig{Table: "users", Key: "id", Batch: 100})
	env, state := newEnv(t, db)

	if err := work.Run(t.Context(), env); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(*state) != 0 {
		t.Fatalf("an empty table recorded a cursor: %s", *state)
	}
}

// TestBackfillRepairsNothing covers the difference from an index build. Every
// batch commits with its cursor, so there is never a partial state to clear.
func TestBackfillRepairsNothing(t *testing.T) {
	_, conn := newFilledTable(t, 10)

	work := newBackfill(t, step.BackfillConfig{Table: "users", Key: "id", Batch: 5})

	if err := work.Repair(t.Context(), conn); err != nil {
		t.Fatalf("repair: %v", err)
	}
}

// TestBackfillReportsAFailingStatement covers SQL the server rejects.
func TestBackfillReportsAFailingStatement(t *testing.T) {
	db, _ := newFilledTable(t, 10)

	broken, err := step.NewBackfill(
		step.Meta{Name: "broken"},
		"UPDATE users SET email = no_such_column WHERE id > $1 AND id <= $2",
		step.BackfillConfig{Table: "users", Key: "id", Batch: 5},
		func(context.Context, catalog.Querier) (bool, error) { return false, nil },
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	env, _ := newEnv(t, db)

	if err := broken.Run(t.Context(), env); err == nil {
		t.Fatal("run accepted a statement against a column that does not exist")
	}
}

// TestBackfillReportsAnUnreadableRange covers a table the annotation names but
// the database does not have.
func TestBackfillReportsAnUnreadableRange(t *testing.T) {
	db, _ := newFilledTable(t, 10)

	work := newBackfill(t, step.BackfillConfig{Table: "no_such_table", Key: "id", Batch: 5})
	env, _ := newEnv(t, db)

	if err := work.Run(t.Context(), env); err == nil {
		t.Fatal("run accepted a table that does not exist")
	}
}

// TestBackfillReportsAnUnreadableCheckpoint covers a cursor that cannot be
// decoded, which must stop the run rather than silently start again.
func TestBackfillReportsAnUnreadableCheckpoint(t *testing.T) {
	db, _ := newFilledTable(t, 10)

	work := newBackfill(t, step.BackfillConfig{Table: "users", Key: "id", Batch: 5})
	env, state := newEnv(t, db)

	*state = []byte("not json")

	if err := work.Run(t.Context(), env); err == nil {
		t.Fatal("run accepted a checkpoint it could not read")
	}
}

// newBackfill builds a backfill over the fixture with a predicate that reports
// when no rows remain.
func newBackfill(t *testing.T, cfg step.BackfillConfig) *step.Backfill {
	t.Helper()

	const remaining = "SELECT NOT EXISTS (SELECT 1 FROM users WHERE email IS NULL)"

	check := func(ctx context.Context, q catalog.Querier) (bool, error) {
		var done bool

		if err := q.QueryRowContext(ctx, remaining).Scan(&done); err != nil {
			return false, err
		}

		return done, nil
	}

	work, err := step.NewBackfill(step.Meta{Name: "fill_email", Kind: step.KindBackfill},
		fillEmail, cfg, check)
	if err != nil {
		t.Fatalf("build backfill: %v", err)
	}

	return work
}

// newEnv supplies what a resumable step needs, and returns the checkpoint the
// step writes so a test can read or seed it.
//
// The transaction it hands out is not fenced: there is no lease here, and what
// is under test is the batch loop rather than the executor's plumbing.
func newEnv(t *testing.T, db *sql.DB) (step.Env, *[]byte) {
	t.Helper()

	state := new([]byte)

	env := step.Env{
		Begin: func(ctx context.Context) (*sql.Tx, error) {
			return db.BeginTx(ctx, nil)
		},
		Checkpoint: func(_ context.Context, _ *sql.Tx, written []byte) error {
			*state = written
			return nil
		},
		Load: func(context.Context) ([]byte, error) {
			return *state, nil
		},
		Read: func(ctx context.Context, query string, args ...any) *sql.Row {
			return db.QueryRowContext(ctx, query, args...)
		},
		Retry: func(ctx context.Context, batch func(context.Context) error) error {
			return batch(ctx)
		},
		Throttle: throttle.New(throttle.Config{Batch: throttle.MinBatch}),
	}

	return env, state
}

// newFilledTable gives the test its own database holding keys 1..rows, with the
// column a backfill fills still empty.
func newFilledTable(t *testing.T, rows int) (*sql.DB, *sql.Conn) {
	t.Helper()

	if shared == nil {
		t.Skip("postgres container not available")
	}

	name, err := shared.Clone(t.Context(), harness.Template)
	if err != nil {
		t.Fatalf("clone template: %v", err)
	}

	db, err := shared.Open(t.Context(), name)
	if err != nil {
		t.Fatalf("open clone: %v", err)
	}

	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatalf("pin connection: %v", err)
	}

	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close connection: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Errorf("close pool: %v", err)
		}

		if err := shared.DropDatabase(context.Background(), name); err != nil {
			t.Errorf("drop clone %q: %v", name, err)
		}
	})

	execDB(t, db, "ALTER TABLE users ADD COLUMN email text")

	if rows > 0 {
		execDB(t, db, `INSERT INTO users (id, name, legacy_email)
                       SELECT g, 'user_' || g, 'user_' || g || '@example.test'
                         FROM generate_series(1, `+strconv.Itoa(rows)+`) AS g`)
	}

	return db, conn
}

// execDB runs a statement on a pool, or fails the test.
func execDB(t *testing.T, db *sql.DB, stmt string) {
	t.Helper()

	if _, err := db.ExecContext(t.Context(), stmt); err != nil {
		t.Fatalf("exec %q: %v", stmt, err)
	}
}

// count runs a counting query, or fails the test.
func count(t *testing.T, db *sql.DB, query string) int {
	t.Helper()

	var n int

	if err := db.QueryRowContext(t.Context(), query).Scan(&n); err != nil {
		t.Fatalf("count with %q: %v", query, err)
	}

	return n
}

// decodeCursor reads a recorded checkpoint.
func decodeCursor(t *testing.T, state []byte) step.Cursor {
	t.Helper()

	if len(state) == 0 {
		t.Fatal("no cursor was recorded")
	}

	var cursor step.Cursor

	if err := json.Unmarshal(state, &cursor); err != nil {
		t.Fatalf("decode cursor %q: %v", state, err)
	}

	return cursor
}
