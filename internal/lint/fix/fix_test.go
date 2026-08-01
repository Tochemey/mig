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
	"strings"
	"testing"

	pgquery "github.com/pganalyze/pg_query_go/v6"
)

// stmt parses one statement for a hand-built step.
func stmt(t *testing.T, sql string) *pgquery.Node {
	t.Helper()

	tree, err := pgquery.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}

	return tree.Stmts[0].GetStmt()
}

func TestRenderAnnotatesEverySetting(t *testing.T) {
	f := &Fix{Steps: []Step{
		{
			Name:    "one",
			Comment: "why one exists",
			Stmts:   []*pgquery.Node{stmt(t, "SELECT 1")},
		},
		{
			Name:    "two",
			Comment: "why two exists",
			NoTx:    true,
			Backfill: &Backfill{
				Table:     "t",
				Key:       "id",
				Batch:     100,
				Satisfied: stmt(t, "SELECT true"),
			},
			Stmts: []*pgquery.Node{stmt(t, "UPDATE t SET c = 1 WHERE id > $1 AND id <= $2")},
		},
	}}

	got, err := f.Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	want := "-- +mig step: one\n" +
		"-- why one exists\n" +
		"SELECT 1;\n" +
		"\n" +
		"-- +mig step: two\n" +
		"-- +mig notx\n" +
		"-- +mig backfill: table=t key=id batch=100\n" +
		"-- +mig satisfied: sql(SELECT true)\n" +
		"-- why two exists\n" +
		"UPDATE t SET c = 1 WHERE id > :cursor_lo AND id <= :cursor_hi;\n"

	if got != want {
		t.Errorf("rendered:\n%q\nwant:\n%q", got, want)
	}
}

func TestRenderScaffoldCommentsEverything(t *testing.T) {
	f := &Fix{
		Scaffold: true,
		TODO:     "decide something first",
		Steps: []Step{{
			Name:    "planned",
			Comment: "why\n\nwith a gap",
			Stmts:   []*pgquery.Node{stmt(t, "SELECT 1")},
		}},
	}

	got, err := f.Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	if !strings.HasPrefix(got, "-- TODO: decide something first\n") {
		t.Errorf("scaffold does not lead with its TODO:\n%s", got)
	}

	for line := range strings.SplitSeq(strings.TrimRight(got, "\n"), "\n") {
		if line != "" && line != "--" && !strings.HasPrefix(line, "-- ") {
			t.Errorf("scaffold line %q is not commented out", line)
		}
	}

	if !strings.Contains(got, "-- -- +mig step: planned\n") {
		t.Errorf("scaffold lost its step annotation:\n%s", got)
	}
}

func TestRenderReportsAnUndeparsableStatement(t *testing.T) {
	f := &Fix{Steps: []Step{{Name: "broken", Comment: "c", Stmts: []*pgquery.Node{{}}}}}

	if _, err := f.Render(); err == nil {
		t.Error("an empty node rendered without error")
	}
}

func TestRenderReportsAnUndeparsablePredicate(t *testing.T) {
	f := &Fix{Steps: []Step{{
		Name:     "broken",
		Comment:  "c",
		Backfill: &Backfill{Table: "t", Key: "id", Batch: 1, Satisfied: &pgquery.Node{}},
		Stmts:    []*pgquery.Node{stmt(t, "SELECT 1")},
	}}}

	if _, err := f.Render(); err == nil {
		t.Error("an empty predicate rendered without error")
	}
}
