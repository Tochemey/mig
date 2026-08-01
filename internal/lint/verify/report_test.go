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
	"errors"
	"strings"
	"testing"
	"time"
)

// failWriter refuses every write, for the path where the sink is the problem.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) {
	return 0, errors.New("sink failed")
}

// measured is a run that got through and hurt somebody.
func measured() Report {
	return Report{
		Baseline:  Window{Count: 100, P50: 800 * time.Microsecond, P99: 2 * time.Millisecond, Max: 14 * time.Millisecond},
		During:    Window{Count: 120, P50: 900 * time.Microsecond, P99: 4310 * time.Millisecond, Max: 38 * time.Second},
		Migration: 42 * time.Second,
		Applied:   true,
		Waits: []Wait{
			{Event: "Lock:relation", Samples: 992, Share: 0.992},
			{Event: "IO:DataFileRead", Samples: 5, Share: 0.005},
		},
		Blocked:    992,
		Longest:    Block{For: 38 * time.Second, Relation: "users"},
		Violations: []Violation{{Term: TermP99, Allowed: 50 * time.Millisecond, Reached: 4310 * time.Millisecond}},
	}
}

func TestRenderReadsAsAVerdict(t *testing.T) {
	out := &strings.Builder{}

	if err := Render(out, measured()); err != nil {
		t.Fatalf("render: %v", err)
	}

	for _, want := range []string{
		"BASELINE   p50 800µs",
		"DURING",
		"<- FAIL",
		"Migration took 42s",
		"Longest block:    38s on users",
		"Wait attribution: Lock:relation 99.2%, IO:DataFileRead 0.5%",
		"FAIL p99 reached 4.31s, budget 50ms",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
}

// TestRenderSaysWhenTheMigrationNeverApplied covers the other verdict: a
// migration the executor refused to force through is a finding, not an error,
// and the report has to say so.
func TestRenderSaysWhenTheMigrationNeverApplied(t *testing.T) {
	report := measured()
	report.Applied = false
	report.ApplyError = "canceling statement due to lock timeout"
	report.Violations = nil

	out := &strings.Builder{}

	if err := Render(out, report); err != nil {
		t.Fatalf("render: %v", err)
	}

	if !strings.Contains(out.String(), "Migration did not apply after 42s: canceling statement") {
		t.Errorf("report does not say the migration failed:\n%s", out.String())
	}

	if !report.Failed() {
		t.Error("a migration that never applied passed its verification")
	}
}

// TestRenderReportsReadingsItCouldNotTake covers the honesty of the
// attribution: a sampler that saw nothing must not read as a quiet server.
func TestRenderReportsReadingsItCouldNotTake(t *testing.T) {
	report := measured()
	report.Unsampled = 7

	out := &strings.Builder{}

	if err := Render(out, report); err != nil {
		t.Fatalf("render: %v", err)
	}

	if !strings.Contains(out.String(), "Readings not taken: 7") {
		t.Errorf("report hides the readings it missed:\n%s", out.String())
	}
}

// TestRenderOfAQuietRun covers the other half of every line: a run that
// passed, blocked nobody and had nothing to attribute.
func TestRenderOfAQuietRun(t *testing.T) {
	quiet := Report{
		Baseline: Window{Count: 100, P50: time.Millisecond},
		During:   Window{Count: 100, P50: time.Millisecond},
		Applied:  true,
	}

	out := &strings.Builder{}

	if err := Render(out, quiet); err != nil {
		t.Fatalf("render: %v", err)
	}

	if quiet.Failed() {
		t.Error("a quiet run failed its verification")
	}

	for _, unwanted := range []string{"FAIL", "Longest block", "Wait attribution"} {
		if strings.Contains(out.String(), unwanted) {
			t.Errorf("a quiet run reported %q:\n%s", unwanted, out.String())
		}
	}
}

// TestUntouchedCannotGoNegative covers the share the blocked count is read
// against when the sampler saw fewer events than blocks.
func TestUntouchedCannotGoNegative(t *testing.T) {
	if got := untouched(Report{Blocked: 5}); got != 0 {
		t.Errorf("untouched = %d, want none", got)
	}
}

func TestRenderReportsItsSink(t *testing.T) {
	if err := Render(failWriter{}, measured()); err == nil {
		t.Error("a failed write went unreported")
	}
}
