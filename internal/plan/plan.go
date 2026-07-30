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

// Package plan turns a directory of annotated SQL into ordered, executable
// steps.
//
// Hand-written, ordered, reviewable SQL is the format. Annotations mark step
// boundaries and the few properties the parser cannot infer.
package plan

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/tochemey/mig/internal/parse"
	"github.com/tochemey/mig/internal/predicate"
	"github.com/tochemey/mig/internal/step"
)

// Extension is the suffix of a migration file.
const Extension = ".sql"

// versionPattern is the zero-padded timestamp every migration file starts with.
var versionPattern = regexp.MustCompile(`^(\d{14})_(.+)$`)

var (
	// ErrNoMigrations reports a directory with no migration files.
	ErrNoMigrations = errors.New("no migration files found")

	// ErrDuplicateVersion reports two migrations claiming the same timestamp,
	// which would make their order depend on the file name rather than on when
	// they were written.
	ErrDuplicateVersion = errors.New("duplicate migration version")

	// ErrDuplicateStep reports two steps sharing a name within one migration.
	ErrDuplicateStep = errors.New("duplicate step name")
)

// Step is one annotated block of a migration.
type Step struct {
	Index      int
	Name       string
	Kind       step.Kind
	SQL        string
	Checksum   []byte
	Statements []parse.Statement

	// Satisfied is the author's predicate expression, empty when inferred.
	Satisfied string

	// Backfill carries the backfill annotation's settings, for that kind only.
	Backfill step.BackfillConfig

	// NoLockTimeout suppresses the default lock_timeout for this step.
	NoLockTimeout bool
}

// Build returns the executable form of the step.
func (s Step) Build() (step.Step, error) {
	meta := step.Meta{Name: s.Name, Kind: s.Kind, Checksum: s.Checksum, SQL: s.SQL}

	check := s.predicate()

	switch s.Kind {
	case step.KindDDLTx:
		return step.NewDDLTx(meta, s.Statements, check), nil

	case step.KindDDLNoTx:
		return step.NewDDLNoTx(meta, s.Statements, check)

	case step.KindBackfill:
		if len(s.Statements) != 1 {
			return nil, fmt.Errorf("step %q: %w: a backfill takes one statement, got %d",
				s.Name, ErrBadBackfill, len(s.Statements))
		}

		return step.NewBackfill(meta, s.Statements[0].SQL, s.Backfill, check)

	default:
		return nil, fmt.Errorf("step %q: %w", s.Name, ErrKindUnsupported)
	}
}

// predicate returns the author's predicate when there is one, otherwise the
// inferred one, otherwise nil.
func (s Step) predicate() step.Predicate {
	if s.Satisfied != "" {
		return predicate.SQL(s.Satisfied)
	}

	return predicate.Infer(s.Statements)
}

// ErrKindUnsupported reports a step kind this build cannot execute.
var ErrKindUnsupported = errors.New("step kind not supported yet")

// Migration is one migration file.
type Migration struct {
	ID      string
	Version string
	Name    string
	Path    string
	Steps   []Step
}

// Plan is the ordered set of migrations in a directory.
type Plan struct {
	Dir        string
	Migrations []Migration
}

// Load reads every migration in dir, in version order.
func Load(dir string) (*Plan, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read migration directory %q: %w", dir, err)
	}

	names := make([]string, 0, len(entries))

	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), Extension) {
			names = append(names, entry.Name())
		}
	}

	if len(names) == 0 {
		return nil, fmt.Errorf("%w in %q", ErrNoMigrations, dir)
	}

	// Lexicographic order over zero-padded timestamps is chronological order.
	sort.Strings(names)

	plan := &Plan{Dir: dir, Migrations: make([]Migration, 0, len(names))}
	seen := make(map[string]string, len(names))

	for _, name := range names {
		migration, err := loadMigration(dir, name)
		if err != nil {
			return nil, err
		}

		if previous, clash := seen[migration.Version]; clash {
			return nil, fmt.Errorf("%w %s: %q and %q", ErrDuplicateVersion, migration.Version, previous, name)
		}

		seen[migration.Version] = name
		plan.Migrations = append(plan.Migrations, migration)
	}

	return plan, nil
}

// loadMigration reads and validates one migration file.
func loadMigration(dir, name string) (Migration, error) {
	base := strings.TrimSuffix(name, Extension)

	match := versionPattern.FindStringSubmatch(base)
	if match == nil {
		return Migration{}, fmt.Errorf(
			"migration %q: name must be <YYYYMMDDHHMMSS>_<name>%s", name, Extension)
	}

	path := filepath.Join(dir, name)

	//nolint:gosec // G304: the path is a migration file the operator pointed us at.
	content, err := os.ReadFile(path)
	if err != nil {
		return Migration{}, fmt.Errorf("read migration %q: %w", path, err)
	}

	steps, err := scan(string(content))
	if err != nil {
		return Migration{}, fmt.Errorf("migration %q: %w", name, err)
	}

	if err := checkStepNames(name, steps); err != nil {
		return Migration{}, err
	}

	return Migration{
		ID:      base,
		Version: match[1],
		Name:    match[2],
		Path:    path,
		Steps:   steps,
	}, nil
}

// checkStepNames rejects duplicates, which would make ledger rows ambiguous to
// anyone reading them.
func checkStepNames(file string, steps []Step) error {
	seen := make(map[string]struct{}, len(steps))

	for _, s := range steps {
		if _, clash := seen[s.Name]; clash {
			return fmt.Errorf("migration %q: %w %q", file, ErrDuplicateStep, s.Name)
		}

		seen[s.Name] = struct{}{}
	}

	return nil
}
