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
)

// work names each duration class as the thing a step does, where durations
// phrases the same classes as how long a lock is held.
var work = map[lockmodel.DurationClass]string{
	lockmodel.Instant:    "catalog work on",
	lockmodel.Scan:       "a full scan of",
	lockmodel.Rewrite:    "a table rewrite of",
	lockmodel.IndexBuild: "an index build on",
}

// l021 flags row-by-row work sharing a transaction with other statements.
//
// The scan or rewrite decides how long the step takes, and every lock the
// step's other statements acquire is held for that whole time rather than for
// the microseconds their own catalog work needs. Alone in its own step, the
// long operation blocks only what it must.
type l021 struct{}

func (l021) ID() string { return L021 }

func (l021) Check(ctx Context, _ *pgquery.RawStmt) []Finding {
	// One finding for the step, reported against its first statement.
	if ctx.StmtIndex != 0 || !transactional(ctx) || len(ctx.Step.Analysed) < 2 {
		return nil
	}

	long, ok := longestWork(ctx.Step.Analysed)
	if !ok {
		return nil
	}

	// Only statements whose own locks stop traffic are being held hostage.
	// A read or a write in the same step holds a lock too, but ACCESS SHARE
	// and ROW EXCLUSIVE conflict with almost nothing, and counting them would
	// turn a real finding into a bigger number rather than a truer one.
	others := 0

	for i, analysis := range ctx.Step.Analysed {
		if _, blocking := blockingMode([]lockmodel.Analysis{analysis}); i != long.index && blocking {
			others++
		}
	}

	if others == 0 {
		return nil
	}

	return finding(SeverityWarn, fmt.Sprintf(
		"%s runs %s %s alongside %s, whose locks are held for the whole of it; "+
			"give the long one its own step",
		ctx.Step.Spec.Name, work[long.effect.Duration], long.effect.Relation,
		count(others, "other statement")), ctx)
}

// heaviest is the heaviest effect in a step and the statement it belongs to.
type heaviest struct {
	index  int
	effect lockmodel.LockEffect
}

// longestWork finds the step's heaviest row-by-row effect, and reports false
// when every statement is catalog work.
func longestWork(analysed []lockmodel.Analysis) (heaviest, bool) {
	found := heaviest{}

	for i, analysis := range analysed {
		for _, effect := range analysis.Effects {
			if effect.Duration > found.effect.Duration {
				found = heaviest{index: i, effect: effect}
			}
		}
	}

	return found, found.effect.Duration > lockmodel.Instant
}
