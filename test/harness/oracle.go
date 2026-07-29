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

package harness

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/tochemey/mig/internal/catalog"
)

// State is everything the oracle compares between two databases.
type State struct {
	// Fingerprint is the hash of the schema.
	Fingerprint string

	// Data is a checksum over the fixture's rows.
	Data string

	// Schema is the readable form of Fingerprint, so a mismatch can be diffed.
	Schema string
}

// dataQuery checksums the fixture's contents.
//
// Ordering is explicit because a sequential scan returns rows in whatever order
// the heap holds them, and that order changes when rows are updated.
const dataQuery = `
SELECT coalesce(md5(string_agg(row_to_json(t)::text, '|' ORDER BY t.id)), 'empty')
  FROM users t`

// Capture reads the schema and data state of a database.
func Capture(ctx context.Context, db *sql.DB) (State, error) {
	fingerprint, err := catalog.Fingerprint(ctx, db)
	if err != nil {
		return State{}, err
	}

	schema, err := catalog.Describe(ctx, db)
	if err != nil {
		return State{}, err
	}

	var data string

	if err := db.QueryRowContext(ctx, dataQuery).Scan(&data); err != nil {
		return State{}, fmt.Errorf("checksum fixture data: %w", err)
	}

	return State{Fingerprint: fingerprint, Data: data, Schema: schema}, nil
}

// Problems lists everything wrong with a database that should have converged.
//
// It returns findings rather than failing, so a caller sees all of them at once
// instead of the first.
func Problems(ctx context.Context, db *sql.DB) ([]string, error) {
	var found []string

	checks := []struct {
		name  string
		query string
	}{
		{
			// The state an interrupted concurrent build leaves behind.
			name: "invalid or unready index",
			query: `
SELECT format('%s.%s valid=%s ready=%s', n.nspname, c.relname, i.indisvalid, i.indisready)
  FROM pg_index i
  JOIN pg_class c ON c.oid = i.indexrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE NOT (i.indisvalid AND i.indisready)
   AND n.nspname NOT IN ('pg_catalog', 'information_schema')
 ORDER BY 1`,
		},
		{
			name: "unvalidated constraint",
			query: `
SELECT format('%s.%s', rel.relname, con.conname)
  FROM pg_constraint con
  JOIN pg_class rel ON rel.oid = con.conrelid
  JOIN pg_namespace n ON n.oid = rel.relnamespace
 WHERE NOT con.convalidated
   AND n.nspname NOT IN ('pg_catalog', 'information_schema')
 ORDER BY 1`,
		},
		{
			name: "step not succeeded",
			query: `
SELECT format('%s[%s] %s status=%s attempts=%s error=%s',
              migration_id, idx, name, status, attempts, coalesce(error, '-'))
  FROM mig.steps
 WHERE status <> 'succeeded'
 ORDER BY 1`,
		},
		{
			name: "migration not succeeded",
			query: `
SELECT format('%s status=%s', id, status)
  FROM mig.migrations
 WHERE status <> 'succeeded'
 ORDER BY 1`,
		},
		{
			// A lease still held means a runner exited without releasing it, or
			// is still running.
			name: "lease still held",
			query: `
SELECT format('owner=%s fence=%s expires_at=%s', owner, fence, expires_at)
  FROM mig.lease
 WHERE owner IS NOT NULL AND expires_at > now()`,
		},
		{
			name: "session idle in transaction",
			query: `
SELECT format('pid=%s state=%s query=%s', pid, state, query)
  FROM pg_stat_activity
 WHERE datname = current_database()
   AND pid <> pg_backend_pid()
   AND state LIKE 'idle in transaction%'`,
		},
	}

	for _, check := range checks {
		rows, err := collect(ctx, db, check.query)
		if err != nil {
			return nil, fmt.Errorf("check for %s: %w", check.name, err)
		}

		for _, row := range rows {
			found = append(found, check.name+": "+row)
		}
	}

	return found, nil
}

// collect runs a single-column query and returns its rows.
func collect(ctx context.Context, db *sql.DB, query string) (_ []string, err error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}

	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close rows: %w", closeErr))
		}
	}()

	var out []string

	for rows.Next() {
		var line string

		if err := rows.Scan(&line); err != nil {
			return nil, err
		}

		out = append(out, line)
	}

	return out, rows.Err()
}

// DiffSchema renders the difference between two schema descriptions, so a
// fingerprint mismatch reports what differs rather than that something does.
func DiffSchema(want, got string) string {
	wantLines := strings.Split(want, "\n")
	gotLines := strings.Split(got, "\n")

	missing := difference(wantLines, gotLines)
	extra := difference(gotLines, wantLines)

	var out strings.Builder

	for _, line := range missing {
		fmt.Fprintf(&out, "- %s\n", line)
	}

	for _, line := range extra {
		fmt.Fprintf(&out, "+ %s\n", line)
	}

	return out.String()
}

// difference returns the lines of a that are absent from b.
func difference(a, b []string) []string {
	present := make(map[string]struct{}, len(b))

	for _, line := range b {
		present[line] = struct{}{}
	}

	var out []string

	for _, line := range a {
		if line == "" {
			continue
		}

		if _, ok := present[line]; !ok {
			out = append(out, line)
		}
	}

	return out
}
