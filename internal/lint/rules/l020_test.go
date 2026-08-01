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
	"testing"
	"testing/fstest"

	"github.com/tochemey/mig/internal/lint"
	"github.com/tochemey/mig/internal/lint/rules"
	"github.com/tochemey/mig/internal/plan"
)

// TestL020 runs the rule's fixture: two tables locked for one transaction,
// next to the shapes that hold one lock or none.
func TestL020(t *testing.T) { golden(t, "l020") }

// runFiles lints a plan of several files and returns the findings for one
// rule, which is what the cross-file shapes need: a fixture is one file, and
// a relation created in an earlier file of the same plan is not fresh.
func runFiles(t *testing.T, rule string, files map[string]string) []rules.Finding {
	t.Helper()

	fsys := fstest.MapFS{}

	for name, body := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}

	loaded, err := plan.LoadFS(fsys)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	result, err := lint.Run(fsys, loaded, 18, nil, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	var found []rules.Finding

	for _, finding := range result.Findings {
		if finding.RuleID == rule {
			found = append(found, finding)
		}
	}

	return found
}

// TestL020CollapsesLockDomains pins that an index, a sequence and a view of
// the plan all belong to their table's lock domain: a step churning every
// dependent of one table alongside the table itself is one window, and a
// view whose base goes untouched stays a window of its own.
func TestL020CollapsesLockDomains(t *testing.T) {
	found := runFiles(t, rules.L020, map[string]string{
		"1_shape.sql": `
CREATE SEQUENCE accounts_id_seq;
CREATE TABLE accounts (id integer NOT NULL DEFAULT nextval('accounts_id_seq'::regclass));
CREATE INDEX idx_accounts_id ON accounts (id);
CREATE VIEW account_names AS SELECT id FROM accounts;
`,
		"2_one_domain.sql": `
DROP VIEW account_names;
DROP INDEX idx_accounts_id;
DROP SEQUENCE accounts_id_seq;
ALTER TABLE accounts ADD COLUMN note text;
CREATE VIEW account_names AS SELECT id, note FROM accounts;
`,
		"3_two_domains.sql": `
DROP VIEW account_names;
ALTER TABLE users ADD COLUMN note text;
`,
	})

	if len(found) != 1 {
		t.Fatalf("found %d L020 findings, want only the untouched-base one: %+v", len(found), found)
	}

	if found[0].File != "3_two_domains.sql" {
		t.Errorf("the finding sits in %s, want the file whose view base goes untouched", found[0].File)
	}
}
