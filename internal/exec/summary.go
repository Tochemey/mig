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

package exec

import "time"

// StepResult is what one step did.
type StepResult struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Status     string `json:"status"`
	Attempts   int    `json:"attempts"`
	Repaired   bool   `json:"repaired"`
	DurationMs int64  `json:"duration_ms"`
}

// Summary is the machine-readable account of a run, written to stdout.
//
// It is a contract rather than a convenience: convergence is asserted by
// checking that a second run reports Applied == 0, and parsing that out of
// human-readable logs would be fragile in both directions.
type Summary struct {
	Migrations int          `json:"migrations"`
	StepsTotal int          `json:"steps_total"`
	Applied    int          `json:"applied"`
	Skipped    int          `json:"skipped"`
	Repaired   int          `json:"repaired"`
	DurationMs int64        `json:"duration_ms"`
	Steps      []StepResult `json:"steps"`
}

// apply records a step whose work this run performed.
func (s *Summary) apply(result StepResult, started time.Time) {
	s.Applied++
	s.record(result, started)
}

// skip records a step the catalog showed was already done.
func (s *Summary) skip(result StepResult, started time.Time) {
	s.Skipped++
	s.record(result, started)
}

// record appends a step's result.
func (s *Summary) record(result StepResult, started time.Time) {
	result.DurationMs = time.Since(started).Milliseconds()

	s.StepsTotal++
	s.Steps = append(s.Steps, result)
}

// finish stamps the total duration.
func (s *Summary) finish(started time.Time) {
	s.DurationMs = time.Since(started).Milliseconds()

	if s.Steps == nil {
		s.Steps = []StepResult{}
	}
}
