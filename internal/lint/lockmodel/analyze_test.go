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
	"reflect"
	"strings"
	"testing"

	pgquery "github.com/pganalyze/pg_query_go/v6"

	"github.com/tochemey/mig/internal/lint/lockmodel"
)

// current is the newest supported major, used wherever a case is not about a
// version-sensitive behaviour.
const current = 18

// rel shortens an unqualified relation.
func rel(name string) lockmodel.Relation {
	return lockmodel.Relation{Name: name}
}

// analyze runs the model and fails the test on error.
func analyze(t *testing.T, sql string, version int) lockmodel.Analysis {
	t.Helper()

	analysis, err := lockmodel.Analyze(sql, version)
	if err != nil {
		t.Fatalf("Analyze(%q): %v", sql, err)
	}

	// A handler that found nothing may hand back an empty slice; the cases
	// spell that as nil.
	if len(analysis.Effects) == 0 {
		analysis.Effects = nil
	}

	return analysis
}

// analyzeCase is one statement and its predicted analysis.
type analyzeCase struct {
	name    string
	sql     string
	version int
	want    lockmodel.Analysis
}

// run checks every case's full analysis, reasons included.
func run(t *testing.T, cases []analyzeCase) {
	t.Helper()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			version := tc.version
			if version == 0 {
				version = current
			}

			got := analyze(t, tc.sql, version)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Analyze(%q, %d)\n got %+v\nwant %+v", tc.sql, version, got, tc.want)
			}
		})
	}
}

func TestAnalyzeStatement(t *testing.T) {
	const sql = "CREATE INDEX idx ON users (name)"

	tree, err := pgquery.Parse(sql)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	fromTree := lockmodel.AnalyzeStatement(tree.Stmts[0], current)
	fromText := analyze(t, sql, current)

	if !reflect.DeepEqual(fromTree, fromText) {
		t.Errorf("AnalyzeStatement = %+v, Analyze = %+v", fromTree, fromText)
	}
}

func TestAnalyzeErrors(t *testing.T) {
	if _, err := lockmodel.Analyze("NOT SQL AT ALL", current); err == nil {
		t.Error("garbage was analysed without error")
	}

	_, err := lockmodel.Analyze("SELECT 1; SELECT 2", current)
	if err == nil || !strings.Contains(err.Error(), "got 2") {
		t.Errorf("two statements analysed as one: %v", err)
	}

	if _, err := lockmodel.Analyze("", current); err == nil {
		t.Error("an empty string was analysed without error")
	}
}

func TestAnalyzeIndexes(t *testing.T) {
	run(t, []analyzeCase{
		{
			name: "create index takes SHARE for the whole build",
			sql:  "CREATE INDEX idx ON users (name)",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{{
				Relation: rel("users"), Mode: lockmodel.Share, Duration: lockmodel.IndexBuild,
				Reason: "index build blocks writes for its whole duration",
			}}},
		},
		{
			name: "create index concurrently keeps writes flowing and refuses a transaction",
			sql:  "CREATE UNIQUE INDEX CONCURRENTLY idx ON app.users (name)",
			want: lockmodel.Analysis{NoTx: true, Effects: []lockmodel.LockEffect{{
				Relation: lockmodel.Relation{Schema: "app", Name: "users"},
				Mode:     lockmodel.ShareUpdateExclusive, Duration: lockmodel.IndexBuild,
				Reason: "concurrent index build: two scans, waits out open transactions",
			}}},
		},
		{
			name: "reindex index blocks every use of the index",
			sql:  "REINDEX INDEX idx",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{{
				Relation: rel("idx"), Mode: lockmodel.AccessExclusive, Duration: lockmodel.IndexBuild,
				Reason: "index rebuild blocks every use of the index",
			}}},
		},
		{
			name: "reindex index concurrently",
			sql:  "REINDEX (CONCURRENTLY) INDEX idx",
			want: lockmodel.Analysis{NoTx: true, Effects: []lockmodel.LockEffect{{
				Relation: rel("idx"), Mode: lockmodel.ShareUpdateExclusive, Duration: lockmodel.IndexBuild,
				Reason: "concurrent index rebuild: two scans, waits out open transactions",
			}}},
		},
		{
			name: "reindex table holds SHARE across every index",
			sql:  "REINDEX TABLE users",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{{
				Relation: rel("users"), Mode: lockmodel.Share, Duration: lockmodel.IndexBuild,
				Reason: "rebuilding every index blocks writes for the whole build",
			}}},
		},
		{
			name: "reindex table concurrently",
			sql:  "REINDEX (CONCURRENTLY) TABLE users",
			want: lockmodel.Analysis{NoTx: true, Effects: []lockmodel.LockEffect{{
				Relation: rel("users"), Mode: lockmodel.ShareUpdateExclusive, Duration: lockmodel.IndexBuild,
				Reason: "concurrent index rebuild: two scans, waits out open transactions",
			}}},
		},
		{
			name: "reindex database locks nothing the statement names",
			sql:  "REINDEX DATABASE mig",
			want: lockmodel.Analysis{NoTx: true},
		},
	})
}

func TestAnalyzeDrops(t *testing.T) {
	run(t, []analyzeCase{
		{
			name: "drop table",
			sql:  "DROP TABLE users",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{{
				Relation: rel("users"), Mode: lockmodel.AccessExclusive, Duration: lockmodel.Instant,
				Reason: "catalog only: drop table",
			}}},
		},
		{
			name: "drop several tables locks each",
			sql:  "DROP TABLE a, app.b",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{
				{Relation: rel("a"), Mode: lockmodel.AccessExclusive, Duration: lockmodel.Instant,
					Reason: "catalog only: drop table"},
				{Relation: lockmodel.Relation{Schema: "app", Name: "b"},
					Mode: lockmodel.AccessExclusive, Duration: lockmodel.Instant,
					Reason: "catalog only: drop table"},
			}},
		},
		{
			name: "drop index concurrently refuses a transaction",
			sql:  "DROP INDEX CONCURRENTLY idx",
			want: lockmodel.Analysis{NoTx: true, Effects: []lockmodel.LockEffect{{
				Relation: rel("idx"), Mode: lockmodel.ShareUpdateExclusive, Duration: lockmodel.Instant,
				Reason: "drop index waits for every transaction using the index",
			}}},
		},
		{
			name: "drop view",
			sql:  "DROP VIEW v",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{{
				Relation: rel("v"), Mode: lockmodel.AccessExclusive, Duration: lockmodel.Instant,
				Reason: "catalog only: drop view",
			}}},
		},
		{
			name: "drop materialized view",
			sql:  "DROP MATERIALIZED VIEW mv",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{{
				Relation: rel("mv"), Mode: lockmodel.AccessExclusive, Duration: lockmodel.Instant,
				Reason: "catalog only: drop materialized view",
			}}},
		},
		{
			name: "drop sequence",
			sql:  "DROP SEQUENCE s",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{{
				Relation: rel("s"), Mode: lockmodel.AccessExclusive, Duration: lockmodel.Instant,
				Reason: "catalog only: drop sequence",
			}}},
		},
		{
			name: "drop type locks no relation",
			sql:  "DROP TYPE mood",
			want: lockmodel.Analysis{},
		},
		{
			name: "a cross-database name is not a relation",
			sql:  "DROP TABLE otherdb.app.users",
			want: lockmodel.Analysis{},
		},
	})
}

func TestAnalyzeRenames(t *testing.T) {
	run(t, []analyzeCase{
		{
			name: "rename table",
			sql:  "ALTER TABLE users RENAME TO people",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{{
				Relation: rel("users"), Mode: lockmodel.AccessExclusive, Duration: lockmodel.Instant,
				Reason: "catalog only: rename table",
			}}},
		},
		{
			name: "rename column",
			sql:  "ALTER TABLE users RENAME COLUMN a TO b",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{{
				Relation: rel("users"), Mode: lockmodel.AccessExclusive, Duration: lockmodel.Instant,
				Reason: "catalog only: rename column",
			}}},
		},
		{
			name: "rename constraint",
			sql:  "ALTER TABLE users RENAME CONSTRAINT a TO b",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{{
				Relation: rel("users"), Mode: lockmodel.AccessExclusive, Duration: lockmodel.Instant,
				Reason: "catalog only: rename constraint",
			}}},
		},
		{
			name: "rename view",
			sql:  "ALTER VIEW v RENAME TO w",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{{
				Relation: rel("v"), Mode: lockmodel.AccessExclusive, Duration: lockmodel.Instant,
				Reason: "catalog only: rename view",
			}}},
		},
		{
			name: "rename materialized view",
			sql:  "ALTER MATERIALIZED VIEW mv RENAME TO mv2",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{{
				Relation: rel("mv"), Mode: lockmodel.AccessExclusive, Duration: lockmodel.Instant,
				Reason: "catalog only: rename materialized view",
			}}},
		},
		{
			name: "rename index needs only SHARE UPDATE EXCLUSIVE from 12",
			sql:  "ALTER INDEX idx RENAME TO idx2",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{{
				Relation: rel("idx"), Mode: lockmodel.ShareUpdateExclusive, Duration: lockmodel.Instant,
				Reason: "catalog only: rename index",
			}}},
		},
		{
			name:    "rename index before 12 takes ACCESS EXCLUSIVE",
			sql:     "ALTER INDEX idx RENAME TO idx2",
			version: 11,
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{{
				Relation: rel("idx"), Mode: lockmodel.AccessExclusive, Duration: lockmodel.Instant,
				Reason: "catalog only: rename index",
			}}},
		},
		{
			name: "rename type locks no relation",
			sql:  "ALTER TYPE mood RENAME TO feeling",
			want: lockmodel.Analysis{},
		},
	})
}

func TestAnalyzeMaintenance(t *testing.T) {
	run(t, []analyzeCase{
		{
			name: "truncate locks every named table",
			sql:  "TRUNCATE users, app.events",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{
				{Relation: rel("users"), Mode: lockmodel.AccessExclusive, Duration: lockmodel.Instant,
					Reason: "truncate replaces the storage without scanning it"},
				{Relation: lockmodel.Relation{Schema: "app", Name: "events"},
					Mode: lockmodel.AccessExclusive, Duration: lockmodel.Instant,
					Reason: "truncate replaces the storage without scanning it"},
			}},
		},
		{
			name: "vacuum full rewrites under ACCESS EXCLUSIVE",
			sql:  "VACUUM FULL users",
			want: lockmodel.Analysis{NoTx: true, Effects: []lockmodel.LockEffect{{
				Relation: rel("users"), Mode: lockmodel.AccessExclusive, Duration: lockmodel.Rewrite,
				Reason: "table rewrite: vacuum full copies the table",
			}}},
		},
		{
			name: "plain vacuum blocks no traffic",
			sql:  "VACUUM users",
			want: lockmodel.Analysis{NoTx: true, Effects: []lockmodel.LockEffect{{
				Relation: rel("users"), Mode: lockmodel.ShareUpdateExclusive, Duration: lockmodel.Scan,
				Reason: "vacuum scans the table without blocking traffic",
			}}},
		},
		{
			name: "bare vacuum names nothing the model can predict",
			sql:  "VACUUM",
			want: lockmodel.Analysis{NoTx: true},
		},
		{
			name: "analyze runs inside a transaction",
			sql:  "ANALYZE users",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{{
				Relation: rel("users"), Mode: lockmodel.ShareUpdateExclusive, Duration: lockmodel.Scan,
				Reason: "analyze samples the table without blocking traffic",
			}}},
		},
		{
			name: "cluster rewrites the table and its ordering index",
			sql:  "CLUSTER users USING idx",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{
				{Relation: rel("users"), Mode: lockmodel.AccessExclusive, Duration: lockmodel.Rewrite,
					Reason: "table rewrite: cluster copies the table in index order"},
				{Relation: rel("idx"), Mode: lockmodel.AccessExclusive, Duration: lockmodel.Rewrite,
					Implicit: true, Reason: "cluster rebuilds the ordering index"},
			}},
		},
		{
			name: "cluster on the remembered index",
			sql:  "CLUSTER users",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{{
				Relation: rel("users"), Mode: lockmodel.AccessExclusive, Duration: lockmodel.Rewrite,
				Reason: "table rewrite: cluster copies the table in index order",
			}}},
		},
		{
			name: "bare cluster refuses a transaction and names nothing",
			sql:  "CLUSTER",
			want: lockmodel.Analysis{NoTx: true},
		},
		{
			name: "refresh materialized view blocks reads until done",
			sql:  "REFRESH MATERIALIZED VIEW mv",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{{
				Relation: rel("mv"), Mode: lockmodel.AccessExclusive, Duration: lockmodel.Rewrite,
				Reason: "refresh replaces the contents, blocking reads until done",
			}}},
		},
		{
			name: "refresh concurrently keeps reads flowing",
			sql:  "REFRESH MATERIALIZED VIEW CONCURRENTLY mv",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{{
				Relation: rel("mv"), Mode: lockmodel.Exclusive, Duration: lockmodel.Scan,
				Reason: "concurrent refresh diffs and merges, blocking writes only",
			}}},
		},
		{
			name: "lock table records the requested mode",
			sql:  "LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{{
				Relation: rel("users"), Mode: lockmodel.ShareRowExclusive, Duration: lockmodel.Instant,
				Reason: "explicit lock, held to the end of the transaction",
			}}},
		},
		{
			name: "lock table defaults to ACCESS EXCLUSIVE",
			sql:  "LOCK TABLE users",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{{
				Relation: rel("users"), Mode: lockmodel.AccessExclusive, Duration: lockmodel.Instant,
				Reason: "explicit lock, held to the end of the transaction",
			}}},
		},
	})
}

func TestAnalyzeCreateTable(t *testing.T) {
	run(t, []analyzeCase{
		{
			name: "an inline foreign key locks the referenced table",
			sql:  "CREATE TABLE t (id int REFERENCES parents (id))",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{{
				Relation: rel("parents"), Mode: lockmodel.ShareRowExclusive,
				Duration: lockmodel.Instant, Implicit: true,
				Reason: "foreign key locks the referenced table",
			}}},
		},
		{
			name: "a table-level foreign key locks the referenced table",
			sql:  "CREATE TABLE t (id int, FOREIGN KEY (id) REFERENCES app.parents (id))",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{{
				Relation: lockmodel.Relation{Schema: "app", Name: "parents"},
				Mode:     lockmodel.ShareRowExclusive, Duration: lockmodel.Instant, Implicit: true,
				Reason: "foreign key locks the referenced table",
			}}},
		},
		{
			name: "a plain create table locks nothing that exists",
			sql:  "CREATE TABLE t (id int PRIMARY KEY, name text)",
			want: lockmodel.Analysis{},
		},
		{
			name: "like copies no locks",
			sql:  "CREATE TABLE t (LIKE u)",
			want: lockmodel.Analysis{},
		},
		{
			name: "a table-level check references nothing",
			sql:  "CREATE TABLE t (id int, CHECK (id > 0))",
			want: lockmodel.Analysis{},
		},
		{
			name: "creating a partition locks the parent",
			sql:  "CREATE TABLE p1 PARTITION OF parted FOR VALUES FROM (1) TO (10)",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{{
				Relation: rel("parted"), Mode: lockmodel.AccessExclusive,
				Duration: lockmodel.Instant, Implicit: true,
				Reason: "creating a partition locks the parent",
			}}},
		},
		{
			name: "plain inheritance is not a partition",
			sql:  "CREATE TABLE c () INHERITS (p)",
			want: lockmodel.Analysis{},
		},
	})
}

func TestAnalyzeEnum(t *testing.T) {
	run(t, []analyzeCase{
		{
			name: "adding an enum value runs inside a transaction from 12",
			sql:  "ALTER TYPE mood ADD VALUE 'curious'",
			want: lockmodel.Analysis{},
		},
		{
			name:    "adding an enum value refuses a transaction before 12",
			sql:     "ALTER TYPE mood ADD VALUE 'curious'",
			version: 11,
			want:    lockmodel.Analysis{NoTx: true},
		},
	})
}

func TestAnalyzeQueries(t *testing.T) {
	run(t, []analyzeCase{
		{
			name: "select",
			sql:  "SELECT * FROM users",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{{
				Relation: rel("users"), Mode: lockmodel.AccessShare, Duration: lockmodel.Scan,
				Reason: "read",
			}}},
		},
		{
			name: "select without a table",
			sql:  "SELECT 1",
			want: lockmodel.Analysis{},
		},
		{
			name: "a join and a subselect are both read",
			sql:  "SELECT * FROM a JOIN (SELECT * FROM b) s ON true",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{
				{Relation: rel("a"), Mode: lockmodel.AccessShare, Duration: lockmodel.Scan,
					Reason: "read"},
				{Relation: rel("b"), Mode: lockmodel.AccessShare, Duration: lockmodel.Scan,
					Reason: "read"},
			}},
		},
		{
			name: "a set operation reads both sides",
			sql:  "SELECT id FROM a UNION SELECT id FROM b",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{
				{Relation: rel("a"), Mode: lockmodel.AccessShare, Duration: lockmodel.Scan,
					Reason: "read"},
				{Relation: rel("b"), Mode: lockmodel.AccessShare, Duration: lockmodel.Scan,
					Reason: "read"},
			}},
		},
		{
			name: "a table function is not a relation",
			sql:  "SELECT * FROM generate_series(1, 10)",
			want: lockmodel.Analysis{},
		},
		{
			name: "for update upgrades every read relation",
			sql:  "SELECT * FROM users FOR UPDATE",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{
				{Relation: rel("users"), Mode: lockmodel.AccessShare, Duration: lockmodel.Scan,
					Reason: "read"},
				{Relation: rel("users"), Mode: lockmodel.RowShare, Duration: lockmodel.Scan,
					Reason: "row locking read"},
			}},
		},
		{
			name: "for share of names its relation",
			sql:  "SELECT * FROM a, b FOR SHARE OF a",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{
				{Relation: rel("a"), Mode: lockmodel.AccessShare, Duration: lockmodel.Scan,
					Reason: "read"},
				{Relation: rel("b"), Mode: lockmodel.AccessShare, Duration: lockmodel.Scan,
					Reason: "read"},
				{Relation: rel("a"), Mode: lockmodel.RowShare, Duration: lockmodel.Scan,
					Reason: "row locking read"},
			}},
		},
		{
			name: "insert",
			sql:  "INSERT INTO users VALUES (1)",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{{
				Relation: rel("users"), Mode: lockmodel.RowExclusive, Duration: lockmodel.Instant,
				Reason: "write",
			}}},
		},
		{
			name: "insert default values reads nothing",
			sql:  "INSERT INTO users DEFAULT VALUES",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{{
				Relation: rel("users"), Mode: lockmodel.RowExclusive, Duration: lockmodel.Instant,
				Reason: "write",
			}}},
		},
		{
			name: "insert from a select reads the source",
			sql:  "INSERT INTO users SELECT * FROM staging",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{
				{Relation: rel("users"), Mode: lockmodel.RowExclusive, Duration: lockmodel.Instant,
					Reason: "write"},
				{Relation: rel("staging"), Mode: lockmodel.AccessShare, Duration: lockmodel.Scan,
					Reason: "read"},
			}},
		},
		{
			name: "update",
			sql:  "UPDATE users SET name = 'x' WHERE id = 1",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{{
				Relation: rel("users"), Mode: lockmodel.RowExclusive, Duration: lockmodel.Scan,
				Reason: "write, scanning for matching rows",
			}}},
		},
		{
			name: "update from reads the joined table",
			sql:  "UPDATE users SET name = r.name FROM refs r WHERE r.id = users.id",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{
				{Relation: rel("users"), Mode: lockmodel.RowExclusive, Duration: lockmodel.Scan,
					Reason: "write, scanning for matching rows"},
				{Relation: rel("refs"), Mode: lockmodel.AccessShare, Duration: lockmodel.Scan,
					Reason: "read"},
			}},
		},
		{
			name: "delete",
			sql:  "DELETE FROM users WHERE id = 1",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{{
				Relation: rel("users"), Mode: lockmodel.RowExclusive, Duration: lockmodel.Scan,
				Reason: "write, scanning for matching rows",
			}}},
		},
		{
			name: "delete using reads the joined table",
			sql:  "DELETE FROM users USING refs WHERE refs.id = users.id",
			want: lockmodel.Analysis{Effects: []lockmodel.LockEffect{
				{Relation: rel("users"), Mode: lockmodel.RowExclusive, Duration: lockmodel.Scan,
					Reason: "write, scanning for matching rows"},
				{Relation: rel("refs"), Mode: lockmodel.AccessShare, Duration: lockmodel.Scan,
					Reason: "read"},
			}},
		},
		{
			name: "a statement without table locks",
			sql:  "CREATE FUNCTION f() RETURNS int LANGUAGE sql AS 'SELECT 1'",
			want: lockmodel.Analysis{},
		},
	})
}
