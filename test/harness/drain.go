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
	"strings"
	"time"
)

// drainPollInterval is how often [Harness.WaitBackendsGone] re-checks.
const drainPollInterval = 25 * time.Millisecond

// backendsQuery lists the server processes attached to a database. Excluding
// pg_backend_pid() covers a maintenance pool pointed at that same database.
const backendsQuery = `
SELECT pid,
       coalesce(state, ''),
       coalesce(query, ''),
       coalesce(application_name, '')
  FROM pg_stat_activity
 WHERE datname = $1
   AND pid <> pg_backend_pid()`

// Backend is a server process attached to a database.
type Backend struct {
	PID         int
	State       string
	Query       string
	Application string
}

// String renders a backend for failure messages, collapsing the query onto one
// line.
func (b Backend) String() string {
	query := strings.Join(strings.Fields(b.Query), " ")

	return fmt.Sprintf("pid=%d state=%q application=%q query=%q", b.PID, b.State, b.Application, query)
}

// Backends lists the server processes currently attached to a database.
func (h *Harness) Backends(ctx context.Context, database string) (_ []Backend, err error) {
	rows, err := h.admin.QueryContext(ctx, backendsQuery, database)
	if err != nil {
		return nil, fmt.Errorf("query backends of %q: %w", database, err)
	}

	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close backends of %q: %w", database, closeErr))
		}
	}()

	var backends []Backend

	for rows.Next() {
		var b Backend

		if err := rows.Scan(&b.PID, &b.State, &b.Query, &b.Application); err != nil {
			return nil, fmt.Errorf("scan backend of %q: %w", database, err)
		}

		backends = append(backends, b)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate backends of %q: %w", database, err)
	}

	return backends, nil
}

// WaitBackendsGone blocks until no server process is attached to database.
//
// Every restart of a killed migrator must go through here. Killing the client
// does not stop its backend, which keeps working until it notices the socket is
// gone, so a test that restarts immediately races a backend that still holds
// locks and may finish the work.
//
// Exceeding the timeout is a failure, and the error names the survivors.
func (h *Harness) WaitBackendsGone(ctx context.Context, database string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	ticker := time.NewTicker(drainPollInterval)
	defer ticker.Stop()

	for {
		backends, err := h.Backends(ctx, database)
		if err != nil {
			return err
		}

		if len(backends) == 0 {
			return nil
		}

		if !time.Now().Before(deadline) {
			return fmt.Errorf("%d backend(s) still attached to %q after %s: %s",
				len(backends), database, timeout, formatBackends(backends))
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for backends of %q to exit: %w", database, ctx.Err())
		case <-ticker.C:
		}
	}
}

// formatBackends renders backends as a single semicolon-separated line.
func formatBackends(backends []Backend) string {
	parts := make([]string, 0, len(backends))

	for _, b := range backends {
		parts = append(parts, b.String())
	}

	return strings.Join(parts, "; ")
}
