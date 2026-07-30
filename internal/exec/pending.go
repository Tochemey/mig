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

package exec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/tochemey/mig/internal/ledger"
	"github.com/tochemey/mig/internal/plan"
	"github.com/tochemey/mig/internal/step"
)

// Outstanding names a step whose work the database does not yet show.
type Outstanding struct {
	Migration string
	Step      string
	Kind      string
}

// String renders an outstanding step for a message.
func (o Outstanding) String() string {
	return fmt.Sprintf("%s/%s (%s)", o.Migration, o.Step, o.Kind)
}

// Pending lists the steps whose work the database does not yet show.
//
// It takes no lease and writes nothing, which is what makes it safe to run from
// every replica at startup. Applying from there is the trap this exists to
// replace: every replica races, none of them is the leader, and a slow
// migration collides with the readiness probe.
//
// A ledger that has never been created means nothing has been applied, which is
// a plan's worth of pending work rather than an error.
func Pending(ctx context.Context, db *sql.DB, p *plan.Plan) (_ []Outstanding, err error) {
	built, err := build(p)
	if err != nil {
		return nil, err
	}

	recorded, err := ledger.Present(ctx, db)
	if err != nil {
		return nil, err
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}

	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("release connection: %w", closeErr))
		}
	}()

	var (
		outstanding []Outstanding
		gap         bool
	)

	for i, migration := range p.Migrations {
		for j, spec := range migration.Steps {
			// Once a step is outstanding, every later one is too, and asking
			// about them is not merely wasted work: a predicate routinely
			// refers to what an earlier step creates, so checking it before
			// that step has run fails against a column that does not exist yet.
			if !gap {
				done, err := stepDone(ctx, db, conn, migration, spec, built[i][j], recorded)
				if err != nil {
					return nil, err
				}

				if done {
					continue
				}

				gap = true
			}

			outstanding = append(outstanding, Outstanding{
				Migration: migration.ID,
				Step:      spec.Name,
				Kind:      string(spec.Kind),
			})
		}
	}

	return outstanding, nil
}

// stepDone reports whether one step's work is already present.
//
// The catalog answers first, exactly as it does when applying. Only a
// transactional step falls back to the ledger, and only because its row commits
// with its work.
func stepDone(ctx context.Context, db *sql.DB, conn *sql.Conn, migration plan.Migration,
	spec plan.Step, work step.Step, recorded bool) (bool, error) {
	done, err := work.Satisfied(ctx, conn)
	if err != nil {
		return false, fmt.Errorf("check step %q of %q: %w", spec.Name, migration.ID, err)
	}

	if done {
		return true, nil
	}

	if _, isTx := work.(step.TxStep); !isTx || !recorded {
		return false, nil
	}

	key := ledger.StepKey{MigrationID: migration.ID, Index: spec.Index}

	row, err := ledger.LoadStep(ctx, db, key)
	if errors.Is(err, ledger.ErrNotRecorded) {
		return false, nil
	}

	if err != nil {
		return false, err
	}

	return row.Status == ledger.StatusSucceeded, nil
}
