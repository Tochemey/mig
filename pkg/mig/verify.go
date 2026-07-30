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
	"fmt"
	"io/fs"
	"strings"

	"github.com/tochemey/mig/internal/exec"
	"github.com/tochemey/mig/internal/plan"
)

// ErrPending reports migrations the database does not yet show.
//
// It is the error a service startup check acts on, so it is a sentinel rather
// than a message: a caller distinguishes "not migrated yet" from "could not
// tell", and the two call for different responses.
var ErrPending = errors.New("migrations pending")

// Verify reports whether every step in fsys is already applied.
//
// It writes nothing, takes no lease, and judges each step by the catalog, so it
// is safe to call from every replica at once. A database with work outstanding
// returns an error wrapping [ErrPending] that names what is missing.
func Verify(ctx context.Context, db *sql.DB, fsys fs.FS) error {
	outstanding, err := Pending(ctx, db, fsys)
	if err != nil {
		return err
	}

	if len(outstanding) == 0 {
		return nil
	}

	names := make([]string, 0, len(outstanding))
	for _, step := range outstanding {
		names = append(names, step.String())
	}

	return fmt.Errorf("%w: %d step(s) outstanding: %s",
		ErrPending, len(outstanding), strings.Join(names, ", "))
}

// Outstanding names a step whose work the database does not yet show.
type Outstanding = exec.Outstanding

// Pending lists the steps that [Verify] would refuse on, for a caller that
// wants to report them itself rather than act on a single error.
func Pending(ctx context.Context, db *sql.DB, fsys fs.FS) ([]Outstanding, error) {
	loaded, err := plan.LoadFS(fsys)
	if err != nil {
		return nil, err
	}

	return exec.Pending(ctx, db, loaded)
}
