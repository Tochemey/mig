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
	"math"
	"slices"
	"time"
)

// The percentiles a window reports. An average would hide the queries a
// migration hurt, which are the only ones being looked for.
const (
	quantileP50  = 0.50
	quantileP99  = 0.99
	quantileP999 = 0.999
)

// Latency is the set of samples one query class produced, kept whole.
//
// Every sample is stored rather than bucketed, so the percentiles are exact
// rather than an approximation with a stated error. The cost is eight bytes a
// query: a ten-minute run at a thousand a second is under five megabytes.
type Latency struct {
	samples []time.Duration
	sorted  bool
}

// Record adds one observation. It is not safe for concurrent use: the
// workload holds a mutex over the classes while its workers record.
func (l *Latency) Record(d time.Duration) {
	l.samples = append(l.samples, d)
	l.sorted = false
}

// Merge folds another set of samples in.
func (l *Latency) Merge(other *Latency) {
	if other == nil {
		return
	}

	l.samples = append(l.samples, other.samples...)
	l.sorted = false
}

// Count is how many observations there are.
func (l *Latency) Count() int {
	return len(l.samples)
}

// Quantile returns the sample at q, and zero when nothing was measured.
func (l *Latency) Quantile(q float64) time.Duration {
	if len(l.samples) == 0 {
		return 0
	}

	if !l.sorted {
		slices.Sort(l.samples)

		l.sorted = true
	}

	// Nearest rank: the smallest sample that at least q of them fall at or
	// below. For p99 of a hundred samples that is the ninety-ninth, not the
	// worst, which belongs to the maximum.
	rank := int(math.Ceil(q*float64(len(l.samples)))) - 1

	return l.samples[min(max(rank, 0), len(l.samples)-1)]
}

// Window is what one stretch of the run measured, overall and per class.
type Window struct {
	// Classes holds each query class by name.
	Classes map[string]*Latency

	// The overall figures, across every class.
	Count               int
	P50, P99, P999, Max time.Duration
}

// newWindow folds the classes into the figures a report and a budget read.
//
// The long reader is left out of them. It is the instrument rather than the
// traffic: it is slow on purpose, it is what makes the lock queue form, and
// averaging it into the percentiles would put a budget's p99 on a query
// nobody is waiting for. Its samples stay in Classes, where a reader can see
// what it did.
func newWindow(classes map[string]*Latency) Window {
	window := Window{Classes: classes}

	overall := &Latency{}

	for name, class := range classes {
		if name != slowReadClass {
			overall.Merge(class)
		}
	}

	window.Count = overall.Count()
	window.P50 = overall.Quantile(quantileP50)
	window.P99 = overall.Quantile(quantileP99)
	window.P999 = overall.Quantile(quantileP999)
	window.Max = overall.Quantile(1)

	return window
}
