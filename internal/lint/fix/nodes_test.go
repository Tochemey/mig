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

package fix

import (
	"testing"

	pgquery "github.com/pganalyze/pg_query_go/v6"
)

// TestGeneratedSQLQuotesReservedNames is the reason statements are built as
// trees and deparsed: a table or column named like a keyword must come out
// quoted, and no quoting rule lives in this package.
func TestGeneratedSQLQuotesReservedNames(t *testing.T) {
	// Inh is what a parsed relation carries; without it the deparser spells
	// the ONLY that suppresses inheritance.
	rel := &pgquery.RangeVar{Relname: "order", Inh: true}

	update, err := deparse(backfillUpdate(rel, "user", intConst(1), "id"))
	if err != nil {
		t.Fatalf("deparse update: %v", err)
	}

	want := `UPDATE "order" SET "user" = 1 WHERE id > $1 AND id <= $2 AND "user" IS NULL`
	if update != want {
		t.Errorf("update = %q, want %q", update, want)
	}

	predicate, err := deparse(noNullsRemain(rel, "user"))
	if err != nil {
		t.Fatalf("deparse predicate: %v", err)
	}

	want = `SELECT NOT EXISTS (SELECT 1 FROM "order" WHERE "user" IS NULL)`
	if predicate != want {
		t.Errorf("predicate = %q, want %q", predicate, want)
	}
}

// TestDupLeavesTheOriginalAlone pins the deep copy every builder relies on.
func TestDupLeavesTheOriginalAlone(t *testing.T) {
	original := &pgquery.RangeVar{Relname: "users"}

	copied := dup(original)
	copied.Relname = "changed"

	if original.GetRelname() != "users" {
		t.Error("dup shared memory with the original")
	}
}
