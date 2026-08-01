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

	domains, statements := lockDomains(ctx)

	// Two statements, and two lock domains between them. One statement
	// locking a table and its own index is not two windows: it is one piece
	// of work that cannot be split. Two statements against the same table
	// are not two windows either, and doing them together is the better
	// shape. Locks on what the step itself creates block nobody at all.
	if statements < 2 || len(domains) < 2 {
		return nil
	}

	return finding(SeverityWarn, fmt.Sprintf(
		"%s holds ACCESS EXCLUSIVE on %s at once, each until the step commits; "+
			"give each table its own step so the windows do not overlap",
		ctx.Step.Spec.Name, strings.Join(domains, " and ")), ctx)
}

// lockDomains names the independent populations of traffic the step's ACCESS
// EXCLUSIVE locks can stop, in a stable order, and counts the statements
// taking one. An index is its table's traffic: dropping it locks the table
// too. A view whose base is itself locked adds nothing, because every query
// through the view already waits on the base.
func lockDomains(ctx Context) (domains []string, statements int) {
	locked := make(map[string]bool)

	var effects []lockmodel.LockEffect

	for i := range ctx.Step.Analysed {
		found := blockableAt(ctx, i, lockmodel.AccessExclusive)
		if len(found) == 0 {
			continue
		}

		statements++

		for _, effect := range found {
			locked[effect.Relation.Name] = true
		}

		effects = append(effects, found...)
	}

	seen := make(map[string]bool)

	for _, effect := range effects {
		name := effect.Relation.Name

		if parent, ok := ctx.History.Parent(name); ok {
			name = parent
		} else if bases := ctx.History.ViewBases(name); covered(bases, locked) {
			continue
		}

		if !seen[name] {
			seen[name] = true

			domains = append(domains, name)
		}
	}

	slices.Sort(domains)

	return domains, statements
}

// covered reports whether any of the view's bases is itself locked.
func covered(bases []string, locked map[string]bool) bool {
	for _, base := range bases {
		if locked[base] {
			return true
		}
	}

	return false
}
