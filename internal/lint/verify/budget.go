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

// Package verify measures what a migration does to a database somebody is
// using.
//
// Static analysis predicts; this measures. A workload runs against a
// throwaway database, its latency is recorded before and during the
// migration, and what the server was waiting on is sampled from
// pg_stat_activity throughout. The verdict is a budget: exceed it and the
// command exits non-zero, so a run can gate a change rather than only
// describe it.
package verify

import (
	"fmt"
	"strings"
	"time"
)

// The budget terms, as they are written on the command line.
const (
	TermP50      = "p50"
	TermP99      = "p99"
	TermP999     = "p999"
	TermMaxBlock = "max_block"
)

// Budget is what the run is allowed to cost. A term left unset is not
// checked, so a budget names the things its author cares about and stays
// silent about the rest.
type Budget struct {
	P50      time.Duration
	P99      time.Duration
	P999     time.Duration
	MaxBlock time.Duration
}

// Empty reports whether the budget asks for anything at all.
func (b Budget) Empty() bool {
	return b == Budget{}
}

// ParseBudget reads a budget as it is written on the command line:
// "p99=50ms,max_block=2s".
func ParseBudget(text string) (Budget, error) {
	var budget Budget

	if strings.TrimSpace(text) == "" {
		return budget, nil
	}

	for term := range strings.SplitSeq(text, ",") {
		name, value, ok := strings.Cut(strings.TrimSpace(term), "=")
		if !ok {
			return Budget{}, fmt.Errorf("budget term %q is not name=duration", term)
		}

		limit, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil {
			return Budget{}, fmt.Errorf("budget term %q: %w", term, err)
		}

		if limit <= 0 {
			return Budget{}, fmt.Errorf("budget term %q: a limit must be positive", term)
		}

		switch strings.TrimSpace(name) {
		case TermP50:
			budget.P50 = limit
		case TermP99:
			budget.P99 = limit
		case TermP999:
			budget.P999 = limit
		case TermMaxBlock:
			budget.MaxBlock = limit
		default:
			return Budget{}, fmt.Errorf("budget term %q: unknown, use %s, %s, %s or %s",
				name, TermP50, TermP99, TermP999, TermMaxBlock)
		}
	}

	return budget, nil
}

// Violation is one term the run exceeded.
type Violation struct {
	Term    string
	Allowed time.Duration
	Reached time.Duration
}

// String renders the violation for a report.
func (v Violation) String() string {
	return fmt.Sprintf("%s reached %s, budget %s", v.Term, v.Reached.Round(time.Millisecond),
		v.Allowed.Round(time.Millisecond))
}

// Exceeded lists the terms the measurements broke, in the order the budget
// names them.
//
// The latency terms are read from the window during the migration: the
// baseline is what the database does when nothing is happening to it, and
// holding that to a budget would be measuring the workload rather than the
// migration.
func (b Budget) Exceeded(during Window, longest time.Duration) []Violation {
	terms := []struct {
		name    string
		allowed time.Duration
		reached time.Duration
	}{
		{TermP50, b.P50, during.P50},
		{TermP99, b.P99, during.P99},
		{TermP999, b.P999, during.P999},
		{TermMaxBlock, b.MaxBlock, longest},
	}

	var exceeded []Violation

	for _, term := range terms {
		if term.allowed > 0 && term.reached > term.allowed {
			exceeded = append(exceeded, Violation{
				Term: term.name, Allowed: term.allowed, Reached: term.reached,
			})
		}
	}

	return exceeded
}
