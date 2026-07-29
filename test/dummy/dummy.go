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

// Package dummy stands in for the migrator while the harness itself is under
// test. It pins a connection and leaves the server-side backend busy, so a
// SIGKILL on the client leaves a live backend behind.
package dummy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver
)

const (
	// ReadyMarker is printed once the backend exists and is about to become
	// busy.
	ReadyMarker = "dummy: ready"

	// AppName identifies the dummy's backend in pg_stat_activity.
	AppName = "mig-dummy"
)

// Run pins one connection, announces readiness on w, then blocks the backend in
// a server-side sleep of hold until the process is killed.
func Run(ctx context.Context, dsn string, hold time.Duration, ready func(string)) (err error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}

	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close pool: %w", closeErr))
		}
	}()

	// A pinned connection, as the executor uses: session state on a pooled
	// handle is meaningless.
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}

	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("return connection to pool: %w", closeErr))
		}
	}()

	// SET takes a literal, not a bind parameter; set_config is the
	// parameterisable form.
	if _, err := conn.ExecContext(ctx, "SELECT set_config('application_name', $1, false)", AppName); err != nil {
		return fmt.Errorf("set application_name: %w", err)
	}

	// Announced only once the backend exists, so the harness cannot race a
	// connection that is not established yet.
	ready(ReadyMarker)

	if _, err := conn.ExecContext(ctx, "SELECT pg_sleep($1)", hold.Seconds()); err != nil {
		return fmt.Errorf("hold backend: %w", err)
	}

	return nil
}
