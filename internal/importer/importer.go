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

// Package importer adopts a history written by another migration tool.
//
// Nobody rewrites their migrations to try a new tool, so adoption has to cost
// nothing: the existing files stay where they are and keep their contents, and
// the ledger learns which of them the other tool already applied.
package importer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/tochemey/mig/internal/ledger"
	"github.com/tochemey/mig/internal/plan"
)

// Source names the tool whose history is being adopted.
type Source string

const (
	// Goose reads goose_db_version, which records one row per apply and per
	// rollback.
	Goose Source = "goose"

	// GolangMigrate reads schema_migrations, which records only where the
	// history got to.
	GolangMigrate Source = "golang-migrate"
)

// ErrUnknownSource reports a source no adapter handles.
var ErrUnknownSource = errors.New("unknown migration source")

// Sources lists what can be adopted, for a flag's help and its validation.
func Sources() []Source {
	return []Source{Goose, GolangMigrate}
}

// History is what the other tool recorded.
//
// The two tools record different things and neither can be converted into the
// other. Goose keeps a row per version, so it knows a rolled-back migration is
// outstanding. golang-migrate keeps only where the history got to, so a hole in
// the middle is invisible to it. Carrying both shapes keeps the difference where
// it belongs — in what is known — rather than inventing a set for a tool that
// never had one.
type History struct {
	// Applied holds the versions the tool records as applied, when it records
	// them one by one.
	Applied []int64

	// Ceiling reports that the tool records only a high-water mark, in Through.
	Ceiling bool

	// Through is the newest version applied, when Ceiling is set. A tool set up
	// but never run leaves it below every version.
	Through int64

	// Dirty reports a golang-migrate run that died mid-migration.
	Dirty bool

	// DirtyVersion is the version the dirty flag refers to. It is left out of
	// the mark, so that the catalog decides how much of it landed.
	DirtyVersion int64
}

// Covers reports whether the other tool considers this version applied.
func (h History) Covers(version int64) bool {
	if h.Ceiling {
		return version <= h.Through
	}

	return slices.Contains(h.Applied, version)
}

// Report is what an import did.
type Report struct {
	// Source is the tool whose history was read.
	Source Source `json:"source"`

	// Adopted lists the migrations marked succeeded without being run.
	Adopted []string `json:"adopted"`

	// Recheck lists the migrations that will be reconciled on the next run,
	// because the other tool does not record them as applied.
	Recheck []string `json:"recheck"`

	// Unknown lists versions the other tool applied for which no file exists.
	//
	// It is reported rather than refused: files get deleted once everyone has
	// deployed past them, and refusing to adopt a history because of one would
	// make adoption impossible for exactly the long-lived repositories that
	// need it.
	Unknown []int64 `json:"unknown"`

	// Dirty repeats golang-migrate's flag, if it was set.
	Dirty bool `json:"dirty"`

	// DirtyVersion is the version the flag referred to.
	DirtyVersion int64 `json:"dirtyVersion,omitempty"`
}

// Read loads the history the other tool recorded.
func Read(ctx context.Context, db *sql.DB, source Source) (History, error) {
	switch source {
	case Goose:
		return readGoose(ctx, db)

	case GolangMigrate:
		return readGolangMigrate(ctx, db)

	default:
		return History{}, fmt.Errorf("%w: %q", ErrUnknownSource, source)
	}
}

// Import adopts the other tool's history into the ledger.
//
// Every file whose version that tool applied is written as a single step marked
// succeeded, so the next run skips it without asking the catalog. Everything
// else is left alone and reconciled normally.
//
// What counts as applied is whatever the other tool's own table says, never a
// guess: adopting a migration that never ran is the one failure with no
// recovery, because the step is then skipped forever and nothing is left to
// notice it. A version the tool does not cover is simply left to reconcile,
// which costs a catalog lookup and is always safe.
func Import(ctx context.Context, db *sql.DB, fence ledger.Fence,
	p *plan.Plan, source Source) (Report, error) {
	history, err := Read(ctx, db, source)
	if err != nil {
		return Report{}, err
	}

	report := Report{
		Source:       source,
		Adopted:      []string{},
		Recheck:      []string{},
		Unknown:      []int64{},
		Dirty:        history.Dirty,
		DirtyVersion: history.DirtyVersion,
	}

	known := make(map[int64]bool, len(p.Migrations))

	for _, migration := range p.Migrations {
		known[migration.Version] = true

		if !history.Covers(migration.Version) {
			report.Recheck = append(report.Recheck, migration.ID)
			continue
		}

		// The report goes back with the error rather than being thrown away.
		// Each migration is adopted in its own transaction, so what is listed
		// has landed, and re-running picks up from there.
		if err := adopt(ctx, db, fence, migration); err != nil {
			return report, err
		}

		report.Adopted = append(report.Adopted, migration.ID)
	}

	for _, version := range history.Applied {
		if !known[version] {
			report.Unknown = append(report.Unknown, version)
		}
	}

	return report, nil
}

// adopt writes one migration's rows as already succeeded.
//
// The rows commit together, so an import killed part way through leaves whole
// migrations adopted and the rest untouched — and re-running it adopts the rest.
func adopt(ctx context.Context, db *sql.DB, fence ledger.Fence, migration plan.Migration) error {
	write := func(ctx context.Context, tx *sql.Tx) error {
		if err := ledger.UpsertMigration(ctx, tx, migration.ID, migration.Name); err != nil {
			return err
		}

		for _, s := range migration.Steps {
			key := ledger.StepKey{MigrationID: migration.ID, Index: s.Index}

			if err := ledger.UpsertStep(ctx, tx, key, s.Name, string(s.Kind), s.Checksum); err != nil {
				return err
			}

			if err := ledger.SetStepStatus(ctx, tx, key, ledger.StatusSucceeded, ""); err != nil {
				return err
			}
		}

		return ledger.SetMigrationStatus(ctx, tx, migration.ID, ledger.StatusSucceeded)
	}

	if err := ledger.Write(ctx, db, fence, write); err != nil {
		return fmt.Errorf("adopt migration %q: %w", migration.ID, err)
	}

	return nil
}
