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

// migrateTableQuery locates golang-migrate's history table.
const migrateTableQuery = `SELECT to_regclass('schema_migrations')::text`

// migrateQuery reads the single row golang-migrate keeps.
const migrateQuery = `SELECT version, dirty FROM %s LIMIT 1`

// readGolangMigrate loads what golang-migrate recorded.
//
// It keeps one row holding where the history got to, not which migrations ran,
// so the result is a high-water mark: everything at or below it is applied.
//
// The dirty flag means that version was being applied when the run died. It is
// reported and excluded from the mark rather than refused: leaving it
// outstanding is what hands it to the catalog, which is the only thing that
// knows how much of it landed. Refusing to import until a human clears the flag
// by hand is the failure this tool exists to remove.
func readGolangMigrate(ctx context.Context, db *sql.DB) (History, error) {
	table, err := locate(ctx, db, migrateTableQuery)
	if err != nil {
		return History{}, err
	}

	var (
		version int64
		dirty   bool
	)

	err = db.QueryRowContext(ctx, fmt.Sprintf(migrateQuery, table)).Scan(&version, &dirty)

	// An empty table is a tool that was set up and never ran, which is a history
	// covering nothing rather than a missing history.
	if errors.Is(err, sql.ErrNoRows) {
		return History{Ceiling: true, Through: -1}, nil
	}

	if err != nil {
		return History{}, fmt.Errorf("read migration history: %w", err)
	}

	history := History{Ceiling: true, Through: version}

	if dirty {
		history.Through = version - 1
		history.Dirty = true
		history.DirtyVersion = version
	}

	return history, nil
}
