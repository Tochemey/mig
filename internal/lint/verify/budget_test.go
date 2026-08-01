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

package verify

import (
	"strings"
	"testing"
	"time"
)

func TestParseBudgetReadsEveryTerm(t *testing.T) {
	budget, err := ParseBudget(" p50=1ms, p99=50ms ,p999=100ms,max_block=2s ")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	want := Budget{
		P50:      time.Millisecond,
		P99:      50 * time.Millisecond,
		P999:     100 * time.Millisecond,
		MaxBlock: 2 * time.Second,
	}

	if budget != want {
		t.Errorf("budget = %+v, want %+v", budget, want)
	}

	if budget.Empty() {
		t.Error("a budget with four terms reported itself empty")
	}
}

// TestParseBudgetAcceptsNothing covers the run that measures and reports
// without failing anything, which is what a first look at a migration is.
func TestParseBudgetAcceptsNothing(t *testing.T) {
	budget, err := ParseBudget("   ")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if !budget.Empty() {
		t.Errorf("budget = %+v, want nothing asked for", budget)
	}

	if exceeded := budget.Exceeded(Window{P99: time.Hour}, time.Hour); exceeded != nil {
		t.Errorf("a budget asking for nothing failed the run: %v", exceeded)
	}
}

func TestParseBudgetRefusesWhatItCannotRead(t *testing.T) {
	cases := map[string]string{
		"no value":         "p99",
		"not a duration":   "p99=soon",
		"a negative limit": "p99=-1s",
		"an unknown term":  "p95=10ms",
	}

	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseBudget(text); err == nil {
				t.Errorf("parsed %q", text)
			}
		})
	}
}

// TestExceededNamesEveryTermBroken covers the verdict: each term is compared
// against what the run reached, and the latency terms read the window during
// the migration rather than the baseline.
func TestExceededNamesEveryTermBroken(t *testing.T) {
	budget := Budget{
		P50:      time.Millisecond,
		P99:      10 * time.Millisecond,
		P999:     20 * time.Millisecond,
		MaxBlock: time.Second,
	}

	during := Window{
		P50:  2 * time.Millisecond,
		P99:  5 * time.Millisecond,
		P999: 50 * time.Millisecond,
	}

	exceeded := budget.Exceeded(during, 3*time.Second)

	if len(exceeded) != 3 {
		t.Fatalf("exceeded %+v, want p50, p999 and max_block", exceeded)
	}

	got := make([]string, 0, len(exceeded))
	for _, violation := range exceeded {
		got = append(got, violation.Term)
	}

	if want := []string{TermP50, TermP999, TermMaxBlock}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("terms = %v, want %v", got, want)
	}

	if line := exceeded[0].String(); !strings.Contains(line, "p50 reached 2ms, budget 1ms") {
		t.Errorf("violation reads %q", line)
	}
}
