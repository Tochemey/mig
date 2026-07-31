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

package rules_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/tochemey/mig/internal/lint"
	"github.com/tochemey/mig/internal/lint/report"
	"github.com/tochemey/mig/internal/lint/rules"
	"github.com/tochemey/mig/internal/plan"
)

// update rewrites the golden files instead of comparing against them. Run
// with: go test ./internal/lint/rules -update, then audit the diff by hand.
var update = flag.Bool("update", false, "rewrite the golden files")

// golden runs one fixture through the whole pipeline at the newest supported
// major and compares the findings against the fixture's golden JSON.
func golden(t *testing.T, id string) {
	t.Helper()
	goldenAt(t, id, 18)
}

// goldenAt is golden pinned to an older target version, for the rules whose
// behaviour flips with it.
func goldenAt(t *testing.T, id string, version int) {
	t.Helper()

	source, err := os.ReadFile(filepath.Join("testdata", id+".sql"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	fsys := fstest.MapFS{"1_" + id + ".sql": &fstest.MapFile{Data: source}}

	loaded, err := plan.LoadFS(fsys)
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}

	findings, _, err := lint.Run(fsys, loaded, version)
	if err != nil {
		t.Fatalf("lint fixture: %v", err)
	}

	var got bytes.Buffer

	if err := report.JSON(&got, findings); err != nil {
		t.Fatalf("render findings: %v", err)
	}

	path := filepath.Join("testdata", id+".json")

	if *update {
		if err := os.WriteFile(path, got.Bytes(), 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (create it with -update): %v", err)
	}

	if !bytes.Equal(got.Bytes(), want) {
		t.Errorf("findings diverge from %s\ngot:\n%s\nwant:\n%s", path, got.Bytes(), want)
	}
}

func TestCatalogueIDs(t *testing.T) {
	seen := make(map[string]bool)
	previous := ""

	for _, rule := range rules.All() {
		id := rule.ID()

		if seen[id] {
			t.Errorf("rule id %q appears twice", id)
		}

		if id <= previous {
			t.Errorf("rule id %q out of order after %q", id, previous)
		}

		seen[id] = true
		previous = id
	}

	if len(seen) != 11 {
		t.Errorf("catalog has %d rules, want 11", len(seen))
	}
}

func TestSeverityRendering(t *testing.T) {
	names := map[rules.Severity]string{
		rules.SeverityInfo:  "info",
		rules.SeverityWarn:  "warn",
		rules.SeverityError: "error",
		rules.Severity(0):   "unknown",
	}

	for severity, want := range names {
		if got := severity.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", severity, got, want)
		}

		encoded, err := json.Marshal(severity)
		if err != nil {
			t.Fatalf("marshal %q: %v", want, err)
		}

		if string(encoded) != `"`+want+`"` {
			t.Errorf("marshal %d = %s, want %q", severity, encoded, want)
		}
	}
}
