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

package session_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tochemey/mig/internal/session"
	"github.com/tochemey/mig/test/harness"
)

// shared is the container for this package, or nil when docker is absent.
var shared *harness.Harness

// TestMain brings up one container for the package.
func TestMain(m *testing.M) {
	os.Exit(harness.Main(m, 0, func(_ context.Context, h *harness.Harness) error {
		shared = h
		return nil
	}))
}

// TestPrepareAppliesSettings covers the session a step runs under. Applying
// these to a pooled handle would appear to work and take effect nowhere, so
// they are read back from the same pinned connection.
func TestPrepareAppliesSettings(t *testing.T) {
	conn := newConn(t)

	cfg := session.Config{
		Application:      "mig/test add_email/index",
		LockTimeout:      3 * time.Second,
		StatementTimeout: 30 * time.Second,
	}

	if err := session.Prepare(t.Context(), conn, cfg); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	want := map[string]string{
		"application_name":                    cfg.Application,
		"lock_timeout":                        "3s",
		"statement_timeout":                   "30s",
		"idle_in_transaction_session_timeout": "30s",
	}

	for setting, expected := range want {
		if got := currentSetting(t, conn, setting); got != expected {
			t.Fatalf("%s is %q, want %q", setting, got, expected)
		}
	}
}

// TestPrepareDisablesTimeouts covers the non-transactional case. A concurrent
// index build runs for as long as it runs, and a statement timeout would kill
// it part-way and leave an invalid index behind every time.
func TestPrepareDisablesTimeouts(t *testing.T) {
	conn := newConn(t)

	cfg := session.Config{Application: "mig/test", LockTimeout: 0, StatementTimeout: 0}

	if err := session.Prepare(t.Context(), conn, cfg); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	for _, setting := range []string{"lock_timeout", "statement_timeout"} {
		if got := currentSetting(t, conn, setting); got != "0" {
			t.Fatalf("%s is %q, want 0", setting, got)
		}
	}
}

// TestPrepareReportsFailure covers a connection that went away.
func TestPrepareReportsFailure(t *testing.T) {
	conn := newConn(t)

	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if err := session.Prepare(t.Context(), conn, session.Config{}); err == nil {
		t.Fatal("prepare on a closed connection returned no error")
	}
}

// TestDetectPoolingAcceptsADirectConnection covers the case that must be
// allowed: a session where settings survive between statements.
func TestDetectPoolingAcceptsADirectConnection(t *testing.T) {
	if err := session.DetectPooling(t.Context(), newConn(t)); err != nil {
		t.Fatalf("a direct connection was rejected: %v", err)
	}
}

// TestDetectPoolingReportsFailure covers the probe itself failing, which must
// not be mistaken for a pooler.
func TestDetectPoolingReportsFailure(t *testing.T) {
	conn := newConn(t)

	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	err := session.DetectPooling(t.Context(), conn)

	if err == nil || errors.Is(err, session.ErrTransactionPooling) {
		t.Fatalf("probe on a closed connection returned %v, want a query error", err)
	}
}

// TestWithLockRetryPassesThrough covers work that succeeds first time.
func TestWithLockRetryPassesThrough(t *testing.T) {
	calls := 0

	err := session.WithLockRetry(t.Context(), session.DefaultRetry(), func(context.Context) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}

	if calls != 1 {
		t.Fatalf("work ran %d times, want 1", calls)
	}
}

// TestWithLockRetryRetriesLockTimeouts covers the case the retry exists for.
//
// Postgres queues lock requests, so a migration that gives up the moment it
// cannot take a lock leaves every later query on that table queued behind it.
func TestWithLockRetryRetriesLockTimeouts(t *testing.T) {
	calls := 0

	retry := session.Retry{Attempts: 5, Base: time.Millisecond, Jitter: 0.2}

	err := session.WithLockRetry(t.Context(), retry, func(context.Context) error {
		calls++

		if calls < 3 {
			return &pgconn.PgError{Code: "55P03", Message: "canceling statement due to lock timeout"}
		}

		return nil
	})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}

	if calls != 3 {
		t.Fatalf("work ran %d times, want 3", calls)
	}
}

// TestWithLockRetryGivesUp covers a lock that never frees up.
func TestWithLockRetryGivesUp(t *testing.T) {
	calls := 0

	retry := session.Retry{Attempts: 3, Base: time.Millisecond, Jitter: 0}

	err := session.WithLockRetry(t.Context(), retry, func(context.Context) error {
		calls++
		return &pgconn.PgError{Code: "55P03"}
	})

	if err == nil {
		t.Fatal("retry gave up without reporting why")
	}

	if calls != retry.Attempts {
		t.Fatalf("work ran %d times, want %d", calls, retry.Attempts)
	}
}

// TestWithLockRetryDoesNotRetryOtherErrors keeps a statement that failed on its
// own terms from failing repeatedly and more slowly.
func TestWithLockRetryDoesNotRetryOtherErrors(t *testing.T) {
	calls := 0
	boom := errors.New("boom")

	err := session.WithLockRetry(t.Context(), session.DefaultRetry(), func(context.Context) error {
		calls++
		return boom
	})

	if !errors.Is(err, boom) {
		t.Fatalf("retry returned %v, want boom", err)
	}

	if calls != 1 {
		t.Fatalf("work ran %d times, want 1", calls)
	}
}

// TestWithLockRetryHonoursCancellation covers a run abandoned while waiting.
func TestWithLockRetryHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())

	retry := session.Retry{Attempts: 100, Base: 50 * time.Millisecond, Jitter: 0}

	time.AfterFunc(100*time.Millisecond, cancel)

	err := session.WithLockRetry(ctx, retry, func(context.Context) error {
		return &pgconn.PgError{Code: "55P03"}
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("retry returned %v, want context.Canceled", err)
	}
}

// TestLockTimeoutIsEnforced covers the setting doing its job against a real
// lock, rather than merely being accepted by the server.
func TestLockTimeoutIsEnforced(t *testing.T) {
	db := newDatabase(t)
	ctx := t.Context()

	// A blocker holds an exclusive lock on the table.
	blocker, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("pin blocker: %v", err)
	}

	defer func() {
		_ = blocker.Close()
	}()

	tx, err := blocker.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	defer func() {
		_ = tx.Rollback()
	}()

	if _, err := tx.ExecContext(ctx, "LOCK TABLE users IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatalf("lock: %v", err)
	}

	victim, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("pin victim: %v", err)
	}

	defer func() {
		_ = victim.Close()
	}()

	cfg := session.Config{Application: "mig/test", LockTimeout: 200 * time.Millisecond}

	if err := session.Prepare(ctx, victim, cfg); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	_, err = victim.ExecContext(ctx, "ALTER TABLE users ADD COLUMN email text")

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("blocked statement returned %v, want a lock timeout", err)
	}
}

// newDatabase gives the test its own database holding the fixture.
func newDatabase(t *testing.T) *sql.DB {
	t.Helper()

	if shared == nil {
		t.Skip("postgres container not available")
	}

	name, err := shared.Clone(t.Context(), harness.Template)
	if err != nil {
		t.Fatalf("clone template: %v", err)
	}

	db, err := shared.Open(t.Context(), name)
	if err != nil {
		t.Fatalf("open clone: %v", err)
	}

	t.Cleanup(func() {
		_ = db.Close()

		if err := shared.DropDatabase(context.Background(), name); err != nil {
			t.Errorf("drop clone %q: %v", name, err)
		}
	})

	return db
}

// newConn pins a connection from a fresh database.
func newConn(t *testing.T) *sql.Conn {
	t.Helper()

	conn, err := newDatabase(t).Conn(t.Context())
	if err != nil {
		t.Fatalf("pin connection: %v", err)
	}

	t.Cleanup(func() {
		_ = conn.Close()
	})

	return conn
}

// currentSetting reads a GUC back from the same pinned connection.
func currentSetting(t *testing.T, conn *sql.Conn, name string) string {
	t.Helper()

	var value string

	if err := conn.QueryRowContext(t.Context(), "SELECT current_setting($1)", name).Scan(&value); err != nil {
		t.Fatalf("read %s: %v", name, err)
	}

	return value
}
