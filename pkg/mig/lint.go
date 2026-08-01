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

package mig

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"

	"github.com/tochemey/mig/internal/lint"
	"github.com/tochemey/mig/internal/lint/policy"
	"github.com/tochemey/mig/internal/lint/rules"
	"github.com/tochemey/mig/internal/lint/stats"
	"github.com/tochemey/mig/internal/plan"
)

// Finding is one hazard the linter reports.
type Finding = rules.Finding

// Policy is what a team decided about the rule catalog: the severity each
// rule carries, the sizes a size-dependent hazard is graded by, and the
// target version. A nil *Policy is the absence of one.
type Policy = policy.Policy

// Suppression is one "-- +mig lint:ignore" directive a migration carries.
type Suppression = policy.Directive

// PolicyFileName is the policy file's conventional name.
const PolicyFileName = policy.FileName

// Severity levels a linted migration's findings carry.
const (
	SeverityInfo  = rules.SeverityInfo
	SeverityWarn  = rules.SeverityWarn
	SeverityError = rules.SeverityError
)

// DefaultTargetVersion is the Postgres major the linter assumes when the
// caller does not name one: the newest supported by the test matrix.
const DefaultTargetVersion = 18

// LintReport is what linting a migration directory produces.
type LintReport struct {
	// Findings is ordered by file and position.
	Findings []Finding

	// Sources maps each linted file to its contents, which is what a
	// renderer needs to show the offending line.
	Sources map[string]string

	// Suppressions is every lint directive the migrations carry, each marked
	// with whether it silenced anything on this run. It is what an audit of
	// them reads: a suppression that no longer suppresses is one nobody has
	// looked at since it was written.
	Suppressions []Suppression

	// Uncalibrated is what went wrong while measuring the server, empty when
	// nothing did. Measuring needs a write, which a read-only standby will
	// not give: the rest of connected mode works there and the findings
	// simply carry no estimates, which is worth saying rather than silently
	// dropping a number the reader was told to expect.
	Uncalibrated string
}

// Errors counts the findings that should fail a build.
func (r *LintReport) Errors() int {
	count := 0

	for _, finding := range r.Findings {
		if finding.Severity == SeverityError {
			count++
		}
	}

	return count
}

// LoadPolicy reads a policy file and resolves it for the migration directory
// being linted, which is what its per-directory overrides are keyed by.
func LoadPolicy(path, dir string) (*Policy, error) {
	return policy.Load(path, dir)
}

// Lint checks every migration in fsys against the offline rule catalog,
// without connecting to anything. The target version decides
// version-sensitive behaviour, such as whether an added default rewrites, and
// the policy may be nil.
//
// Offline, nothing here knows how large any table is, so every hazard whose
// cost is the table's size stays a warning. [LintConnected] is the same
// catalog with those sizes in hand.
func Lint(fsys fs.FS, targetVersion int, pol *Policy) (*LintReport, error) {
	loaded, err := plan.LoadFS(fsys)
	if err != nil {
		return nil, err
	}

	result, err := lint.Run(fsys, loaded, targetVersion, nil, pol)
	if err != nil {
		return nil, err
	}

	return reportOf(result, ""), nil
}

// LintConnected checks every migration with the live catalog in reach.
//
// The target version is the server's own, whatever the policy says, the
// severity of a size-dependent hazard is the size of the table it names, and
// each such finding carries an estimate of how long the work will hold its
// lock. Nothing is written: the catalog is read, and the calibration probe
// builds a table it rolls back.
func LintConnected(ctx context.Context, db *sql.DB, fsys fs.FS, pol *Policy) (*LintReport, error) {
	loaded, err := plan.LoadFS(fsys)
	if err != nil {
		return nil, err
	}

	major, err := stats.ServerMajor(ctx, db)
	if err != nil {
		return nil, err
	}

	relations, err := lint.Relations(loaded, major)
	if err != nil {
		return nil, err
	}

	snapshot, err := stats.Collect(ctx, db, relations)
	if err != nil {
		return nil, err
	}

	uncalibrated := ""
	if err := calibrate(ctx, db, snapshot); err != nil {
		uncalibrated = err.Error()
	}

	result, err := lint.Run(fsys, loaded, major, snapshot, pol)
	if err != nil {
		return nil, err
	}

	return reportOf(result, uncalibrated), nil
}

// reportOf is the public shape of what the engine produced.
func reportOf(result *lint.Result, uncalibrated string) *LintReport {
	return &LintReport{
		Findings:     result.Findings,
		Sources:      result.Sources,
		Suppressions: result.Suppressions,
		Uncalibrated: uncalibrated,
	}
}

// calibrate measures the server and hangs the result on the snapshot,
// returning why it could not rather than failing the run: a standby refuses
// the temporary table, and a report without estimates still carries every
// finding and every size.
func calibrate(ctx context.Context, db *sql.DB, snapshot *stats.Snapshot) (err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}

	defer func() {
		err = errors.Join(err, conn.Close())
	}()

	throughput, err := stats.Calibrate(ctx, conn)
	if err != nil {
		return err
	}

	snapshot.WithThroughput(throughput)

	return nil
}
