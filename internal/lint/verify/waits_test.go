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
	"testing"
)

// TestReadWaitsReportsAnUnreadableAnswer covers the scan failing, which is
// how a reading that came back in a shape the sampler cannot use is handled.
func TestReadWaitsReportsAnUnreadableAnswer(t *testing.T) {
	control, _ := pools(t)

	rows, err := control.QueryContext(t.Context(), "SELECT 'a', 'b', 'c', 'not a number'")
	if err != nil {
		t.Fatalf("query: %v", err)
	}

	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("close rows: %v", err)
		}
	}()

	if _, err := readWaits(rows); err == nil {
		t.Error("an unscannable answer was read as waits")
	}
}

// TestStopOrdersEquallyLoudWaitsByName covers the tie-break, so a report of
// the same run reads the same way twice.
func TestStopOrdersEquallyLoudWaitsByName(t *testing.T) {
	sampler := &Sampler{
		events:  map[string]int{"Lock:relation": 2, "IO:DataFileRead": 2, "Client:ClientRead": 1},
		samples: 5,
		stop:    func() {},
	}

	waits, _, _, _ := sampler.Stop()

	got := make([]string, 0, len(waits))
	for _, wait := range waits {
		got = append(got, wait.Event)
	}

	want := []string{"IO:DataFileRead", "Lock:relation", "Client:ClientRead"}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("waits = %v, want %v", got, want)
		}
	}
}

// TestFirstErrorPrefersTheRead covers the pair a reading is judged by: a read
// that failed is the finding, and a close that also failed does not hide it.
func TestFirstErrorPrefersTheRead(t *testing.T) {
	read := errors.New("read")
	closed := errors.New("close")

	if got := firstError(read, closed); !errors.Is(got, read) {
		t.Errorf("firstError = %v, want the read failure", got)
	}

	if got := firstError(nil, closed); !errors.Is(got, closed) {
		t.Errorf("firstError = %v, want the close failure", got)
	}

	if got := firstError(nil, nil); got != nil {
		t.Errorf("firstError = %v, want nothing", got)
	}
}
