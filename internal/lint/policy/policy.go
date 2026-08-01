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

// Package policy holds what a team decides about the rule catalog: the
// severity each rule carries, the sizes a size-dependent hazard is graded by,
// and the suppressions that silence a rule where the team has looked and
// disagreed.
//
// Both halves exist so the linter can be kept honest rather than muted. A
// severity is configurable so nobody has to turn the tool off to get past a
// rule they have thought about, and a suppression must give a reason and can
// be listed with its age, so silence is auditable rather than permanent.
package policy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/tochemey/mig/internal/lint/rules"
)

// FileName is the policy file's conventional name.
const FileName = ".miglint.yaml"

// off is the severity a rule the policy turns off carries. Severities start
// at one, so the zero value is free for it and no finding can hold it.
const off = rules.Severity(0)

// reasonL000 says why the linter's own rule is neither gradable nor
// suppressible, in the one place both refusals read it from.
const reasonL000 = "it reports a lint directive this build cannot honour, and a suppression " +
	"that could silence the complaint about itself would never be fixed"

// levels are the severities a policy file may name.
var levels = map[string]rules.Severity{
	"off":   off,
	"info":  rules.SeverityInfo,
	"warn":  rules.SeverityWarn,
	"error": rules.SeverityError,
}

// document is the policy file's shape on disk.
type document struct {
	// TargetVersion is the Postgres major the migrations are written for,
	// used when the command line does not name one and no database was read.
	TargetVersion int `yaml:"target_version"`

	// Thresholds move the sizes a size-dependent hazard changes grade at.
	Thresholds sizes `yaml:"thresholds"`

	// Rules assigns severities: off, info, warn or error.
	Rules map[string]string `yaml:"rules"`

	// Overrides re-assign them for one migration directory.
	Overrides []scope `yaml:"overrides"`
}

// sizes is the thresholds block. They are plain counts and plain bytes: a
// number that means what it says beats a suffix the reader has to trust.
type sizes struct {
	BigRows    int64 `yaml:"big_rows"`
	BigBytes   int64 `yaml:"big_bytes"`
	SmallRows  int64 `yaml:"small_rows"`
	SmallBytes int64 `yaml:"small_bytes"`
}

// scope is one per-directory override.
type scope struct {
	Path  string            `yaml:"path"`
	Rules map[string]string `yaml:"rules"`
}

// Policy is what a policy file says, resolved for one migration directory.
//
// A nil *Policy is the absence of a policy file and every method is nil-safe,
// so the linter runs the same way with one and without.
type Policy struct {
	targetVersion int
	limits        rules.Thresholds
	severities    map[string]rules.Severity
}

// Load reads the policy file at path and resolves it for the directory being
// linted, which is what the per-directory overrides are keyed by.
func Load(path, dir string) (*Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read policy: %w", err)
	}

	loaded, err := Parse(data, dir)
	if err != nil {
		return nil, fmt.Errorf("policy %q: %w", path, err)
	}

	return loaded, nil
}

// Parse reads a policy and resolves it for dir.
//
// An unknown field is an error rather than a line quietly skipped, for the
// reason the loader rejects an unknown annotation: a misspelled setting the
// author believes is in force is worse than no setting at all.
func Parse(data []byte, dir string) (*Policy, error) {
	var doc document

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)

	// An empty file is a policy that says nothing, which is not an error.
	if err := decoder.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse policy: %w", err)
	}

	severities, err := severitiesOf(doc.Rules)
	if err != nil {
		return nil, err
	}

	// Every override is read, and only the matching ones are folded in: a
	// misspelled rule under a directory this run is not linting is still a
	// mistake, and finding it here beats finding it on the run that needed it.
	for _, scoped := range doc.Overrides {
		if scoped.Path == "" {
			return nil, errors.New("an override needs a path")
		}

		overridden, err := severitiesOf(scoped.Rules)
		if err != nil {
			return nil, fmt.Errorf("override %q: %w", scoped.Path, err)
		}

		if under(dir, scoped.Path) {
			maps.Copy(severities, overridden)
		}
	}

	limits, err := thresholdsOf(doc.Thresholds)
	if err != nil {
		return nil, err
	}

	return &Policy{targetVersion: doc.TargetVersion, limits: limits, severities: severities}, nil
}

// TargetVersion is the Postgres major the policy names, and zero when it
// names none.
func (p *Policy) TargetVersion() int {
	if p == nil {
		return 0
	}

	return p.targetVersion
}

// Thresholds are the sizes the policy grades by. A field it does not name
// stays zero, which the rules read as their own default.
func (p *Policy) Thresholds() rules.Thresholds {
	if p == nil {
		return rules.Thresholds{}
	}

	return p.limits
}

// Apply re-grades the findings the policy has an opinion about, and drops the
// ones whose rule it turns off.
func (p *Policy) Apply(findings []rules.Finding) []rules.Finding {
	if p == nil || len(p.severities) == 0 {
		return findings
	}

	kept := make([]rules.Finding, 0, len(findings))

	for _, finding := range findings {
		severity, named := p.severities[finding.RuleID]

		switch {
		case named && severity == off:
			continue
		case named:
			finding.Severity = severity
		}

		kept = append(kept, finding)
	}

	return kept
}

// severitiesOf reads one rules block, rejecting a rule or a severity this
// build does not have.
func severitiesOf(assigned map[string]string) (map[string]rules.Severity, error) {
	severities := make(map[string]rules.Severity, len(assigned))

	for id, name := range assigned {
		switch {
		case rules.Describe(id) == "":
			return nil, fmt.Errorf("%q is no rule this build has", id)

		case id == rules.L000:
			return nil, fmt.Errorf("%s cannot be graded: %s", rules.L000, reasonL000)
		}

		severity, ok := levels[name]
		if !ok {
			return nil, fmt.Errorf("rule %s: %q is not off, info, warn or error", id, name)
		}

		severities[id] = severity
	}

	return severities, nil
}

// thresholdsOf reads the thresholds block.
func thresholdsOf(block sizes) (rules.Thresholds, error) {
	limits := rules.Thresholds{
		BigRows:    block.BigRows,
		BigBytes:   block.BigBytes,
		SmallRows:  block.SmallRows,
		SmallBytes: block.SmallBytes,
	}

	if limits.BigRows < 0 || limits.BigBytes < 0 || limits.SmallRows < 0 || limits.SmallBytes < 0 {
		return rules.Thresholds{}, errors.New("a threshold cannot be negative")
	}

	return limits, nil
}

// under reports whether dir is path, or sits inside it.
func under(dir, path string) bool {
	dir, path = clean(dir), clean(path)

	return dir == path || strings.HasPrefix(dir, path+"/")
}

// clean puts a path in the one form the comparison can be made in.
func clean(path string) string {
	return filepath.ToSlash(filepath.Clean(path))
}
