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

package importer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrNoHistory reports a database the named tool has never run against.
var ErrNoHistory = errors.New("no migration history found")

// gooseTableQuery locates goose's history table in any schema on the path.
const gooseTableQuery = `SELECT to_regclass('goose_db_version')::text`

// gooseQuery reads the current verdict for each version.
//
// Goose appends a row per apply and per rollback rather than deleting, so only
// the newest row for a version says anything about it. Taking the maximum id
// per version and keeping the ones still applied is what makes a rolled-back
// migration read as outstanding, which is what it is.
const gooseQuery = `
SELECT version_id
  FROM (
    SELECT DISTINCT ON (version_id) version_id, is_applied
      FROM %s
     ORDER BY version_id, id DESC
  ) current
 WHERE is_applied
   AND version_id > 0
 ORDER BY version_id`

// readGoose loads what goose recorded.
//
// Version 0 is goose's own bootstrap row and never corresponds to a file, so
// it is excluded in the query rather than reported as an unknown version.
func readGoose(ctx context.Context, db *sql.DB) (History, error) {
	table, err := locate(ctx, db, gooseTableQuery)
	if err != nil {
		return History{}, err
	}

	applied, err := readVersions(ctx, db, fmt.Sprintf(gooseQuery, table))
	if err != nil {
		return History{}, err
	}

	return History{Applied: applied}, nil
}

// locate resolves a history table, and reports its absence as such.
func locate(ctx context.Context, db *sql.DB, query string) (string, error) {
	var name sql.NullString

	if err := db.QueryRowContext(ctx, query).Scan(&name); err != nil {
		return "", fmt.Errorf("look for the history table: %w", err)
	}

	if !name.Valid {
		return "", ErrNoHistory
	}

	return name.String, nil
}

// readVersions runs a history query and collects the versions it returns.
func readVersions(ctx context.Context, db *sql.DB, query string) (_ []int64, err error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("read migration history: %w", err)
	}

	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close history rows: %w", closeErr))
		}
	}()

	var versions []int64

	for rows.Next() {
		var version int64

		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("scan migration history: %w", err)
		}

		versions = append(versions, version)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate migration history: %w", err)
	}

	return versions, nil
}
