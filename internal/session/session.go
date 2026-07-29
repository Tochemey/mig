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

// Package session prepares the pinned connections that steps run on.
//
// Every step runs on a pinned *sql.Conn rather than the pool. Session state —
// SET, advisory locks, search_path — is meaningless on a pooled handle, and
// getting that wrong produces failures that appear only under concurrency.
package session

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	// DefaultLockTimeout bounds how long a statement waits for a lock.
	//
	// Postgres queues lock requests, so a migration blocked behind a long
	// SELECT blocks every later query on that table behind itself. Waiting for
	// the lock, not the migration, is what takes the outage.
	DefaultLockTimeout = 3 * time.Second

	// DefaultStatementTimeout bounds a transactional step.
	DefaultStatementTimeout = 30 * time.Second

	// IdleInTransactionTimeout stops a wedged step from holding locks forever.
	IdleInTransactionTimeout = 30 * time.Second
)

// ErrTransactionPooling reports a pooler that discards session state.
var ErrTransactionPooling = errors.New(
	"connection is behind a transaction-pooling pooler (PgBouncer); " +
		"session settings and advisory locks do not survive, so migrations cannot run safely. " +
		"Use a direct connection to Postgres, or a session-pooling port")

// Config describes the session a step needs.
type Config struct {
	// Application is the value for application_name, which is how an operator
	// finds this runner in pg_stat_activity.
	Application string

	// LockTimeout bounds lock waits. Zero disables the timeout, which the
	// no_lock_timeout annotation asks for explicitly.
	LockTimeout time.Duration

	// StatementTimeout bounds the statement itself. Zero means no limit, which
	// is what a non-transactional index build needs.
	StatementTimeout time.Duration
}

// Prepare applies cfg to a pinned connection.
func Prepare(ctx context.Context, conn *sql.Conn, cfg Config) error {
	settings := []struct {
		name  string
		value string
	}{
		{"application_name", cfg.Application},
		{"lock_timeout", milliseconds(cfg.LockTimeout)},
		{"statement_timeout", milliseconds(cfg.StatementTimeout)},
		{"idle_in_transaction_session_timeout", milliseconds(IdleInTransactionTimeout)},
	}

	for _, setting := range settings {
		// SET takes a literal, not a bind parameter; set_config is the
		// parameterisable form.
		const query = "SELECT set_config($1, $2, false)"

		if _, err := conn.ExecContext(ctx, query, setting.name, setting.value); err != nil {
			return fmt.Errorf("set %s: %w", setting.name, err)
		}
	}

	return nil
}

// milliseconds renders a duration as Postgres expects it. Zero means no limit.
func milliseconds(d time.Duration) string {
	if d <= 0 {
		return "0"
	}

	return fmt.Sprintf("%d", d.Milliseconds())
}

// DetectPooling reports whether the connection is behind a transaction-pooling
// pooler, which silently discards everything [Prepare] sets.
//
// The probe is two round trips on the same pinned connection: set a value, then
// read it back. Under transaction pooling the second lands on a different
// server connection and the value is gone.
func DetectPooling(ctx context.Context, conn *sql.Conn) error {
	probe := "mig-probe-" + rand.Text()

	if _, err := conn.ExecContext(ctx, "SELECT set_config('application_name', $1, false)", probe); err != nil {
		return fmt.Errorf("probe for connection pooling: %w", err)
	}

	var observed string

	if err := conn.QueryRowContext(ctx, "SELECT current_setting('application_name')").Scan(&observed); err != nil {
		return fmt.Errorf("probe for connection pooling: %w", err)
	}

	if observed != probe {
		return ErrTransactionPooling
	}

	return nil
}
