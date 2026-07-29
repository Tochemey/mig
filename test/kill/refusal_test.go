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

package kill_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tochemey/mig/test/harness"
)

// TestAnUnreconcilableStepStopsTheRunBeforeItStarts is the refusal the design
// trades for a dirty flag.
//
// A non-transactional step with no way to recognise its own finished work
// cannot converge after a crash. Refusing it is only worth anything if it
// happens before any migration is applied: a run that applies the first
// migration and then refuses the second leaves exactly the half-migrated
// database nobody can reason about, which is what the refusal exists to
// prevent.
func TestAnUnreconcilableStepStopsTheRunBeforeItStarts(t *testing.T) {
	t.Parallel()

	if shared == nil {
		t.Skip("postgres container not available")
	}

	database := newDatabase(t)
	dir := t.TempDir()

	files := map[string]string{
		"20240101000000_first.sql": "ALTER TABLE users ADD COLUMN email text;\n",
		"20240202000000_second.sql": `-- +mig step: unreconcilable
-- +mig notx
ALTER TYPE mood ADD VALUE 'ok';
`,
	}

	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %q: %v", name, err)
		}
	}

	proc, err := harness.Start(migBin,
		[]string{"up", "--dsn", shared.DSN(database), "--dir", dir, "--lease-ttl", "3s"}, nil)
	if err != nil {
		t.Fatalf("start mig: %v", err)
	}

	t.Cleanup(proc.Cleanup)

	code, err := proc.Wait(time.Minute)
	if err != nil {
		t.Fatalf("wait for mig: %v", err)
	}

	if code == 0 {
		t.Fatalf("mig accepted an unreconcilable step\nstdout: %s", proc.Stdout())
	}

	if !strings.Contains(proc.Stderr(), "satisfied:") {
		t.Fatalf("the refusal does not say how to fix it: %s", proc.Stderr())
	}

	db, err := shared.Open(t.Context(), database)
	if err != nil {
		t.Fatalf("open %q: %v", database, err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close %q: %v", database, err)
		}
	})

	// The first migration is applied only if the refusal came too late.
	if columnExists(t, db) {
		t.Fatal("the first migration was applied before the second was refused")
	}
}

// columnExists reports whether the first migration's column is present.
func columnExists(t *testing.T, db *sql.DB) bool {
	t.Helper()

	const query = `SELECT exists(
        SELECT 1 FROM information_schema.columns
         WHERE table_name = 'users' AND column_name = 'email')`

	var exists bool

	if err := db.QueryRowContext(t.Context(), query).Scan(&exists); err != nil {
		t.Fatalf("check for column: %v", err)
	}

	return exists
}
