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

// The renderer is unexported on purpose, so its tests live inside the package.
package cli

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// display builds a renderer over a buffer and the logger that feeds it,
// standing in for a run watched through a pipe.
func display() (*bytes.Buffer, *slog.Logger) {
	var buf bytes.Buffer

	return &buf, slog.New(&renderer{out: &buf})
}

// TestRendererShowsAnAppliedStep covers the ordinary line: counter, name,
// mark, duration.
func TestRendererShowsAnAppliedStep(t *testing.T) {
	buf, log := display()

	log.Info("step running", "position", 1, "total", 3, "step", "add_column")
	log.Info("step done", "position", 1, "total", 3, "step", "add_column",
		"status", "succeeded", "duration_ms", int64(12))

	if got, want := buf.String(), "  [1/3] add_column   ✓ 12ms\n"; got != want {
		t.Fatalf("rendered %q, want %q", got, want)
	}
}

// TestRendererShowsARepair is the line the design names as the definition of
// success: the interrupted build found, named, and cleared.
func TestRendererShowsARepair(t *testing.T) {
	buf, log := display()

	log.Info("step running", "position", 2, "total", 3, "step", "index_email")
	log.Info("repairing partial step", "step", "x/1", "kind", "ddl_notx", "attempts", 1)
	log.Info("step done", "position", 2, "total", 3, "step", "index_email",
		"status", "succeeded", "duration_ms", int64(361_000))

	want := "  [2/3] index_email\n" +
		"      found invalid index, dropping and rebuilding\n" +
		"      ✓ 6m01s\n"

	if got := buf.String(); got != want {
		t.Fatalf("rendered %q, want %q", got, want)
	}
}

// TestRendererShowsABackfillResuming covers the other half of the same
// transcript, including the grouped key.
func TestRendererShowsABackfillResuming(t *testing.T) {
	buf, log := display()

	log.Info("step running", "position", 3, "total", 3, "step", "fill_email")
	log.Info("backfill resuming", "step", "x/2", "cursor", int64(4_210_000), "rows", int64(2_000_000))
	log.Info("backfill progress", "step", "x/2", "cursor", int64(4_215_000), "rows", int64(2_005_000))
	log.Info("step done", "position", 3, "total", 3, "step", "fill_email",
		"status", "succeeded", "duration_ms", int64(22*60*1000))

	want := "  [3/3] fill_email\n" +
		"      resuming from id=4,210,000\n" +
		"      ✓ 22m\n"

	// Per-batch progress must not reach an append-only display: a backfill
	// reports every batch, and a line per batch would drown the log.
	if got := buf.String(); got != want {
		t.Fatalf("rendered %q, want %q", got, want)
	}
}

// TestRendererIsSilentAboutSkippedSteps is the transcript's quietest feature:
// a converged database renders as nothing rather than as a list of steps not
// run.
func TestRendererIsSilentAboutSkippedSteps(t *testing.T) {
	buf, log := display()

	log.Info("step done", "position", 1, "total", 2, "step", "add_column",
		"status", "skipped", "duration_ms", int64(1))

	if got := buf.String(); got != "" {
		t.Fatalf("a skipped step rendered %q", got)
	}
}

// TestRendererMarksAFailure covers the mark a person scans for.
func TestRendererMarksAFailure(t *testing.T) {
	buf, log := display()

	log.Info("step running", "position", 1, "total", 1, "step", "add_column")
	log.Info("step done", "position", 1, "total", 1, "step", "add_column",
		"status", "failed", "duration_ms", int64(90))

	if got := buf.String(); !strings.Contains(got, "✗") {
		t.Fatalf("a failed step rendered %q without a mark", got)
	}
}

// TestRendererSurfacesWarnings covers records like checksum drift, which a run
// must not finish without a person seeing.
func TestRendererSurfacesWarnings(t *testing.T) {
	buf, log := display()

	log.Warn("checksum drift allowed", "step", "x/0", "name", "add_column")

	if got, want := buf.String(), "  ! checksum drift allowed (x/0)\n"; got != want {
		t.Fatalf("rendered %q, want %q", got, want)
	}
}

// TestRendererIgnoresUnknownInfoRecords keeps a future executor record from
// breaking the display: it belongs to the JSON logs until the display learns
// it.
func TestRendererIgnoresUnknownInfoRecords(t *testing.T) {
	buf, log := display()

	log.Info("some future record", "detail", 42)

	if got := buf.String(); got != "" {
		t.Fatalf("an unknown record rendered %q", got)
	}
}

// TestRendererAnimatesOnATerminal covers the in-place line: drawn with a
// spinner while the step runs, erased and replaced by the outcome.
func TestRendererAnimatesOnATerminal(t *testing.T) {
	var buf bytes.Buffer

	// animate stays false so the test drives every frame itself.
	log := slog.New(&renderer{out: &buf, tty: true})

	log.Info("step running", "position", 1, "total", 1, "step", "index_email")

	if got := buf.String(); !strings.Contains(got, "\r\x1b[2K  [1/1] index_email") {
		t.Fatalf("no in-place line was drawn: %q", got)
	}

	if got := buf.String(); !strings.ContainsRune(got, spinnerFrames[0]) {
		t.Fatalf("no spinner frame was drawn: %q", got)
	}

	log.Info("step done", "position", 1, "total", 1, "step", "index_email",
		"status", "succeeded", "duration_ms", int64(500))

	lines := strings.Split(buf.String(), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], "  [1/1] index_email   ✓ 500ms") {
		t.Fatalf("the outcome did not replace the line: %q", buf.String())
	}
}

// TestFmtDuration pins the shapes a person reads.
func TestFmtDuration(t *testing.T) {
	cases := map[time.Duration]string{
		42 * time.Millisecond:                 "42ms",
		1200 * time.Millisecond:               "1.2s",
		42 * time.Second:                      "42s",
		6*time.Minute + time.Second:           "6m01s",
		22 * time.Minute:                      "22m",
		time.Hour + 5*time.Minute:             "65m",
		59*time.Second + 800*time.Millisecond: "60s",
	}

	for d, want := range cases {
		if got := fmtDuration(d); got != want {
			t.Fatalf("fmtDuration(%s) = %q, want %q", d, got, want)
		}
	}
}

// TestGrouped pins the digit grouping.
func TestGrouped(t *testing.T) {
	cases := map[int64]string{
		0:         "0",
		999:       "999",
		1000:      "1,000",
		4_210_000: "4,210,000",
		-1234:     "-1,234",
	}

	for n, want := range cases {
		if got := grouped(n); got != want {
			t.Fatalf("grouped(%d) = %q, want %q", n, got, want)
		}
	}
}

// TestRendererAnimationTicksOnItsOwn covers the goroutine behind the spinner:
// frames advance without records arriving, and stop advancing the moment the
// step is over.
func TestRendererAnimationTicksOnItsOwn(t *testing.T) {
	var buf bytes.Buffer

	r := &renderer{out: &buf, tty: true, animate: true}
	log := slog.New(r)

	log.Info("step running", "position", 1, "total", 1, "step", "index_email")
	time.Sleep(4 * spinnerInterval)
	log.Info("step done", "position", 1, "total", 1, "step", "index_email",
		"status", "succeeded", "duration_ms", int64(500))

	r.mu.Lock()
	frames := r.frame
	r.mu.Unlock()

	if frames == 0 {
		t.Fatal("the spinner never advanced on its own")
	}

	if !strings.HasSuffix(buf.String(), "✓ 500ms\n") {
		t.Fatalf("the outcome line did not land: %q", buf.String())
	}
}

// TestRendererRedrawsAroundAWarning covers a warning arriving while a step is
// on screen: the line is cleared, the warning printed, the line redrawn.
func TestRendererRedrawsAroundAWarning(t *testing.T) {
	var buf bytes.Buffer

	log := slog.New(&renderer{out: &buf, tty: true})

	log.Info("step running", "position", 1, "total", 1, "step", "add_email")
	log.Warn("checksum drift allowed", "step", "x/0")

	if got := buf.String(); !strings.Contains(got, "  ! checksum drift allowed (x/0)\n") {
		t.Fatalf("the warning did not land: %q", got)
	}

	if !strings.HasSuffix(buf.String(), fmtLastDraw(&buf)) {
		t.Fatalf("the step line was not redrawn after the warning: %q", buf.String())
	}
}

// fmtLastDraw returns the text after the last carriage return, which is what
// the terminal is left showing.
func fmtLastDraw(buf *bytes.Buffer) string {
	s := buf.String()

	return s[strings.LastIndex(s, "\r"):]
}

// TestRendererShowsProgressOnATerminal covers the live text a backfill puts
// beside the spinner, and the generic repair wording for a kind with no
// special case.
func TestRendererShowsProgressOnATerminal(t *testing.T) {
	var buf bytes.Buffer

	log := slog.New(&renderer{out: &buf, tty: true})

	log.Info("step running", "position", 1, "total", 1, "step", "fill_email")
	log.Info("repairing partial step", "step", "x/0", "kind", "backfill", "attempts", 2)
	log.Info("backfill progress", "step", "x/0", "cursor", int64(2_000), "rows", int64(1_000))

	if got := buf.String(); !strings.Contains(got, "id=2,000 (1,000 rows)") {
		t.Fatalf("the live progress was not drawn: %q", got)
	}

	if got := buf.String(); !strings.Contains(got, "repairing what an earlier attempt left behind") {
		t.Fatalf("the generic repair wording was not drawn: %q", got)
	}
}

// TestRendererHandlerPassthroughs covers the two interface methods the
// executor never exercises, which must hand back a working handler.
func TestRendererHandlerPassthroughs(t *testing.T) {
	buf, _ := display()

	r := &renderer{out: buf}

	if r.WithAttrs(nil) != slog.Handler(r) || r.WithGroup("g") != slog.Handler(r) {
		t.Fatal("the passthroughs did not return the renderer")
	}
}
