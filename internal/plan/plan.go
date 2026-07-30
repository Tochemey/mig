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
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/tochemey/mig/internal/parse"
	"github.com/tochemey/mig/internal/predicate"
	"github.com/tochemey/mig/internal/step"
)

// Extension is the suffix of a migration file.
const Extension = ".sql"

const (
	// downSuffix marks a golang-migrate rollback file, which is not loaded.
	downSuffix = ".down" + Extension

	// upSuffix marks a golang-migrate forward file. It is trimmed from the
	// identifier so that an adopted migration is named the same whether or not
	// the directory it came from used the convention.
	upSuffix = ".up"
)

// versionPattern is the leading number every migration file starts with.
//
// The format's own version is a zero-padded timestamp, and ordering is by the
// number rather than by the text so that a shorter sequential version — which
// is what an adopted goose or golang-migrate history carries — sorts where it
// belongs rather than where its digits fall.
var versionPattern = regexp.MustCompile(`^(\d+)_(.+)$`)

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

	var satisfied step.Predicate

	if check := s.Check(); check != nil {
		satisfied = check.Satisfied
	}

	switch s.Kind {
	case step.KindDDLTx:
		return step.NewDDLTx(meta, s.Statements, satisfied), nil

	case step.KindDDLNoTx:
		return step.NewDDLNoTx(meta, s.Statements, satisfied)

	case step.KindBackfill:
		if len(s.Statements) != 1 {
			return nil, fmt.Errorf("step %q: %w: a backfill takes one statement, got %d",
				s.Name, ErrBadBackfill, len(s.Statements))
		}

		return step.NewBackfill(meta, s.Statements[0].SQL, s.Backfill, satisfied)

	default:
		return nil, fmt.Errorf("step %q: %w", s.Name, ErrKindUnsupported)
	}
}

// Check returns the author's predicate when there is one, otherwise the
// inferred one, otherwise nil.
//
// It is exported so that the plan command can show what a run will check
// without connecting to anything.
func (s Step) Check() *predicate.Check {
	if s.Satisfied != "" {
		return predicate.SQL(s.Satisfied)
	}

	return predicate.Infer(s.Statements)
}

// ErrKindUnsupported reports a step kind this build cannot execute.
var ErrKindUnsupported = errors.New("step kind not supported yet")

// Migration is one migration file.
type Migration struct {
	// ID is the file name without its extension, and is what the ledger holds.
	ID string

	// Version orders the migration against its siblings.
	Version int64

	Name string
	File string

	Steps []Step
}

// Plan is the ordered set of migrations in a source.
type Plan struct {
	Migrations []Migration
}

// Load reads every migration in dir, in version order.
func Load(dir string) (*Plan, error) {
	loaded, err := LoadFS(os.DirFS(dir))
	if err != nil {
		return nil, fmt.Errorf("%w (in %q)", err, dir)
	}

	return loaded, nil
}

// LoadFS reads every migration from fsys, in version order.
//
// It takes a file system rather than a path so that a service binary can embed
// its migrations and check them at startup without shipping the directory
// alongside the binary.
func LoadFS(fsys fs.FS) (*Plan, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read migration directory: %w", err)
	}

	names := make([]string, 0, len(entries))

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), Extension) {
			continue
		}

		// A golang-migrate directory holds an up and a down file per version.
		// Loading both would read them as two migrations claiming one version,
		// so an adopted directory would refuse to load at all.
		if strings.HasSuffix(entry.Name(), downSuffix) {
			continue
		}

		names = append(names, entry.Name())
	}

	if len(names) == 0 {
		return nil, ErrNoMigrations
	}

	migrations := make([]Migration, 0, len(names))
	seen := make(map[int64]string, len(names))

	for _, name := range names {
		migration, err := loadMigration(fsys, name)
		if err != nil {
			return nil, err
		}

		if previous, clash := seen[migration.Version]; clash {
			return nil, fmt.Errorf("%w %d: %q and %q",
				ErrDuplicateVersion, migration.Version, previous, name)
		}

		seen[migration.Version] = name
		migrations = append(migrations, migration)
	}

	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].Version < migrations[j].Version
	})

	return &Plan{Migrations: migrations}, nil
}

// loadMigration reads and validates one migration file.
func loadMigration(fsys fs.FS, name string) (Migration, error) {
	base := strings.TrimSuffix(strings.TrimSuffix(name, Extension), upSuffix)

	match := versionPattern.FindStringSubmatch(base)
	if match == nil {
		return Migration{}, fmt.Errorf(
			"migration %q: name must be <version>_<name>%s", name, Extension)
	}

	version, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		return Migration{}, fmt.Errorf("migration %q: version %q is not a number", name, match[1])
	}

	content, err := fs.ReadFile(fsys, name)
	if err != nil {
		return Migration{}, fmt.Errorf("read migration %q: %w", name, err)
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
		Version: version,
		Name:    match[2],
		File:    name,
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
