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
	"slices"
	"strings"

	pgquery "github.com/pganalyze/pg_query_go/v6"

	"github.com/tochemey/mig/internal/lint/lockmodel"
)

// l020 flags a transactional step that takes ACCESS EXCLUSIVE on more than
// one table.
//
// Each lock is held until the step commits, so the tables are not blocked one
// after another but all at once, for the sum of the work. Two tables locked
// for ten seconds each is one window of twenty seconds across both, and every
// reader of either waits it out.
type l020 struct{}

func (l020) ID() string { return L020 }

func (l020) Check(ctx Context, _ *pgquery.RawStmt) []Finding {
	// One finding for the step, reported against its first statement.
	if ctx.StmtIndex != 0 || !transactional(ctx) {
		return nil
	}

	locked, statements := relationsLockedAt(ctx.Step.Analysed, lockmodel.AccessExclusive)

	// Two statements, and two tables between them. One statement locking a
	// table and its own index is not two windows: it is one piece of work
	// that cannot be split. Two statements against the same table are not
	// two windows either, and doing them together is the better shape.
	if statements < 2 || len(locked) < 2 {
		return nil
	}

	return finding(SeverityWarn, fmt.Sprintf(
		"%s holds ACCESS EXCLUSIVE on %s at once, each until the step commits; "+
			"give each table its own step so the windows do not overlap",
		ctx.Step.Spec.Name, strings.Join(locked, " and ")), ctx)
}

// relationsLockedAt names every relation the step takes at least this mode on,
// in a stable order and each once, and counts how many statements take it.
func relationsLockedAt(analysed []lockmodel.Analysis,
	mode lockmodel.LockMode) (locked []string, statements int) {
	seen := make(map[string]bool)

	for _, analysis := range analysed {
		takes := false

		for _, effect := range analysis.Effects {
			if effect.Mode < mode {
				continue
			}

			takes = true

			if name := effect.Relation.String(); !seen[name] {
				seen[name] = true

				locked = append(locked, name)
			}
		}

		if takes {
			statements++
		}
	}

	slices.Sort(locked)

	return locked, statements
}
