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

package history_test

import (
	"testing"
	"testing/fstest"

	pgquery "github.com/pganalyze/pg_query_go/v6"

	"github.com/tochemey/mig/internal/lint/history"
	"github.com/tochemey/mig/internal/parse"
	"github.com/tochemey/mig/internal/plan"
)

// build loads a two-file plan and reads its history. The first file carries
// the past: a table with typed columns, its index, a sequence-backed column,
// a view, and a grant lives only in the second.
func build(t *testing.T) *history.History {
	t.Helper()

	fsys := fstest.MapFS{
		"1_past.sql": &fstest.MapFile{Data: []byte(`
CREATE SEQUENCE orders_id_seq;
CREATE TABLE orders (
    id integer NOT NULL DEFAULT nextval('orders_id_seq'::regclass),
    note varchar(100),
    total numeric(10, 2)
);
CREATE INDEX idx_orders_note ON orders (note);
CREATE VIEW busy_orders AS
WITH latest AS (SELECT id FROM orders)
SELECT latest.id FROM latest;
CREATE VIEW order_owners AS SELECT o.id FROM orders o JOIN users u ON u.id = o.id;
`)},
		"2_present.sql": &fstest.MapFile{Data: []byte(`
DROP TABLE orders;
CREATE TABLE orders (id bigint);
ALTER TABLE users ADD COLUMN nickname varchar(64);
ALTER TABLE users ALTER COLUMN nickname TYPE text;
GRANT SELECT ON users TO app;
`)},
	}

	loaded, err := plan.LoadFS(fsys)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	return history.Build(loaded)
}

func TestFreshAtFollowsIncarnations(t *testing.T) {
	h := build(t)

	cases := []struct {
		name     string
		relation string
		file     string
		stmt     int
		want     bool
	}{
		{name: "a creation's own lock is on the fresh table", relation: "orders", file: "1_past.sql", stmt: 1, want: true},
		{name: "the index it builds stays fresh", relation: "idx_orders_note", file: "1_past.sql", stmt: 2, want: true},
		{name: "a later file locks the pre-existing table", relation: "orders", file: "2_present.sql", stmt: 0, want: false},
		{name: "the recreation is fresh again", relation: "orders", file: "2_present.sql", stmt: 1, want: true},
		{name: "a relation the plan never creates", relation: "legacy", file: "2_present.sql", stmt: 0, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := h.FreshAt(tc.relation, tc.file, 0, tc.stmt); got != tc.want {
				t.Errorf("FreshAt(%s, %s, %d) = %v, want %v", tc.relation, tc.file, tc.stmt, got, tc.want)
			}
		})
	}
}

func TestEverCreated(t *testing.T) {
	h := build(t)

	if !h.EverCreated("orders") || !h.EverCreated("busy_orders") || !h.EverCreated("orders_id_seq") {
		t.Error("a created relation went unrecorded")
	}

	if h.EverCreated("users") {
		t.Error("users is never created by this plan")
	}
}

func TestParentCoversIndexesAndSequences(t *testing.T) {
	h := build(t)

	if parent, ok := h.Parent("idx_orders_note"); !ok || parent != "orders" {
		t.Errorf("index parent = %q, %v", parent, ok)
	}

	if parent, ok := h.Parent("orders_id_seq"); !ok || parent != "orders" {
		t.Errorf("sequence parent = %q, %v", parent, ok)
	}

	if _, ok := h.Parent("orders"); ok {
		t.Error("a table has no parent")
	}
}

func TestViewBasesReachThroughCtesAndJoins(t *testing.T) {
	h := build(t)

	bases := h.ViewBases("busy_orders")
	if !contains(bases, "orders") {
		t.Errorf("the CTE's base is missing: %v", bases)
	}

	joined := h.ViewBases("order_owners")
	if !contains(joined, "orders") || !contains(joined, "users") {
		t.Errorf("the join's bases are missing: %v", joined)
	}

	if h.ViewBases("orders") != nil {
		t.Error("a table has no view bases")
	}
}

func TestHasGrants(t *testing.T) {
	if !build(t).HasGrants() {
		t.Error("the plan grants and the history says otherwise")
	}
}

func TestColumnTypeBeforeTracksTheLatestChange(t *testing.T) {
	h := build(t)

	// The type as CREATE TABLE spelled it, read from a later file.
	spec, ok := h.ColumnTypeBefore("orders", "note", "2_present.sql", 0, 0)
	if !ok || spec.Name != "varchar" || len(spec.Mods) != 1 || spec.Mods[0] != 100 {
		t.Errorf("note = %+v, %v", spec, ok)
	}

	if spec, _ := h.ColumnTypeBefore("orders", "total", "2_present.sql", 0, 0); spec.Name != "numeric" {
		t.Errorf("total = %+v", spec)
	}

	// ADD COLUMN sets the type; the ALTER after it changes what a still later
	// statement would see.
	if spec, _ := h.ColumnTypeBefore("users", "nickname", "2_present.sql", 0, 3); spec.Name != "varchar" {
		t.Errorf("nickname before the alter = %+v", spec)
	}

	if spec, _ := h.ColumnTypeBefore("users", "nickname", "2_present.sql", 0, 4); spec.Name != "text" {
		t.Errorf("nickname after the alter = %+v", spec)
	}

	if _, ok := h.ColumnTypeBefore("users", "id", "2_present.sql", 0, 0); ok {
		t.Error("a column the plan never types came back known")
	}
}

func TestColumnAddedInFile(t *testing.T) {
	h := build(t)

	if !h.ColumnAddedInFile("users", "nickname", "2_present.sql", 0, 4) {
		t.Error("the added column went unseen")
	}

	// The same column asked about from a different file is someone else's.
	if h.ColumnAddedInFile("users", "nickname", "1_past.sql", 0, 0) {
		t.Error("an addition in another file is not this file's")
	}
}

// TestBuildCoversTheRarerStatements pins the recorder's remaining shapes: a
// CREATE TABLE AS creation, defaults that are not sequences, a revoke that
// is not a grant, a dropped trigger that must not pass for a relation, a
// view over a subselect, and a statement that does not parse, which records
// nothing.
func TestBuildCoversTheRarerStatements(t *testing.T) {
	loaded := &plan.Plan{Migrations: []plan.Migration{{
		File: "1_odd.sql",
		Steps: []plan.Step{{Statements: []parse.Statement{
			{SQL: "CREATE TABLE snapshots AS SELECT 1 AS one"},
			{SQL: "CREATE TABLE flags (on_by_default boolean DEFAULT true, " +
				"stamp text DEFAULT lower('X'), " +
				"seed float DEFAULT random(), " +
				"odd integer DEFAULT nextval(42))"},
			{SQL: "REVOKE SELECT ON flags FROM app"},
			{SQL: "DROP TRIGGER flags ON snapshots"},
			{SQL: "CREATE VIEW summarized AS SELECT one FROM (SELECT 1 AS one) inner_rows"},
			{SQL: "this does not parse"},
		}}},
	}}}

	h := history.Build(loaded)

	if !h.EverCreated("snapshots") {
		t.Error("CREATE TABLE AS went unrecorded")
	}

	if h.HasGrants() {
		t.Error("a revoke is not grant discipline")
	}

	if _, ok := h.Parent("flags"); ok {
		t.Error("no default here draws from a sequence")
	}

	// The dropped trigger shares the table's name; recording it would make
	// the still-standing table look freshly incarnated at the drop.
	if !h.FreshAt("flags", "1_odd.sql", 0, 5) {
		t.Error("the trigger drop was mistaken for dropping the table")
	}

	if bases := h.ViewBases("summarized"); len(bases) != 0 {
		t.Errorf("a subselect names no relation, got %v", bases)
	}
}

// TestFreshAtAcrossSteps pins statement order between steps of one file.
func TestFreshAtAcrossSteps(t *testing.T) {
	fsys := fstest.MapFS{"1_steps.sql": &fstest.MapFile{Data: []byte(`
-- +mig step: first
CREATE TABLE staging (id int);

-- +mig step: second
DROP TABLE staging;
`)}}

	loaded, err := plan.LoadFS(fsys)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	h := history.Build(loaded)

	if !h.FreshAt("staging", "1_steps.sql", 1, 0) {
		t.Error("a table created in the previous step of this file is its own")
	}
}

// TestNilHistoryAnswersConservatively covers the caller without a plan.
func TestNilHistoryAnswersConservatively(t *testing.T) {
	var h *history.History

	if h.FreshAt("t", "f", 0, 0) || h.EverCreated("t") || h.HasGrants() {
		t.Error("a nil history claims knowledge")
	}

	if _, ok := h.Parent("i"); ok {
		t.Error("a nil history knows a parent")
	}

	if h.ViewBases("v") != nil {
		t.Error("a nil history knows a view")
	}

	if _, ok := h.ColumnTypeBefore("t", "c", "f", 0, 0); ok {
		t.Error("a nil history knows a type")
	}

	if h.ColumnAddedInFile("t", "c", "f", 0, 0) {
		t.Error("a nil history saw an addition")
	}
}

// TestSpecOf pins the exported reader against a parse tree and its absence.
func TestSpecOf(t *testing.T) {
	if _, ok := history.SpecOf(&pgquery.TypeName{}); ok {
		t.Error("an empty type name has no spec")
	}
}

// contains reports membership.
func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}

	return false
}
