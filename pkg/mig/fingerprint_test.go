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
	"strings"
	"testing"

	"github.com/tochemey/mig/pkg/mig"
)

// TestFingerprintIsStableAndSensitive covers both halves of what a digest is
// for: the same schema hashes the same, a changed one does not.
func TestFingerprintIsStableAndSensitive(t *testing.T) {
	db := newDatabase(t)

	before, err := mig.Fingerprint(t.Context(), db)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}

	again, err := mig.Fingerprint(t.Context(), db)
	if err != nil {
		t.Fatalf("second fingerprint: %v", err)
	}

	if before != again {
		t.Fatalf("an unchanged schema hashed to %q then %q", before, again)
	}

	if _, err := mig.Up(t.Context(), db, migrations(t), mig.Options{}); err != nil {
		t.Fatalf("up: %v", err)
	}

	after, err := mig.Fingerprint(t.Context(), db)
	if err != nil {
		t.Fatalf("fingerprint after up: %v", err)
	}

	if after == before {
		t.Fatal("applying a migration did not change the fingerprint")
	}
}

// TestDescribeNamesWhatWasHashed covers the form that says why two databases
// differ, which a bare digest cannot.
func TestDescribeNamesWhatWasHashed(t *testing.T) {
	db := newDatabase(t)

	described, err := mig.Describe(t.Context(), db)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}

	if !strings.Contains(described, "users") {
		t.Fatalf("description %q does not mention the fixture table", described)
	}
}

// TestFingerprintRejectsAClosedDatabase covers the read failing rather than
// hashing to something an unrelated database might match.
func TestFingerprintRejectsAClosedDatabase(t *testing.T) {
	db := newDatabase(t)

	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, err := mig.Fingerprint(t.Context(), db); err == nil {
		t.Fatal("fingerprint accepted a closed database")
	}

	if _, err := mig.Describe(t.Context(), db); err == nil {
		t.Fatal("describe accepted a closed database")
	}
}
