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

package mig_test

import (
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/tochemey/mig/pkg/mig"
)

// hazardous is a migration with one warning and one error.
const hazardous = `-- +mig step: index
CREATE INDEX idx_users_email ON users (email);

-- +mig step: compact
-- +mig notx
VACUUM FULL users;
`

func TestLintReportsHazards(t *testing.T) {
	fsys := fstest.MapFS{"1_m.sql": &fstest.MapFile{Data: []byte(hazardous)}}

	linted, err := mig.Lint(fsys, mig.DefaultTargetVersion)
	if err != nil {
		t.Fatalf("lint: %v", err)
	}

	if len(linted.Findings) != 2 {
		t.Fatalf("found %d findings, want 2: %+v", len(linted.Findings), linted.Findings)
	}

	if linted.Findings[0].RuleID != "L001" || linted.Findings[1].RuleID != "L010" {
		t.Errorf("found %s and %s, want L001 and L010",
			linted.Findings[0].RuleID, linted.Findings[1].RuleID)
	}

	if linted.Errors() != 1 {
		t.Errorf("Errors() = %d, want 1", linted.Errors())
	}

	if linted.Sources["1_m.sql"] != hazardous {
		t.Error("the report does not carry the linted source")
	}
}

func TestLintRejectsAnEmptyDirectory(t *testing.T) {
	if _, err := mig.Lint(fstest.MapFS{}, mig.DefaultTargetVersion); err == nil {
		t.Error("an empty directory linted clean")
	}
}

// vanishingFS serves a file a limited number of times, standing in for a
// directory that changes under the linter.
type vanishingFS struct {
	inner fs.FS
	name  string
	opens int
	limit int
}

func (v *vanishingFS) Open(name string) (fs.File, error) {
	if name == v.name {
		v.opens++

		if v.opens > v.limit {
			return nil, errors.New("vanished")
		}
	}

	return v.inner.Open(name)
}

func TestLintReportsAVanishingFile(t *testing.T) {
	inner := fstest.MapFS{"1_m.sql": &fstest.MapFile{Data: []byte(hazardous)}}

	// The loader reads the file once; the linter's second read fails.
	fsys := &vanishingFS{inner: inner, name: "1_m.sql", limit: 1}

	if _, err := mig.Lint(fsys, mig.DefaultTargetVersion); err == nil {
		t.Error("a vanishing migration linted clean")
	}
}
