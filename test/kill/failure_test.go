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

package kill_test

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/tochemey/mig/internal/ledger"
	"github.com/tochemey/mig/test/harness"
)

// brokenFixture is a step the server rejects: the column does not exist. It
// parses, so it reaches the database rather than being caught at plan time.
var brokenFixture = fixture{
	file: "20240817120000_broken_index.sql",
	body: `-- +mig step: index_absent_column
-- +mig notx
CREATE INDEX CONCURRENTLY idx_users_absent ON users (no_such_column);
`,
}

// TestAFailedStepIsRecordedAndReleasesTheLease covers the ordinary failure
// path: a step the database refuses.
//
// The run must exit non-zero, record why for a human to read, and let go of the
// lease. A failure that kept the lease would lock every later run out until the
// TTL lapsed, and one that recorded nothing would leave an operator with an
// exit code and no explanation.
func TestAFailedStepIsRecordedAndReleasesTheLease(t *testing.T) {
	t.Parallel()

	env := newRun(t, brokenFixture)

	proc := env.start("")

	code, err := proc.Wait(time.Minute)
	if err != nil {
		t.Fatalf("wait for mig: %v", err)
	}

	if code == 0 {
		t.Fatalf("mig reported success for a step the server rejected\nstdout: %s", proc.Stdout())
	}

	env.drain()

	recorded := env.recordedStep(0)

	if recorded.Status != ledger.StatusFailed {
		t.Fatalf("step is %q, want %q", recorded.Status, ledger.StatusFailed)
	}

	if recorded.Error == "" {
		t.Fatal("the failure was recorded without saying what went wrong")
	}

	if !strings.Contains(recorded.Error, "no_such_column") {
		t.Fatalf("the recorded error does not name the cause: %q", recorded.Error)
	}

	// The attempt is counted, which is what tells the next run the database may
	// have been left part-way through.
	if recorded.Attempts != 1 {
		t.Fatalf("step records %d attempts, want 1", recorded.Attempts)
	}

	// A failed run still hands the lease back, so the next one need not wait it
	// out. Anything else here would show up as a held lease.
	problems := env.leaseProblems()
	if len(problems) > 0 {
		t.Fatalf("after a failed run: %s", strings.Join(problems, "; "))
	}
}

// leaseProblems reports whether the run left a lease behind.
func (r *run) leaseProblems() []string {
	r.t.Helper()

	var found []string

	r.withDB(func(db *sql.DB) {
		problems, err := harness.Problems(r.t.Context(), db)
		if err != nil {
			r.t.Fatalf("inspect database: %v", err)
		}

		for _, problem := range problems {
			if strings.HasPrefix(problem, "lease still held") {
				found = append(found, problem)
			}
		}
	})

	return found
}
