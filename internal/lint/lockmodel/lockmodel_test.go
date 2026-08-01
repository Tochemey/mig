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

package lockmodel_test

import (
	"testing"

	"github.com/tochemey/mig/internal/lint/lockmodel"
)

// modes lists every mode with both of its spellings.
var modes = []struct {
	mode    lockmodel.LockMode
	human   string
	pgLocks string
}{
	{lockmodel.AccessShare, "ACCESS SHARE", "AccessShareLock"},
	{lockmodel.RowShare, "ROW SHARE", "RowShareLock"},
	{lockmodel.RowExclusive, "ROW EXCLUSIVE", "RowExclusiveLock"},
	{lockmodel.ShareUpdateExclusive, "SHARE UPDATE EXCLUSIVE", "ShareUpdateExclusiveLock"},
	{lockmodel.Share, "SHARE", "ShareLock"},
	{lockmodel.ShareRowExclusive, "SHARE ROW EXCLUSIVE", "ShareRowExclusiveLock"},
	{lockmodel.Exclusive, "EXCLUSIVE", "ExclusiveLock"},
	{lockmodel.AccessExclusive, "ACCESS EXCLUSIVE", "AccessExclusiveLock"},
}

func TestLockModeSpellings(t *testing.T) {
	for _, tc := range modes {
		if got := tc.mode.String(); got != tc.human {
			t.Errorf("%d.String() = %q, want %q", tc.mode, got, tc.human)
		}

		if got := tc.mode.PgLocksName(); got != tc.pgLocks {
			t.Errorf("%d.PgLocksName() = %q, want %q", tc.mode, got, tc.pgLocks)
		}

		back, ok := lockmodel.ModeFromPgLocks(tc.pgLocks)
		if !ok || back != tc.mode {
			t.Errorf("ModeFromPgLocks(%q) = %v, %v; want %v, true", tc.pgLocks, back, ok, tc.mode)
		}
	}

	if got := lockmodel.LockMode(0).String(); got != "UNKNOWN" {
		t.Errorf("LockMode(0).String() = %q, want UNKNOWN", got)
	}

	if _, ok := lockmodel.ModeFromPgLocks("SIRelationLock"); ok {
		t.Error("ModeFromPgLocks accepted a spelling that is not a table lock mode")
	}
}

// TestLockModeBlocking pins the conflict table's two columns that matter to a
// migration: does holding the mode stop reads, and does it stop writes.
func TestLockModeBlocking(t *testing.T) {
	blocking := []struct {
		mode   lockmodel.LockMode
		reads  bool
		writes bool
	}{
		{lockmodel.AccessShare, false, false},
		{lockmodel.RowShare, false, false},
		{lockmodel.RowExclusive, false, false},
		{lockmodel.ShareUpdateExclusive, false, false},
		{lockmodel.Share, false, true},
		{lockmodel.ShareRowExclusive, false, true},
		{lockmodel.Exclusive, false, true},
		{lockmodel.AccessExclusive, true, true},
	}

	for _, tc := range blocking {
		if got := tc.mode.BlocksReads(); got != tc.reads {
			t.Errorf("%v.BlocksReads() = %v, want %v", tc.mode, got, tc.reads)
		}

		if got := tc.mode.BlocksWrites(); got != tc.writes {
			t.Errorf("%v.BlocksWrites() = %v, want %v", tc.mode, got, tc.writes)
		}
	}
}

func TestDurationClassString(t *testing.T) {
	durations := map[lockmodel.DurationClass]string{
		lockmodel.Instant:          "instant",
		lockmodel.Scan:             "scan",
		lockmodel.Rewrite:          "rewrite",
		lockmodel.IndexBuild:       "index build",
		lockmodel.DurationClass(0): "unknown",
	}

	for class, want := range durations {
		if got := class.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", class, got, want)
		}
	}
}

func TestRelationString(t *testing.T) {
	bare := lockmodel.Relation{Name: "users"}
	if got := bare.String(); got != "users" {
		t.Errorf("bare name rendered as %q", got)
	}

	qualified := lockmodel.Relation{Schema: "app", Name: "users"}
	if got := qualified.String(); got != "app.users" {
		t.Errorf("qualified name rendered as %q", got)
	}
}

func TestAnalysisBlocks(t *testing.T) {
	cases := []struct {
		name     string
		analysis lockmodel.Analysis
		want     lockmodel.BlockProfile
	}{
		{
			name:     "no effects",
			analysis: lockmodel.Analysis{},
			want:     lockmodel.BlockProfile{},
		},
		{
			name: "reads only conflict with nothing",
			analysis: lockmodel.Analysis{Effects: []lockmodel.LockEffect{
				{Mode: lockmodel.AccessShare},
			}},
			want: lockmodel.BlockProfile{},
		},
		{
			name: "an index build blocks writes",
			analysis: lockmodel.Analysis{Effects: []lockmodel.LockEffect{
				{Mode: lockmodel.Share},
			}},
			want: lockmodel.BlockProfile{Writes: true},
		},
		{
			name: "the strongest effect wins",
			analysis: lockmodel.Analysis{Effects: []lockmodel.LockEffect{
				{Mode: lockmodel.AccessShare},
				{Mode: lockmodel.AccessExclusive},
			}},
			want: lockmodel.BlockProfile{Reads: true, Writes: true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.analysis.Blocks(); got != tc.want {
				t.Errorf("Blocks() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
