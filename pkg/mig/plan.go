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
	"errors"
	"io/fs"

	"github.com/tochemey/mig/internal/plan"
	"github.com/tochemey/mig/internal/step"
)

// Unreconcilable is what a step with no way to recognise its own finished work
// is described as.
const Unreconcilable = "cannot be reconciled: no predicate"

// PlannedStep is one step and the condition it will be judged by.
type PlannedStep struct {
	// Migration is the file the step came from.
	Migration string

	// Index orders the step within its migration.
	Index int

	Name string
	Kind string

	// Describe says what the catalog must show for the step to count as done.
	Describe string

	// Reconcilable reports whether the step can run at all. A step that cannot
	// recognise its own finished work stops a run before it starts, so it is
	// worth seeing without a database in hand.
	Reconcilable bool
}

// Plan reports what a run would check, without connecting to anything.
//
// It is the answer to "what will this do?" asked before a database is involved,
// which is when a step that cannot be reconciled is cheapest to find.
func Plan(fsys fs.FS) ([]PlannedStep, error) {
	loaded, err := plan.LoadFS(fsys)
	if err != nil {
		return nil, err
	}

	var planned []PlannedStep

	for _, migration := range loaded.Migrations {
		for _, spec := range migration.Steps {
			describe, reconcilable := describe(spec)

			planned = append(planned, PlannedStep{
				Migration:    migration.ID,
				Index:        spec.Index,
				Name:         spec.Name,
				Kind:         string(spec.Kind),
				Describe:     describe,
				Reconcilable: reconcilable,
			})
		}
	}

	return planned, nil
}

// describe reports the condition a step will be judged by, and whether it can
// run at all.
func describe(spec plan.Step) (string, bool) {
	if _, err := spec.Build(); err != nil {
		if errors.Is(err, step.ErrNoPredicate) || errors.Is(err, step.ErrNoBackfillPredicate) {
			return Unreconcilable, false
		}

		return err.Error(), false
	}

	check := spec.Check()
	if check == nil {
		// A transactional step needs no predicate: its ledger row commits with
		// its work, so there is nothing to reconcile.
		return "ledger (commits with the step)", true
	}

	return check.Describe, true
}
