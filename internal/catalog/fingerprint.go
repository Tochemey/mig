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

package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

// fingerprintQueries read the schema into a canonical, ordered form.
//
// The catalog is queried directly rather than diffed from pg_dump output,
// whose ordering and formatting vary between versions and would produce
// differences that are not differences.
//
// Each query returns one text column per row, already sorted by the server.
//
// Schemas beginning pg_ are excluded along with information_schema and the
// ledger's own. TOAST index names embed the OID of the table they serve, so two
// databases holding an identical schema disagree about them.
var fingerprintQueries = []struct {
	section string
	query   string
}{
	{
		section: "columns",
		query: `
SELECT format('%s.%s.%s %s null=%s default=%s',
              c.table_schema, c.table_name, c.column_name,
              c.data_type, c.is_nullable, coalesce(c.column_default, '-'))
  FROM information_schema.columns c
 WHERE c.table_schema NOT IN ('pg_catalog', 'information_schema')
   AND c.table_schema <> 'mig'
 ORDER BY 1`,
	},
	{
		// indisvalid and indisready are the point: an interrupted concurrent
		// build leaves an index that exists but is neither.
		section: "indexes",
		query: `
SELECT format('%s.%s valid=%s ready=%s live=%s %s',
              n.nspname, c.relname, i.indisvalid, i.indisready, i.indislive,
              pg_get_indexdef(i.indexrelid))
  FROM pg_index i
  JOIN pg_class c ON c.oid = i.indexrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname NOT LIKE 'pg\_%'
   AND n.nspname NOT IN ('information_schema', 'mig')
 ORDER BY 1`,
	},
	{
		section: "constraints",
		query: `
SELECT format('%s.%s.%s type=%s validated=%s %s',
              n.nspname, rel.relname, con.conname, con.contype, con.convalidated,
              pg_get_constraintdef(con.oid))
  FROM pg_constraint con
  JOIN pg_class rel ON rel.oid = con.conrelid
  JOIN pg_namespace n ON n.oid = rel.relnamespace
 WHERE n.nspname NOT LIKE 'pg\_%'
   AND n.nspname NOT IN ('information_schema', 'mig')
 ORDER BY 1`,
	},
	{
		section: "sequences",
		query: `
SELECT format('%s.%s start=%s increment=%s',
              s.sequence_schema, s.sequence_name, s.start_value, s.increment)
  FROM information_schema.sequences s
 WHERE s.sequence_schema NOT IN ('pg_catalog', 'information_schema')
   AND s.sequence_schema <> 'mig'
 ORDER BY 1`,
	},
	{
		section: "ownership",
		query: `
SELECT format('%s.%s owner=%s', n.nspname, c.relname, pg_get_userbyid(c.relowner))
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname NOT LIKE 'pg\_%'
   AND n.nspname NOT IN ('information_schema', 'mig')
   AND c.relkind IN ('r', 'p', 'S', 'v', 'm')
 ORDER BY 1`,
	},
	{
		section: "grants",
		query: `
SELECT format('%s.%s %s -> %s: %s',
              g.table_schema, g.table_name, g.grantor, g.grantee, g.privilege_type)
  FROM information_schema.role_table_grants g
 WHERE g.table_schema NOT IN ('pg_catalog', 'information_schema')
   AND g.table_schema <> 'mig'
 ORDER BY 1`,
	},
}

// Fingerprint hashes the schema into a value that two databases share exactly
// when their schemas match.
//
// The ledger's own schema is excluded. It carries attempt counts and timestamps
// that differ between an interrupted run and an uninterrupted one, and
// comparing them would report a difference where the schemas agree.
func Fingerprint(ctx context.Context, q Querier) (string, error) {
	sum := sha256.New()

	for _, section := range fingerprintQueries {
		if _, err := fmt.Fprintf(sum, "[%s]\n", section.section); err != nil {
			return "", fmt.Errorf("hash section %q: %w", section.section, err)
		}

		if err := hashRows(ctx, q, sum, section.query); err != nil {
			return "", fmt.Errorf("fingerprint %s: %w", section.section, err)
		}
	}

	return hex.EncodeToString(sum.Sum(nil)), nil
}

// Describe returns the same content as [Fingerprint] in readable form, so a
// mismatch can be diffed instead of merely reported.
func Describe(ctx context.Context, q Querier) (string, error) {
	var out strings.Builder

	for _, section := range fingerprintQueries {
		if _, err := fmt.Fprintf(&out, "[%s]\n", section.section); err != nil {
			return "", fmt.Errorf("describe section %q: %w", section.section, err)
		}

		if err := hashRows(ctx, q, &out, section.query); err != nil {
			return "", fmt.Errorf("describe %s: %w", section.section, err)
		}
	}

	return out.String(), nil
}

// hashRows writes every row of query to sink, one per line.
func hashRows(ctx context.Context, q Querier, sink io.Writer, query string) (err error) {
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return err
	}

	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close rows: %w", closeErr))
		}
	}()

	for rows.Next() {
		var line string

		if err := rows.Scan(&line); err != nil {
			return err
		}

		if _, err := fmt.Fprintln(sink, line); err != nil {
			return err
		}
	}

	return rows.Err()
}
