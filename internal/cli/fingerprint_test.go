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

package cli_test

import (
	"strings"
	"testing"
)

// TestFingerprintIsStableAndSensitive covers both halves of what a digest is
// for: the same schema hashes the same, a changed one does not.
func TestFingerprintIsStableAndSensitive(t *testing.T) {
	database := newDatabase(t)
	dsn := shared.DSN(database)

	before, _, err := run(t, "fingerprint", "--dsn", dsn)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}

	again, _, err := run(t, "fingerprint", "--dsn", dsn)
	if err != nil {
		t.Fatalf("second fingerprint: %v", err)
	}

	if before != again {
		t.Fatalf("an unchanged schema hashed to %q then %q", before, again)
	}

	if _, _, err := run(t, "up", "--dsn", dsn, "--dir", migrationDir(t)); err != nil {
		t.Fatalf("up: %v", err)
	}

	after, _, err := run(t, "fingerprint", "--dsn", dsn)
	if err != nil {
		t.Fatalf("fingerprint after up: %v", err)
	}

	if after == before {
		t.Fatal("adding a column did not change the fingerprint")
	}
}

// TestFingerprintDescribes covers the form that says what went into the digest,
// which is the only way to work out why two databases differ.
func TestFingerprintDescribes(t *testing.T) {
	database := newDatabase(t)

	stdout, _, err := run(t, "fingerprint", "--dsn", shared.DSN(database), "--describe")
	if err != nil {
		t.Fatalf("fingerprint --describe: %v", err)
	}

	if !strings.Contains(stdout, "users") {
		t.Fatalf("description %q does not mention the fixture table", stdout)
	}
}

// TestFingerprintRejectsBadInvocations covers being pointed nowhere.
func TestFingerprintRejectsBadInvocations(t *testing.T) {
	requireHarness(t)

	cases := map[string][]string{
		"no dsn":              {"fingerprint"},
		"unreachable":         {"fingerprint", "--dsn", shared.DSN("no_such_database")},
		"unexpected argument": {"fingerprint", "--dsn", "postgres://x/y", "extra"},
	}

	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := run(t, args...); err == nil {
				t.Fatalf("%v was accepted", args)
			}
		})
	}
}
