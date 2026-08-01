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

package report_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tochemey/mig/internal/lint/report"
	"github.com/tochemey/mig/internal/lint/rules"
)

// log is as much of a SARIF document as the assertions read.
type log struct {
	Schema string `json:"$schema"`

	Version string `json:"version"`

	Runs []struct {
		Tool struct {
			Driver struct {
				Name    string `json:"name"`
				Version string `json:"version"`

				Rules []struct {
					ID               string `json:"id"`
					ShortDescription struct {
						Text string `json:"text"`
					} `json:"shortDescription"`
				} `json:"rules"`
			} `json:"driver"`
		} `json:"tool"`

		Results []struct {
			RuleID  string `json:"ruleId"`
			Level   string `json:"level"`
			Message struct {
				Text string `json:"text"`
			} `json:"message"`

			Locations []struct {
				PhysicalLocation struct {
					ArtifactLocation struct {
						URI string `json:"uri"`
					} `json:"artifactLocation"`

					Region *struct {
						StartLine   int `json:"startLine"`
						StartColumn int `json:"startColumn"`
						EndLine     int `json:"endLine"`
						EndColumn   int `json:"endColumn"`
					} `json:"region"`
				} `json:"physicalLocation"`
			} `json:"locations"`
		} `json:"results"`
	} `json:"runs"`
}

// sarifOf renders and decodes, which is the only reading of the output that
// proves a consumer can make one.
func sarifOf(t *testing.T, findings []rules.Finding, sources map[string]string, root string) log {
	t.Helper()

	var out strings.Builder

	if err := report.SARIF(&out, findings, sources, root, "0.1.0"); err != nil {
		t.Fatalf("render: %v", err)
	}

	var decoded log

	if err := json.Unmarshal([]byte(out.String()), &decoded); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out.String())
	}

	if len(decoded.Runs) != 1 {
		t.Fatalf("rendered %d runs, want 1", len(decoded.Runs))
	}

	return decoded
}

func TestSARIFRendersOneRunWithTheRulesThatFired(t *testing.T) {
	source := "-- +mig step: one\nCREATE INDEX i ON t (c);\n"

	findings := []rules.Finding{{
		RuleID:   rules.L001,
		Severity: rules.SeverityWarn,
		Message:  "this build locks out writes",
		Detail:   "SHARE on t, held for an index build; blocks writes",
		Estimate: "estimated 2m to 6m",
		File:     "1_index.sql",
		Span:     rules.Span{Start: 18, End: 41, Line: 2},
	}}

	decoded := sarifOf(t, findings, map[string]string{"1_index.sql": source}, "migrations")

	if decoded.Version != "2.1.0" || !strings.Contains(decoded.Schema, "sarif") {
		t.Errorf("document = %+v", decoded)
	}

	driver := decoded.Runs[0].Tool.Driver
	if driver.Name != "mig lint" || driver.Version != "0.1.0" {
		t.Errorf("driver = %+v", driver)
	}

	if len(driver.Rules) != 1 || driver.Rules[0].ID != rules.L001 ||
		driver.Rules[0].ShortDescription.Text != rules.Describe(rules.L001) {
		t.Errorf("rules = %+v", driver.Rules)
	}

	results := decoded.Runs[0].Results
	if len(results) != 1 {
		t.Fatalf("rendered %d results, want 1", len(results))
	}

	if results[0].Level != "warning" {
		t.Errorf("level = %q, want warning", results[0].Level)
	}

	// The alert carries the diagnosis, not only the headline: the lock and
	// the estimate are the two things a reviewer decides on.
	for _, want := range []string{"locks out writes", "SHARE on t", "estimated 2m"} {
		if !strings.Contains(results[0].Message.Text, want) {
			t.Errorf("message lacks %q:\n%s", want, results[0].Message.Text)
		}
	}

	place := results[0].Locations[0].PhysicalLocation

	// A finding is named as the repository holds it, or the annotation lands
	// on no line of the pull request at all.
	if place.ArtifactLocation.URI != "migrations/1_index.sql" {
		t.Errorf("uri = %q", place.ArtifactLocation.URI)
	}

	if place.Region == nil {
		t.Fatal("a located finding came back without a region")
	}

	want := struct{ startLine, startColumn, endLine, endColumn int }{2, 1, 2, 24}
	if place.Region.StartLine != want.startLine || place.Region.StartColumn != want.startColumn ||
		place.Region.EndLine != want.endLine || place.Region.EndColumn != want.endColumn {
		t.Errorf("region = %+v, want %+v", *place.Region, want)
	}
}

// TestSARIFSortsAndDeduplicatesTheRuleDescriptors pins that the rules array
// names each rule once, whatever order the findings arrive in.
func TestSARIFSortsAndDeduplicatesTheRuleDescriptors(t *testing.T) {
	findings := []rules.Finding{
		{RuleID: rules.L010, Severity: rules.SeverityError, File: "1_m.sql"},
		{RuleID: rules.L001, Severity: rules.SeverityWarn, File: "1_m.sql"},
		{RuleID: rules.L010, Severity: rules.SeverityError, File: "2_m.sql"},
	}

	driver := sarifOf(t, findings, nil, "").Runs[0].Tool.Driver

	if len(driver.Rules) != 2 || driver.Rules[0].ID != rules.L001 || driver.Rules[1].ID != rules.L010 {
		t.Errorf("rules = %+v", driver.Rules)
	}
}

// TestSARIFPlacesWhatItCan covers the two findings a region cannot be built
// for: one the engine could not locate, and one whose source is not to hand.
func TestSARIFPlacesWhatItCan(t *testing.T) {
	findings := []rules.Finding{
		{RuleID: rules.L040, Severity: rules.SeverityWarn, File: "1_m.sql"},
		{RuleID: rules.L010, Severity: rules.SeverityError, File: "2_m.sql", Span: rules.Span{Line: 4}},
	}

	results := sarifOf(t, findings, nil, ".").Runs[0].Results

	if results[0].Locations[0].PhysicalLocation.Region != nil {
		t.Error("an unlocated finding was given a region")
	}

	// A root of "." is the directory the command was run in, and must not
	// grow a "./" the repository does not use.
	if uri := results[0].Locations[0].PhysicalLocation.ArtifactLocation.URI; uri != "1_m.sql" {
		t.Errorf("uri = %q, want the bare file", uri)
	}

	region := results[1].Locations[0].PhysicalLocation.Region
	if region == nil || region.StartLine != 4 || region.StartColumn != 1 {
		t.Errorf("region = %+v, want the line the engine found", region)
	}
}

// TestSARIFClampsASpanPastTheEndOfItsSource covers the mismatch a caller
// could hand in: the renderer reports the end of the file rather than
// indexing past it.
func TestSARIFClampsASpanPastTheEndOfItsSource(t *testing.T) {
	findings := []rules.Finding{{
		RuleID: rules.L010, Severity: rules.SeverityError,
		File: "1_m.sql", Span: rules.Span{Start: 0, End: 500, Line: 1},
	}}

	region := sarifOf(t, findings, map[string]string{"1_m.sql": "VACUUM FULL t;\n"}, "").
		Runs[0].Results[0].Locations[0].PhysicalLocation.Region

	if region == nil || region.EndLine != 2 || region.EndColumn != 1 {
		t.Errorf("region = %+v, want the end of the file", region)
	}
}

func TestSARIFReportsItsSink(t *testing.T) {
	if err := report.SARIF(failWriter{}, nil, nil, "", ""); err == nil {
		t.Error("a failed write went unreported")
	}
}
