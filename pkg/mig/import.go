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
	"io/fs"

	"github.com/tochemey/mig/internal/importer"
	"github.com/tochemey/mig/internal/ledger"
	"github.com/tochemey/mig/internal/plan"
)

// Source names a migration tool whose history can be adopted.
type Source = importer.Source

const (
	// Goose reads goose_db_version.
	Goose = importer.Goose

	// GolangMigrate reads schema_migrations.
	GolangMigrate = importer.GolangMigrate
)

// ErrUnknownSource reports a source no adapter handles.
var ErrUnknownSource = importer.ErrUnknownSource

// Sources lists the tools whose histories can be adopted.
func Sources() []Source {
	return importer.Sources()
}

// ImportReport is what an import adopted and what it left to reconcile.
type ImportReport = importer.Report

// Import adopts a history written by another tool, so that the migrations it
// already applied are not applied again.
//
// It takes the lease, because it writes to the same ledger rows a run does.
func Import(ctx context.Context, db *sql.DB, fsys fs.FS,
	source Source, opts Options) (ImportReport, error) {
	loaded, err := plan.LoadFS(fsys)
	if err != nil {
		return ImportReport{}, err
	}

	var report ImportReport

	err = underLease(ctx, db, opts, func(ctx context.Context, fence ledger.Fence) error {
		var importErr error

		// The report is kept whatever happens: each migration is adopted in its
		// own transaction, so a failure part way through still has to say what
		// landed before it.
		report, importErr = importer.Import(ctx, db, fence, loaded, source)

		return importErr
	})

	return report, err
}
