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

package report

import (
	"encoding/json"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tochemey/mig/internal/lint/rules"
)

// The document's fixed fields.
const (
	sarifSchema  = "https://json.schemastore.org/sarif-2.1.0.json"
	sarifVersion = "2.1.0"
	toolName     = "mig lint"
	toolURI      = "https://github.com/tochemey/mig"
)

// levels maps a severity to the level a code-scanning UI reads. SARIF's
// "note" is this catalog's info: an observation that annotates the diff
// without failing anything.
var levels = map[rules.Severity]string{
	rules.SeverityInfo:  "note",
	rules.SeverityWarn:  "warning",
	rules.SeverityError: "error",
}

// document is one SARIF log.
type document struct {
	Schema  string `json:"$schema"`
	Version string `json:"version"`
	Runs    []run  `json:"runs"`
}

// run is one invocation of one tool.
type run struct {
	Tool    tool     `json:"tool"`
	Results []result `json:"results"`
}

// tool names what produced the results.
type tool struct {
	Driver driver `json:"driver"`
}

// driver carries the tool's identity and the rules it ran.
type driver struct {
	Name           string       `json:"name"`
	Version        string       `json:"version,omitempty"`
	InformationURI string       `json:"informationUri"`
	Rules          []descriptor `json:"rules"`
}

// descriptor says what a rule detects, which is what a code-scanning UI shows
// beside the alert.
type descriptor struct {
	ID               string `json:"id"`
	ShortDescription text   `json:"shortDescription"`
}

// text is SARIF's string wrapper.
type text struct {
	Text string `json:"text"`
}

// result is one finding.
type result struct {
	RuleID    string     `json:"ruleId"`
	Level     string     `json:"level"`
	Message   text       `json:"message"`
	Locations []location `json:"locations"`
}

// location is where a finding sits.
type location struct {
	PhysicalLocation physical `json:"physicalLocation"`
}

// physical names the file and the span within it.
type physical struct {
	ArtifactLocation artifact `json:"artifactLocation"`
	Region           *region  `json:"region,omitempty"`
}

// artifact is the file.
type artifact struct {
	URI string `json:"uri"`
}

// region is the span, in SARIF's 1-based lines and columns.
type region struct {
	StartLine   int `json:"startLine"`
	StartColumn int `json:"startColumn"`
	EndLine     int `json:"endLine"`
	EndColumn   int `json:"endColumn"`
}

// SARIF renders findings for GitHub code scanning, which annotates the
// changed lines of a pull request with them.
//
// The root is the migration directory as the repository sees it, since a
// finding's file is named relative to that directory and an annotation
// anchored anywhere else lands on no diff at all. The version identifies the
// build that produced the run.
func SARIF(w io.Writer, findings []rules.Finding, sources map[string]string, root, version string) error {
	results := make([]result, 0, len(findings))

	for _, finding := range findings {
		results = append(results, result{
			RuleID:    finding.RuleID,
			Level:     levels[finding.Severity],
			Message:   text{Text: sarifMessage(finding)},
			Locations: []location{locate(finding, sources[finding.File], root)},
		})
	}

	log := document{
		Schema:  sarifSchema,
		Version: sarifVersion,
		Runs: []run{{
			Tool: tool{Driver: driver{
				Name:           toolName,
				Version:        version,
				InformationURI: toolURI,
				Rules:          descriptors(findings),
			}},
			Results: results,
		}},
	}

	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")

	if err := encoder.Encode(log); err != nil {
		return fmt.Errorf("write findings: %w", err)
	}

	return nil
}

// sarifMessage is the finding as a reader of the alert needs it: what will
// happen, the lock behind it, and how long it is expected to hold.
func sarifMessage(finding rules.Finding) string {
	lines := []string{finding.Message}

	if finding.Detail != "" {
		lines = append(lines, finding.Detail)
	}

	if finding.Estimate != "" {
		lines = append(lines, finding.Estimate)
	}

	return strings.Join(lines, "\n")
}

// descriptors lists the rules that fired, each with what it detects.
func descriptors(findings []rules.Finding) []descriptor {
	seen := make(map[string]bool, len(findings))

	ids := make([]string, 0, len(findings))

	for _, finding := range findings {
		if !seen[finding.RuleID] {
			seen[finding.RuleID] = true
			ids = append(ids, finding.RuleID)
		}
	}

	sort.Strings(ids)

	listed := make([]descriptor, 0, len(ids))

	for _, id := range ids {
		listed = append(listed, descriptor{ID: id, ShortDescription: text{Text: rules.Describe(id)}})
	}

	return listed
}

// locate turns a span into a SARIF location. A finding the engine could not
// place, which is what a backfill's rewritten statement produces, is reported
// against the file without a region rather than against line zero.
func locate(finding rules.Finding, source, root string) location {
	place := physical{ArtifactLocation: artifact{URI: uriOf(root, finding.File)}}

	switch {
	case finding.Span.Line == 0:
	case source == "":
		place.Region = &region{
			StartLine: finding.Span.Line, StartColumn: 1,
			EndLine: finding.Span.Line, EndColumn: 1,
		}

	default:
		startLine, startColumn := position(source, finding.Span.Start)
		endLine, endColumn := position(source, finding.Span.End)

		place.Region = &region{
			StartLine: startLine, StartColumn: startColumn,
			EndLine: endLine, EndColumn: endColumn,
		}
	}

	return location{PhysicalLocation: place}
}

// uriOf names the file as the repository holds it.
func uriOf(root, file string) string {
	if root == "" {
		return file
	}

	return path.Join(filepath.ToSlash(filepath.Clean(root)), file)
}

// position converts a byte offset into SARIF's 1-based line and column.
func position(source string, offset int) (line, column int) {
	if offset > len(source) {
		offset = len(source)
	}

	start := strings.LastIndexByte(source[:offset], '\n') + 1

	return 1 + strings.Count(source[:offset], "\n"), 1 + offset - start
}
