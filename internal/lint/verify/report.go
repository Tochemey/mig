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
	"fmt"
	"io"
	"strings"
	"time"
)

// waitsReported is how many wait events the human report names before it
// stops: past the first few, the tail is noise.
const waitsReported = 4

// Render writes the report the way a person reads it: the two windows side by
// side, what the server was waiting on, and the verdict.
func Render(w io.Writer, report Report) error {
	out := &strings.Builder{}

	_, _ = fmt.Fprintf(out, "BASELINE   p50 %-9s p99 %-9s max %-9s (%d queries)\n",
		short(report.Baseline.P50), short(report.Baseline.P99), short(report.Baseline.Max),
		report.Baseline.Count)

	_, _ = fmt.Fprintf(out, "DURING     p50 %-9s p99 %-9s max %-9s (%d queries)%s\n",
		short(report.During.P50), short(report.During.P99), short(report.During.Max),
		report.During.Count, verdict(report))

	if report.Applied {
		_, _ = fmt.Fprintf(out, "\nMigration took %s\n", short(report.Migration))
	} else {
		_, _ = fmt.Fprintf(out, "\nMigration did not apply after %s: %s\n",
			short(report.Migration), report.ApplyError)
	}

	_, _ = fmt.Fprintf(out, "Blocked readings: %d of %d\n",
		report.Blocked, report.Blocked+untouched(report))

	if report.Longest.Relation != "" {
		_, _ = fmt.Fprintf(out, "Longest block:    %s on %s\n",
			short(report.Longest.For), report.Longest.Relation)
	}

	if attribution := attribution(report.Waits); attribution != "" {
		_, _ = fmt.Fprintf(out, "Wait attribution: %s\n", attribution)
	}

	if report.Unsampled > 0 {
		_, _ = fmt.Fprintf(out, "Readings not taken: %d\n", report.Unsampled)
	}

	for _, violation := range report.Violations {
		_, _ = fmt.Fprintf(out, "FAIL %s\n", violation)
	}

	if _, err := io.WriteString(w, out.String()); err != nil {
		return fmt.Errorf("write report: %w", err)
	}

	return nil
}

// verdict marks the during line when the budget was broken.
func verdict(report Report) string {
	if report.Failed() {
		return "   <- FAIL"
	}

	return ""
}

// untouched is how many sampled backends were not blocked on a relation, so
// the blocked count reads as a share of something.
func untouched(report Report) int {
	total := 0
	for _, wait := range report.Waits {
		total += wait.Samples
	}

	if total < report.Blocked {
		return 0
	}

	return total - report.Blocked
}

// attribution renders the loudest wait events as shares.
func attribution(waits []Wait) string {
	if len(waits) == 0 {
		return ""
	}

	parts := make([]string, 0, waitsReported)

	for _, wait := range waits[:min(len(waits), waitsReported)] {
		parts = append(parts, fmt.Sprintf("%s %.1f%%", wait.Event, wait.Share*100))
	}

	return strings.Join(parts, ", ")
}

// short renders a duration the way the report reads it.
func short(d time.Duration) string {
	switch {
	case d >= time.Second:
		return d.Round(100 * time.Millisecond).String()
	case d >= time.Millisecond:
		return d.Round(100 * time.Microsecond).String()
	default:
		return d.Round(time.Microsecond).String()
	}
}
