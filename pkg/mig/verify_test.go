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
	"strings"
	"testing"
	"testing/fstest"

	"github.com/tochemey/mig/pkg/mig"
)

// TestVerifyRefusesAnUnmigratedDatabase covers the check a service makes as it
// starts. Nothing has been applied, so it must refuse rather than boot.
func TestVerifyRefusesAnUnmigratedDatabase(t *testing.T) {
	db := newDatabase(t)

	err := mig.Verify(t.Context(), db, migrations(t))
	if !errors.Is(err, mig.ErrPending) {
		t.Fatalf("verify returned %v, want ErrPending", err)
	}

	// The message has to name what is missing; "migrations pending" alone
	// leaves an operator with nothing to act on.
	if !strings.Contains(err.Error(), "add_email") {
		t.Fatalf("error %q does not name the outstanding step", err)
	}
}

// TestVerifyAcceptsAMigratedDatabase covers the other half: work the catalog
// already shows is not reported, whatever the ledger holds.
func TestVerifyAcceptsAMigratedDatabase(t *testing.T) {
	db := newDatabase(t)

	apply(t, db,
		"ALTER TABLE users ADD COLUMN email text",
		"CREATE INDEX idx_users_email ON users (email)")

	if err := mig.Verify(t.Context(), db, migrations(t)); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// TestVerifyPassesWithNoLedger is the premise of the whole tool. A database
// migrated by hand, with no ledger at all, is up to date.
func TestVerifyPassesWithNoLedger(t *testing.T) {
	db := newDatabase(t)

	apply(t, db,
		"ALTER TABLE users ADD COLUMN email text",
		"CREATE INDEX idx_users_email ON users (email)")

	present, err := ledgerExists(t.Context(), db)
	if err != nil {
		t.Fatalf("look for the ledger: %v", err)
	}

	if present {
		t.Fatal("the fixture already has a ledger, so this proves nothing")
	}

	if err := mig.Verify(t.Context(), db, migrations(t)); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// TestVerifyWritesNothing is what makes it safe to call from every replica at
// once: no ledger is created, no lease is taken.
func TestVerifyWritesNothing(t *testing.T) {
	db := newDatabase(t)

	if err := mig.Verify(t.Context(), db, migrations(t)); !errors.Is(err, mig.ErrPending) {
		t.Fatalf("verify returned %v, want ErrPending", err)
	}

	present, err := ledgerExists(t.Context(), db)
	if err != nil {
		t.Fatalf("look for the ledger: %v", err)
	}

	if present {
		t.Fatal("verify created the ledger")
	}
}

// TestPendingNamesEveryOutstandingStep covers the caller that reports the list
// itself rather than acting on one error.
func TestPendingNamesEveryOutstandingStep(t *testing.T) {
	db := newDatabase(t)

	apply(t, db, "ALTER TABLE users ADD COLUMN email text")

	outstanding, err := mig.Pending(t.Context(), db, migrations(t))
	if err != nil {
		t.Fatalf("pending: %v", err)
	}

	if len(outstanding) != 1 {
		t.Fatalf("pending is %v, want only the index", outstanding)
	}

	if outstanding[0].Step != "index_email" {
		t.Fatalf("pending step is %q", outstanding[0].Step)
	}

	if outstanding[0].String() == "" {
		t.Fatal("an outstanding step renders as nothing")
	}
}

// TestPendingReportsAnUnusableIndex covers the failure the whole design turns
// on: an interrupted concurrent build leaves an index that exists and is not
// valid. Existence alone would report it as applied.
func TestPendingReportsAnUnusableIndex(t *testing.T) {
	db := newDatabase(t)

	apply(t, db,
		"ALTER TABLE users ADD COLUMN email text",
		"CREATE INDEX idx_users_email ON users (email)",
		"UPDATE pg_index SET indisvalid = false FROM pg_class c "+
			"WHERE c.oid = pg_index.indexrelid AND c.relname = 'idx_users_email'")

	outstanding, err := mig.Pending(t.Context(), db, migrations(t))
	if err != nil {
		t.Fatalf("pending: %v", err)
	}

	if len(outstanding) != 1 || outstanding[0].Step != "index_email" {
		t.Fatalf("pending is %v, want the invalid index", outstanding)
	}
}

// TestVerifyRejectsAnUnloadableSource covers a caller who embedded the wrong
// path, which otherwise looks exactly like having nothing to do.
func TestVerifyRejectsAnUnloadableSource(t *testing.T) {
	db := newDatabase(t)

	if err := mig.Verify(t.Context(), db, fstest.MapFS{}); err == nil {
		t.Fatal("verify accepted a source with no migrations")
	}
}

// TestVerifyRejectsAClosedDatabase covers the connection failing, which must be
// distinguishable from work being outstanding: a service retries one and
// refuses to start on the other.
func TestVerifyRejectsAClosedDatabase(t *testing.T) {
	db := newDatabase(t)

	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	err := mig.Verify(t.Context(), db, migrations(t))
	if err == nil {
		t.Fatal("verify accepted a closed database")
	}

	if errors.Is(err, mig.ErrPending) {
		t.Fatal("a broken connection was reported as pending work")
	}
}

// TestFencedIsReexported keeps the sentinel a caller matches on from drifting
// away from the one the ledger raises.
func TestFencedIsReexported(t *testing.T) {
	if mig.ErrFenced == nil {
		t.Fatal("ErrFenced is nil")
	}
}
