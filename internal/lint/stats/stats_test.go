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

package stats

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/tochemey/mig/internal/lint/lockmodel"
	"github.com/tochemey/mig/test/harness"
)

// users is the relation the seeded template holds.
var users = lockmodel.Relation{Name: "users"}

// shared is the container for this package, or nil when docker is absent.
var shared *harness.Harness

// TestMain brings up one container for the package.
func TestMain(m *testing.M) {
	os.Exit(harness.Main(m, 0, func(_ context.Context, h *harness.Harness) error {
		shared = h
		return nil
	}))
}

func TestCollectReadsSizeAndKey(t *testing.T) {
	db := newDatabase(t)

	exec(t, db, "INSERT INTO users SELECT g, 'n' || g, 'e' || g FROM generate_series(1, 500) g")
	exec(t, db, "ANALYZE users")

	snapshot, err := Collect(t.Context(), db, []lockmodel.Relation{users})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}

	table := snapshot.Table(users)

	if !table.Exists {
		t.Fatal("the seeded table was reported absent")
	}

	if table.Rows != 500 {
		t.Errorf("rows = %d, want the 500 that were inserted", table.Rows)
	}

	if table.Bytes <= 0 {
		t.Errorf("bytes = %d, want a size on disk", table.Bytes)
	}

	if len(table.PrimaryKey) != 1 || table.PrimaryKey[0] != "id" {
		t.Errorf("primary key = %v, want [id]", table.PrimaryKey)
	}
}

// TestCollectReportsAnAbsentRelation covers the ordinary case of linting a
// migration before it runs: the tables it creates are not there yet, and that
// supports no claim about size rather than being an error.
func TestCollectReportsAnAbsentRelation(t *testing.T) {
	db := newDatabase(t)

	unborn := lockmodel.Relation{Name: "not_created_yet"}

	snapshot, err := Collect(t.Context(), db, []lockmodel.Relation{unborn, unborn})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}

	if table := snapshot.Table(unborn); table.Exists || table.Bytes != 0 {
		t.Errorf("absent relation reported as %+v", table)
	}
}

// TestNilSnapshotIsOffline covers the offline path: every lookup answers the
// same as a relation nobody knows about, so no rule has to check for nil.
func TestNilSnapshotIsOffline(t *testing.T) {
	var snapshot *Snapshot

	if table := snapshot.Table(users); table.Exists {
		t.Errorf("offline lookup reported %+v", table)
	}

	if snapshot.Throughput().Known() {
		t.Error("offline throughput reported as measured")
	}
}

// swapQuerier answers one of the collector's queries with another, which is
// how the reads after the first one are made to fail against a server that is
// working perfectly well.
type swapQuerier struct {
	db   *sql.DB
	from string
	to   string
}

func (q swapQuerier) QueryContext(ctx context.Context, text string, args ...any) (*sql.Rows, error) {
	if text == q.from {
		return q.db.QueryContext(ctx, q.to)
	}

	return q.db.QueryContext(ctx, text, args...)
}

func (q swapQuerier) QueryRowContext(ctx context.Context, text string, args ...any) *sql.Row {
	if text == q.from {
		return q.db.QueryRowContext(ctx, q.to)
	}

	return q.db.QueryRowContext(ctx, text, args...)
}

// TestCollectReportsEachReadThatFails covers the reads behind the first one,
// each in its own way of going wrong: refused outright, answering with
// something unscannable, and failing partway through the rows.
func TestCollectReportsEachReadThatFails(t *testing.T) {
	cases := []struct {
		name string
		from string
		to   string
	}{
		{name: "the key is refused", from: keyQuery, to: "SELECT no_such_function()"},
		{name: "the key comes back unscannable", from: keyQuery, to: "SELECT NULL::text"},
		{
			name: "the key fails partway through",
			from: keyQuery, to: "SELECT (1 / (2 - g))::text FROM generate_series(1, 3) g",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newDatabase(t)

			q := swapQuerier{db: db, from: tc.from, to: tc.to}

			if _, err := Collect(t.Context(), q, []lockmodel.Relation{users}); err == nil {
				t.Error("collect reported success")
			}
		})
	}
}

// TestOfCarriesSuppliedSizes covers the snapshot a caller builds itself,
// which is what everything downstream is tested against.
func TestOfCarriesSuppliedSizes(t *testing.T) {
	measured := Throughput{Rewrite: 1 << 20, IndexBuild: 1 << 20, RewriteRows: 1000, IndexRows: 1000}

	snapshot := Of(map[lockmodel.Relation]Table{
		users: {Exists: true, Rows: 500, Bytes: 1 << 20},
	}).WithThroughput(measured)

	if table := snapshot.Table(users); !table.Exists || table.Rows != 500 {
		t.Errorf("table = %+v, want the supplied sizes", table)
	}

	if snapshot.Throughput() != measured {
		t.Errorf("throughput = %+v, want %+v", snapshot.Throughput(), measured)
	}
}

// TestCollectFailsOnAClosedDatabase covers the error path every query here
// shares.
func TestCollectFailsOnAClosedDatabase(t *testing.T) {
	db := newDatabase(t)

	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, err := Collect(t.Context(), db, []lockmodel.Relation{users}); err == nil {
		t.Error("collect reported success against a closed pool")
	}

	if _, err := ServerMajor(t.Context(), db); err == nil {
		t.Error("server version read from a closed pool")
	}
}

func TestServerMajorReadsTheRunningServer(t *testing.T) {
	db := newDatabase(t)

	major, err := ServerMajor(t.Context(), db)
	if err != nil {
		t.Fatalf("server major: %v", err)
	}

	if major != shared.Major() {
		t.Errorf("major = %d, want the container's %d", major, shared.Major())
	}
}

// newDatabase gives one test its own clone of the seeded template.
func newDatabase(t *testing.T) *sql.DB {
	t.Helper()

	if shared == nil {
		t.Skip("postgres container not available")
	}

	name, err := shared.Clone(t.Context(), harness.Template)
	if err != nil {
		t.Fatalf("clone template: %v", err)
	}

	db, err := shared.Open(t.Context(), name)
	if err != nil {
		t.Fatalf("open clone: %v", err)
	}

	t.Cleanup(func() {
		// One test closes the pool itself; closing an already-closed pool is
		// a no-op, so this does not report it twice.
		if err := db.Close(); err != nil {
			t.Errorf("close pool: %v", err)
		}

		if err := shared.DropDatabase(context.Background(), name); err != nil {
			t.Errorf("drop clone %q: %v", name, err)
		}
	})

	return db
}

// exec runs a statement or fails the test.
func exec(t *testing.T, db *sql.DB, stmt string) {
	t.Helper()

	if _, err := db.ExecContext(t.Context(), stmt); err != nil {
		t.Fatalf("exec %q: %v", stmt, err)
	}
}
