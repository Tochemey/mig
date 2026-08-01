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

package mig

import (
	"context"
	"database/sql"
	"io"
	"io/fs"

	"github.com/tochemey/mig/internal/lint/verify"
)

// Workload is a chaos run's traffic, read from a file.
type Workload = verify.Config

// ChaosReport is what a chaos run measured.
type ChaosReport = verify.Report

// Budget is what a chaos run is allowed to cost.
type Budget = verify.Budget

// ParseWorkload reads a workload file.
func ParseWorkload(data []byte) (Workload, error) {
	return verify.ParseConfig(data)
}

// ParseBudget reads a budget as it is written on the command line, such as
// "p99=50ms,max_block=2s".
func ParseBudget(text string) (Budget, error) {
	return verify.ParseBudget(text)
}

// RenderChaos writes the report the way a person reads it.
func RenderChaos(w io.Writer, report ChaosReport) error {
	return verify.Render(w, report)
}

// Chaos applies the migrations under a workload and measures what it cost.
//
// Static analysis predicts; this measures. The traffic runs on work while the
// migration runs on control, which must be separate pools: a pool the
// workload has exhausted looks exactly like lock contention and would be
// reported as the migration's doing.
//
// It is destructive by design. Point it at a throwaway database, which is
// what the command builds for itself.
func Chaos(ctx context.Context, control, work *sql.DB, fsys fs.FS,
	workload Workload, budget Budget) (ChaosReport, error) {
	return verify.Run(ctx, control, work, verify.Options{
		Config:     workload,
		Migrations: fsys,
		Budget:     budget,
		Apply: func(ctx context.Context, db *sql.DB, migrations fs.FS) error {
			_, err := Up(ctx, db, migrations, Options{})

			return err
		},
	})
}
