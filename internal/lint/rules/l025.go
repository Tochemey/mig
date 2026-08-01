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

package rules

import (
	"fmt"

	pgquery "github.com/pganalyze/pg_query_go/v6"

	"github.com/tochemey/mig/internal/lint/lockmodel"
	"github.com/tochemey/mig/internal/plan"
)

// l025 flags a step that turns the lock timeout off while taking a lock
// strong enough to queue traffic behind it.
//
// Postgres grants locks in order, so a statement waiting for ACCESS EXCLUSIVE
// puts every later reader behind it: the wait itself is the outage, and it
// lasts as long as whatever is already holding the table. The timeout bounds
// it. Without one, nothing does.
type l025 struct{}

func (l025) ID() string { return L025 }

func (l025) Check(ctx Context, _ *pgquery.RawStmt) []Finding {
	// One finding for the step, reported against its first statement.
	if ctx.StmtIndex != 0 || !ctx.Step.Spec.NoLockTimeout {
		return nil
	}

	strongest, ok := blockingMode(ctx.Step.Analysed)
	if !ok {
		return nil
	}

	return finding(SeverityError, fmt.Sprintf(
		"%s waits for %s with the lock timeout off; a statement queued behind a long "+
			"read blocks every reader that arrives after it, for as long as that read "+
			"takes. Drop %s%s from the step",
		ctx.Step.Spec.Name, strongest,
		plan.AnnotationPrefix, plan.AnnotationNoLockTimeout), ctx)
}

// blockingMode reports the strongest mode the step takes, and whether it is
// strong enough to make waiting for it a hazard. SHARE is the first mode that
// stops writes, and the first that a queue behind it is worth a finding.
func blockingMode(analysed []lockmodel.Analysis) (lockmodel.LockMode, bool) {
	var strongest lockmodel.LockMode

	for _, analysis := range analysed {
		for _, effect := range analysis.Effects {
			if effect.Mode > strongest {
				strongest = effect.Mode
			}
		}
	}

	return strongest, strongest >= lockmodel.Share
}
