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
	"io/fs"

	"github.com/tochemey/mig/internal/lint"
	"github.com/tochemey/mig/internal/lint/rules"
	"github.com/tochemey/mig/internal/plan"
)

// Finding is one hazard the linter reports.
type Finding = rules.Finding

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

// Lint checks every migration in fsys against the offline rule catalog,
// without connecting to anything. The target version decides
// version-sensitive behaviour, such as whether an added default rewrites.
func Lint(fsys fs.FS, targetVersion int) (*LintReport, error) {
	loaded, err := plan.LoadFS(fsys)
	if err != nil {
		return nil, err
	}

	findings, sources, err := lint.Run(fsys, loaded, targetVersion)
	if err != nil {
		return nil, err
	}

	return &LintReport{Findings: findings, Sources: sources}, nil
}
