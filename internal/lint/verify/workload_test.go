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

package verify

import (
	"strings"
	"testing"
	"time"
)

// minimal is a workload with everything required and nothing else, so the
// defaults are what the rest of the file is about.
const minimal = `
setup:
  - CREATE TABLE t (id int)
queries:
  - name: read
    sql: SELECT 1
slow_read:
  sql: SELECT pg_sleep(1)
`

func TestParseConfigFillsInWhatTheFileLeftOut(t *testing.T) {
	config, err := ParseConfig([]byte(minimal))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if config.Baseline != defaultBaseline || config.Settle != defaultSettle {
		t.Errorf("windows are %s and %s, want the defaults", config.Baseline, config.Settle)
	}

	if config.SlowRead.Every != defaultSlowRead {
		t.Errorf("slow read runs every %s, want the default", config.SlowRead.Every)
	}

	if config.Queries[0].Rate != defaultRate {
		t.Errorf("rate = %d, want the default", config.Queries[0].Rate)
	}
}

func TestParseConfigKeepsWhatTheFileSaid(t *testing.T) {
	config, err := ParseConfig([]byte(minimal + `
keys: 500
baseline: 2s
settle: 3s
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if config.Baseline != 2*time.Second || config.Settle != 3*time.Second {
		t.Errorf("windows are %s and %s, want 2s and 3s", config.Baseline, config.Settle)
	}

	if config.Keys != 500 {
		t.Errorf("keys = %d, want 500", config.Keys)
	}
}

// TestParseConfigRefusesWhatCannotBeMeasured covers the validation, and the
// slow reader in particular: the design's review checklist rejects a workload
// without one, because the hazard it exists to reproduce never forms.
func TestParseConfigRefusesWhatCannotBeMeasured(t *testing.T) {
	cases := map[string]string{
		"not yaml at all": "\tqueries: [",
		"no setup": `
queries:
  - name: read
    sql: SELECT 1
slow_read:
  sql: SELECT pg_sleep(1)
`,
		"no queries": `
setup:
  - CREATE TABLE t (id int)
slow_read:
  sql: SELECT pg_sleep(1)
`,
		"a query with no sql": `
setup:
  - CREATE TABLE t (id int)
queries:
  - name: read
slow_read:
  sql: SELECT pg_sleep(1)
`,
		"a key with no range": `
setup:
  - CREATE TABLE t (id int)
queries:
  - name: read
    sql: SELECT $1
    key: true
slow_read:
  sql: SELECT pg_sleep(1)
`,
		"no slow reader": `
setup:
  - CREATE TABLE t (id int)
queries:
  - name: read
    sql: SELECT 1
`,
	}

	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseConfig([]byte(text)); err == nil {
				t.Error("the workload was accepted")
			}
		})
	}
}

// TestParseConfigSaysWhyTheReaderIsRequired covers the wording, because the
// refusal is the only place a person learns what the reader is for.
func TestParseConfigSaysWhyTheReaderIsRequired(t *testing.T) {
	_, err := ParseConfig([]byte(`
setup:
  - CREATE TABLE t (id int)
queries:
  - name: read
    sql: SELECT 1
`))

	if err == nil || !strings.Contains(err.Error(), "blocks everything arriving after it") {
		t.Errorf("err = %v, want the reason the reader is required", err)
	}
}
