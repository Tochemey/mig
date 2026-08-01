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
	"github.com/tochemey/mig/internal/lint/lockmodel"
)

// blockable reports whether a lock the step's statement takes on the relation
// can block anyone. Two kinds cannot:
//
// A relation this migration itself creates before the statement runs is empty
// and invisible to traffic: nothing reads it, nothing writes it, and a lock
// on it is a lock on nothing. The check is per incarnation, so a drop of a
// pre-existing relation stays blockable even when the same step recreates it.
//
// A relation dropped IF EXISTS that no migration of the plan ever creates is
// reconciliation of a schema this plan never built. On a database the plan
// built, the drop is a no-op and takes no lock at all.
func blockable(ctx Context, relation lockmodel.Relation, stmtIndex int) bool {
	name := relation.Name

	if ctx.History.FreshAt(name, ctx.Migration.File, ctx.StepIndex, stmtIndex) {
		return false
	}

	if drop := ctx.Step.Parsed[stmtIndex].GetStmt().GetDropStmt(); drop != nil &&
		drop.GetMissingOk() && !ctx.History.EverCreated(name) {
		return false
	}

	return true
}

// blockableAt collects the effects of one statement whose mode is at least
// the given one and whose relation a lock can actually block.
func blockableAt(ctx Context, stmtIndex int, mode lockmodel.LockMode) []lockmodel.LockEffect {
	var found []lockmodel.LockEffect

	for _, effect := range ctx.Step.Analysed[stmtIndex].Effects {
		if effect.Mode >= mode && blockable(ctx, effect.Relation, stmtIndex) {
			found = append(found, effect)
		}
	}

	return found
}
