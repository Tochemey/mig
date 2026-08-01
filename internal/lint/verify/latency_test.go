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
	"testing"
	"time"
)

// TestLatencyKeepsTheTail pins the definition every budget is read against:
// nearest rank, so a quantile is the smallest sample that at least that share
// of them fall at or below.
//
// A thousand samples, of which ten are slow. The p99 sits at the top of the
// fast ones, because 99% of the samples are fast; the tail shows up at p999
// and at the maximum. An average over the same samples is eleven
// milliseconds and describes nothing that happened.
func TestLatencyKeepsTheTail(t *testing.T) {
	samples := &Latency{}

	for range 990 {
		samples.Record(time.Millisecond)
	}

	for range 10 {
		samples.Record(time.Second)
	}

	if got := samples.Count(); got != 1000 {
		t.Errorf("count = %d, want 1000", got)
	}

	if got := samples.Quantile(quantileP50); got != time.Millisecond {
		t.Errorf("p50 = %s, want 1ms", got)
	}

	if got := samples.Quantile(quantileP99); got != time.Millisecond {
		t.Errorf("p99 = %s, want the top of the fast samples", got)
	}

	if got := samples.Quantile(quantileP999); got != time.Second {
		t.Errorf("p999 = %s, want the tail", got)
	}

	if got := samples.Quantile(1); got != time.Second {
		t.Errorf("max = %s, want the worst sample", got)
	}
}

// TestLatencyOfNothingIsZero covers a class that never ran, which is what a
// window taken before its first tick holds.
func TestLatencyOfNothingIsZero(t *testing.T) {
	var samples Latency

	if got := samples.Quantile(quantileP99); got != 0 {
		t.Errorf("p99 of nothing = %s, want zero", got)
	}

	samples.Merge(nil)

	if got := samples.Count(); got != 0 {
		t.Errorf("merging nothing left %d samples", got)
	}
}

// TestWindowLeavesTheInstrumentOut is the measurement's own credibility: the
// long reader is slow by design, and a budget read from a pool it was
// averaged into would fail every run for the wrong reason.
func TestWindowLeavesTheInstrumentOut(t *testing.T) {
	fast := &Latency{}
	for range 100 {
		fast.Record(time.Millisecond)
	}

	slow := &Latency{}
	for range 100 {
		slow.Record(time.Second)
	}

	window := newWindow(map[string]*Latency{"point_read": fast, slowReadClass: slow})

	if window.Count != 100 {
		t.Errorf("count = %d, want only the traffic", window.Count)
	}

	if window.Max != time.Millisecond {
		t.Errorf("max = %s, want the traffic's worst rather than the reader's", window.Max)
	}

	// It is still there to be read: the reader's own latency is what says the
	// instrument was working.
	if got := window.Classes[slowReadClass].Count(); got != 100 {
		t.Errorf("the reader's samples were dropped: %d", got)
	}
}
