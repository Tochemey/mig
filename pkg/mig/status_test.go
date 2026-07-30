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

package mig_test

import (
	"testing"

	"github.com/tochemey/mig/pkg/mig"
)

// TestStatusReportsWhatTheLedgerHolds covers the attempts and states no catalog
// can report, which is the only reason to read the ledger at all.
func TestStatusReportsWhatTheLedgerHolds(t *testing.T) {
	db := newDatabase(t)

	empty, err := mig.Status(t.Context(), db)
	if err != nil {
		t.Fatalf("status before any run: %v", err)
	}

	if len(empty) != 0 {
		t.Fatalf("an untouched database reported %+v", empty)
	}

	if _, err := mig.Up(t.Context(), db, migrations(t), mig.Options{}); err != nil {
		t.Fatalf("up: %v", err)
	}

	reported, err := mig.Status(t.Context(), db)
	if err != nil {
		t.Fatalf("status: %v", err)
	}

	if len(reported) != 2 {
		t.Fatalf("reported %+v, want both steps", reported)
	}

	for _, step := range reported {
		if step.Status != "succeeded" {
			t.Fatalf("step %q is %q, want succeeded", step.Name, step.Status)
		}

		if step.Attempts != 1 {
			t.Fatalf("step %q took %d attempts, want 1", step.Name, step.Attempts)
		}
	}
}

// TestStatusRejectsAClosedDatabase covers the read failing, which must not be
// reported as a database with nothing recorded.
func TestStatusRejectsAClosedDatabase(t *testing.T) {
	db := newDatabase(t)

	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, err := mig.Status(t.Context(), db); err == nil {
		t.Fatal("status accepted a closed database")
	}
}
