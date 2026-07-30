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

package kill_test

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tochemey/mig/internal/crash"
)

// backfillFixture adds a column and fills it in batches.
//
// The two steps matter together: the transactional one creates the column, and
// the resumable one walks the table. Unlike an index build, a batch and the
// cursor covering it can share a transaction, so they do — which is what makes
// "the rows landed but the cursor did not" impossible rather than unlikely.
var backfillFixture = fixture{
	file: "20240817120000_backfill_email.sql",
	body: `-- +mig step: add_email
ALTER TABLE users ADD COLUMN email text;

-- +mig step: fill_email
-- +mig backfill: table=users key=id batch=250
-- +mig satisfied: sql(SELECT NOT EXISTS (SELECT 1 FROM users WHERE email IS NULL))
UPDATE users
   SET email = legacy_email
 WHERE id > :cursor_lo AND id <= :cursor_hi
   AND email IS NULL;
`,
}

// TestBackfillFillsEveryRow is the control: without it, a suite where every run
// quietly did nothing would still pass.
func TestBackfillFillsEveryRow(t *testing.T) {
	t.Parallel()

	env := newRun(t, backfillFixture)

	summary := env.run()
	if summary.Applied != 2 {
		t.Fatalf("applied %d steps, want 2: %+v", summary.Applied, summary)
	}

	if remaining := env.unfilled(); remaining != 0 {
		t.Fatalf("%d rows were left unfilled", remaining)
	}

	env.assertMatchesGolden()
	env.assertSecondRunDoesNothing()
}

// TestBackfillConvergesFromEveryCrashPoint covers being killed part-way through
// a batch, immediately after one committed, and while pacing between them.
func TestBackfillConvergesFromEveryCrashPoint(t *testing.T) {
	cases := []struct {
		name  string
		point string
	}{
		{name: "rows written, nothing committed", point: crash.MidBatch},
		{name: "batch and cursor committed together", point: crash.AfterCheckpoint},
		{name: "between batches, while pacing", point: crash.DuringThrottle},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			env := newRun(t, backfillFixture)

			env.crash(tc.point)
			env.converge()

			if remaining := env.unfilled(); remaining != 0 {
				t.Fatalf("%d rows were left unfilled", remaining)
			}

			env.assertMatchesGolden()
			env.assertSecondRunDoesNothing()
		})
	}
}

// TestBatchRollsBackWithItsCursor is the property the design calls structural.
//
// Killed with a batch's rows written and nothing committed, both the rows and
// the cursor must be gone. A cursor that survived its rows would skip them
// forever, and rows that survived their cursor would be rewritten.
func TestBatchRollsBackWithItsCursor(t *testing.T) {
	t.Parallel()

	env := newRun(t, backfillFixture)

	env.crash(crash.MidBatch)

	cursor, recorded := env.cursor()

	// Killed inside the first batch there is no cursor, and so nothing may have
	// been written either.
	if !recorded {
		if filled := env.filled(); filled != 0 {
			t.Fatalf("%d rows are filled with no cursor recorded: a batch committed without one", filled)
		}

		env.converge()
		env.assertMatchesGolden()
		env.assertSecondRunDoesNothing()

		return
	}

	// The cursor names the highest key it covers, so every row at or below it
	// must be filled and nothing above it may be.
	if above := env.filledAbove(cursor.Position); above != 0 {
		t.Fatalf("%d rows are filled beyond the cursor at %d: a batch committed without it",
			above, cursor.Position)
	}

	if below := env.unfilledAtOrBelow(cursor.Position); below != 0 {
		t.Fatalf("%d rows at or below the cursor at %d are unfilled: the cursor committed without its rows",
			below, cursor.Position)
	}

	t.Logf("cursor at %d with %d rows filled", cursor.Position, env.filled())

	env.converge()
	env.assertMatchesGolden()
	env.assertSecondRunDoesNothing()
}

// TestBackfillConvergesAfterRepeatedKills covers a backfill interrupted again
// and again. Recovery has to converge rather than oscillate, and the attempt
// count has to reflect what actually happened.
func TestBackfillConvergesAfterRepeatedKills(t *testing.T) {
	t.Parallel()

	env := newRun(t, backfillFixture)

	const kills = 3

	for range kills {
		env.crash(crash.AfterCheckpoint)
	}

	env.converge()

	if remaining := env.unfilled(); remaining != 0 {
		t.Fatalf("%d rows were left unfilled", remaining)
	}

	// One attempt per run that reached the step, plus the one that finished it.
	if got := env.recordedStep(1).Attempts; got != kills+1 {
		t.Fatalf("step records %d attempts after %d kills and a completion", got, kills)
	}

	env.assertMatchesGolden()
	env.assertSecondRunDoesNothing()
}

// unfilled counts the rows the backfill has still to touch.
func (r *run) unfilled() int {
	return r.count("SELECT count(*) FROM users WHERE email IS NULL")
}

// filled counts the rows the backfill has already touched.
func (r *run) filled() int {
	return r.count("SELECT count(*) FROM users WHERE email IS NOT NULL")
}

// filledAbove counts filled rows beyond the cursor.
func (r *run) filledAbove(position int64) int {
	return r.count("SELECT count(*) FROM users WHERE email IS NOT NULL AND id > $1", position)
}

// unfilledAtOrBelow counts rows the cursor claims to cover that are not filled.
func (r *run) unfilledAtOrBelow(position int64) int {
	return r.count("SELECT count(*) FROM users WHERE email IS NULL AND id <= $1", position)
}

// count runs a counting query against the run's database.
func (r *run) count(query string, args ...any) int {
	r.t.Helper()

	var n int

	r.withDB(func(db *sql.DB) {
		if err := db.QueryRowContext(r.t.Context(), query, args...).Scan(&n); err != nil {
			r.t.Fatalf("count with %q: %v", query, err)
		}
	})

	return n
}

// checkpoint is a backfill's recorded progress, as the ledger holds it.
type checkpoint struct {
	Position int64 `json:"cursor"`
	Rows     int64 `json:"rows"`
}

// cursor reads the backfill's recorded progress, reporting whether it has any.
func (r *run) cursor() (checkpoint, bool) {
	r.t.Helper()

	var raw []byte

	r.withDB(func(db *sql.DB) {
		const query = `SELECT coalesce(checkpoint, 'null'::jsonb) FROM mig.steps
                        WHERE migration_id = $1 AND idx = 1`

		id := strings.TrimSuffix(r.fixture.file, ".sql")

		if err := db.QueryRowContext(r.t.Context(), query, id).Scan(&raw); err != nil {
			r.t.Fatalf("read checkpoint: %v", err)
		}
	})

	if string(raw) == "null" {
		return checkpoint{}, false
	}

	var cursor checkpoint

	if err := json.Unmarshal(raw, &cursor); err != nil {
		r.t.Fatalf("decode checkpoint %q: %v", raw, err)
	}

	return cursor, true
}
