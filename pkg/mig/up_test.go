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
	"errors"
	"testing"
	"testing/fstest"

	"github.com/tochemey/mig/pkg/mig"
)

// A program that migrates without shelling out to the binary is the point of
// this half of the package.

// TestUpAppliesAndConverges covers a run doing its work, and a second run over
// the same database finding nothing left to do.
func TestUpAppliesAndConverges(t *testing.T) {
	db := newDatabase(t)

	summary, err := mig.Up(t.Context(), db, migrations(t), mig.Options{})
	if err != nil {
		t.Fatalf("up: %v", err)
	}

	if summary.Applied != 2 {
		t.Fatalf("applied %d steps, want 2: %+v", summary.Applied, summary)
	}

	// What the run just did is what verify must now agree about.
	if err := mig.Verify(t.Context(), db, migrations(t)); err != nil {
		t.Fatalf("verify after up: %v", err)
	}

	again, err := mig.Up(t.Context(), db, migrations(t), mig.Options{})
	if err != nil {
		t.Fatalf("second up: %v", err)
	}

	if again.Applied != 0 {
		t.Fatalf("the second run applied %d steps: %+v", again.Applied, again)
	}
}

// TestUpReportsBeingLocked covers a second runner meeting the first. A caller
// has to tell contention from failure, because it retries one and reports the
// other.
func TestUpReportsBeingLocked(t *testing.T) {
	db := newDatabase(t)

	release := holdLease(t, db)
	defer release()

	_, err := mig.Up(t.Context(), db, migrations(t), mig.Options{OnLocked: mig.Fail})
	if !errors.Is(err, mig.ErrLocked) {
		t.Fatalf("up returned %v, want ErrLocked", err)
	}
}

// TestUpRejectsAnUnloadableSource covers a caller who embedded the wrong path.
// Nothing to apply and nothing embedded look identical from the outside, so the
// second has to be an error.
func TestUpRejectsAnUnloadableSource(t *testing.T) {
	db := newDatabase(t)

	if _, err := mig.Up(t.Context(), db, fstest.MapFS{}, mig.Options{}); err == nil {
		t.Fatal("up accepted a source with no migrations")
	}
}

// TestUpDetectsChecksumDrift covers a migration edited after it was applied,
// and the option that says the edit was deliberate.
func TestUpDetectsChecksumDrift(t *testing.T) {
	db := newDatabase(t)

	if _, err := mig.Up(t.Context(), db, migrations(t), mig.Options{}); err != nil {
		t.Fatalf("up: %v", err)
	}

	edited := fstest.MapFS{
		"20240817120000_add_email.sql": &fstest.MapFile{Data: []byte(
			"-- +mig step: add_email\nALTER TABLE users ADD COLUMN email varchar(320);\n")},
	}

	_, err := mig.Up(t.Context(), db, edited, mig.Options{})
	if !errors.Is(err, mig.ErrChecksumDrift) {
		t.Fatalf("up returned %v, want ErrChecksumDrift", err)
	}

	if _, err := mig.Up(t.Context(), db, edited, mig.Options{AllowDrift: true}); err != nil {
		t.Fatalf("up with AllowDrift: %v", err)
	}
}

// TestUpRunsBatchesOnTheirOwnPool covers the option that keeps batch traffic
// off the pool the heartbeat uses.
func TestUpRunsBatchesOnTheirOwnPool(t *testing.T) {
	name, db := newNamedDatabase(t)
	work := openOn(t, name)

	summary, err := mig.Up(t.Context(), db, migrations(t), mig.Options{Work: work})
	if err != nil {
		t.Fatalf("up: %v", err)
	}

	if summary.Applied != 2 {
		t.Fatalf("applied %d steps, want 2: %+v", summary.Applied, summary)
	}
}
