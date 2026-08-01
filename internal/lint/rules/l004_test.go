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
	"strings"
	"testing"

	"github.com/tochemey/mig/internal/lint/rules"
)

// TestL004 runs the rule's fixture: the hazard in its flagged form next to
// the spellings and orderings that must stay silent.
func TestL004(t *testing.T) { golden(t, "l004") }

// TestL004KnowsTheNoRewriteTransitions pins the changes Postgres performs in
// place when the plan's own DDL shows the prior type, and that everything
// else, the unknown prior included, keeps the warning.
func TestL004KnowsTheNoRewriteTransitions(t *testing.T) {
	found := runFiles(t, rules.L004, map[string]string{
		"1_shape.sql": `
CREATE TABLE profiles (bio varchar(100), score numeric(10, 2), age integer, code varchar(8));
`,
		"2_change.sql": `
ALTER TABLE profiles ALTER COLUMN bio TYPE varchar(255);
ALTER TABLE profiles ALTER COLUMN bio TYPE varchar;
ALTER TABLE profiles ALTER COLUMN bio TYPE text;
ALTER TABLE profiles ALTER COLUMN score TYPE numeric(12, 2);
ALTER TABLE profiles ALTER COLUMN score TYPE numeric;
ALTER TABLE profiles ALTER COLUMN age TYPE integer;
ALTER TABLE profiles ALTER COLUMN age TYPE bigint;
ALTER TABLE profiles ALTER COLUMN code TYPE varchar(4);
ALTER TABLE profiles ALTER COLUMN score TYPE numeric(12, 4);
ALTER TABLE profiles ALTER COLUMN mystery TYPE text;
`,
	})

	// The widenings, the unbindings, the move to text and the no-op stay
	// silent; the bigint, the narrowed varchar, the reshaped numeric and the
	// column the plan never typed all warn.
	if len(found) != 4 {
		t.Fatalf("found %d L004 findings, want 4: %+v", len(found), found)
	}

	for _, finding := range found {
		if !strings.Contains(finding.Message, "profiles") {
			t.Errorf("a finding lost its table: %q", finding.Message)
		}
	}
}
