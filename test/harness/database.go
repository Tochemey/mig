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
	"errors"
	"fmt"
	"time"
)

// seedSchema is the fixture the tested migrations operate on. It omits the
// columns, indexes and constraints those migrations create.
const seedSchema = `
CREATE TABLE users (
    id           bigint PRIMARY KEY,
    name         text   NOT NULL,
    legacy_email text   NOT NULL
)`

// seedRows populates the fixture in a single server-side statement, since
// sending rows over the wire is what makes seeding slow.
const seedRows = `
INSERT INTO users (id, name, legacy_email)
SELECT g, 'user_' || g, 'user_' || g || '@example.test'
  FROM generate_series(1, $1) AS g`

// templateDrainTimeout bounds the wait for the seeding connections to clear.
// CREATE DATABASE ... TEMPLATE fails while any session is attached to the
// template.
const templateDrainTimeout = 30 * time.Second

// SeedTemplate creates database name, populates it with rows fixture rows, and
// leaves it free of connections so it can serve as a clone template.
//
// It is idempotent: an existing database of that name is dropped first.
func (h *Harness) SeedTemplate(ctx context.Context, name string, rows int) error {
	if err := h.DropDatabase(ctx, name); err != nil {
		return err
	}

	//nolint:gosec // G201: identifiers cannot be bound as parameters; quoteIdent is the guard.
	if _, err := h.admin.ExecContext(ctx, "CREATE DATABASE "+quoteIdent(name)); err != nil {
		return fmt.Errorf("create template %q: %w", name, err)
	}

	if err := h.seed(ctx, name, rows); err != nil {
		return err
	}

	// The pool above is closed but its backends linger briefly, and racing them
	// fails cloning with "source database is being accessed by other users".
	if err := h.WaitBackendsGone(ctx, name, templateDrainTimeout); err != nil {
		return fmt.Errorf("drain template %q: %w", name, err)
	}

	return nil
}

// seed fills a freshly created database with the fixture and closes its pool.
//
// The close is reported rather than discarded, because a pool that fails to
// close keeps backends attached and the caller is about to wait for this
// database to become connection-free.
func (h *Harness) seed(ctx context.Context, name string, rows int) (err error) {
	db, err := h.Open(ctx, name)
	if err != nil {
		return err
	}

	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close seeding pool for %q: %w", name, closeErr))
		}
	}()

	if _, err := db.ExecContext(ctx, seedSchema); err != nil {
		return fmt.Errorf("create fixture schema in %q: %w", name, err)
	}

	if _, err := db.ExecContext(ctx, seedRows, rows); err != nil {
		return fmt.Errorf("seed %d rows into %q: %w", rows, name, err)
	}

	// Without statistics the planner picks sequential scans for keyset lookups,
	// which distorts the batch timings a throttle reacts to.
	if _, err := db.ExecContext(ctx, "ANALYZE users"); err != nil {
		return fmt.Errorf("analyze %q: %w", name, err)
	}

	return nil
}

// Clone creates a fresh database from template and returns its name. The copy
// happens inside the server, so it costs a file copy rather than a re-seed.
func (h *Harness) Clone(ctx context.Context, template string) (string, error) {
	name := fmt.Sprintf("mig_test_%d", h.seq.Add(1))

	//nolint:gosec // G201: identifiers cannot be bound as parameters; quoteIdent is the guard.
	stmt := "CREATE DATABASE " + quoteIdent(name) + " TEMPLATE " + quoteIdent(template)
	if _, err := h.admin.ExecContext(ctx, stmt); err != nil {
		return "", fmt.Errorf("clone %q from %q: %w", name, template, err)
	}

	return name, nil
}

// DropDatabase removes a database, disconnecting anything still attached to it.
// It is a no-op when the database does not exist.
func (h *Harness) DropDatabase(ctx context.Context, name string) error {
	//nolint:gosec // G201: identifiers cannot be bound as parameters; quoteIdent is the guard.
	stmt := "DROP DATABASE IF EXISTS " + quoteIdent(name) + " WITH (FORCE)"
	if _, err := h.admin.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("drop database %q: %w", name, err)
	}

	return nil
}
