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

// StepReport is one step's recorded state.
//
// It is what the ledger holds rather than what the catalog shows. Attempts,
// checkpoints and prior errors are the things a catalog cannot report, and they
// are the whole reason to read the ledger at all.
type StepReport struct {
	Migration  string `json:"migration"`
	Index      int    `json:"index"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Status     Status `json:"status"`
	Attempts   int    `json:"attempts"`
	Checkpoint string `json:"checkpoint,omitempty"`
	Error      string `json:"error,omitempty"`
}

// reportQuery reads every recorded step, in the order they are applied.
const reportQuery = `
SELECT migration_id, idx, name, kind, status, attempts,
       coalesce(checkpoint::text, ''), coalesce(error, '')
  FROM mig.steps
 ORDER BY migration_id, idx`

// Report reads the ledger's account of every step.
//
// A database no run has touched has no ledger, which is an empty report rather
// than a failure: nothing has been applied, and saying so is the answer.
func Report(ctx context.Context, db *sql.DB) (_ []StepReport, err error) {
	present, err := Present(ctx, db)
	if err != nil {
		return nil, err
	}

	if !present {
		return []StepReport{}, nil
	}

	rows, err := db.QueryContext(ctx, reportQuery)
	if err != nil {
		return nil, fmt.Errorf("read step status: %w", err)
	}

	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close status rows: %w", closeErr))
		}
	}()

	report := []StepReport{}

	for rows.Next() {
		var step StepReport

		if err := rows.Scan(&step.Migration, &step.Index, &step.Name, &step.Kind,
			&step.Status, &step.Attempts, &step.Checkpoint, &step.Error); err != nil {
			return nil, fmt.Errorf("scan step status: %w", err)
		}

		report = append(report, step)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate step status: %w", err)
	}

	return report, nil
}
