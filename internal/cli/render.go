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

package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/tochemey/mig/internal/step"
)

// spinnerFrames animates a step that is still running.
var spinnerFrames = []rune("⣾⣽⣻⢿⡿⣟⣯⣷")

// spinnerInterval paces the animation.
const spinnerInterval = 120 * time.Millisecond

// renderer turns the executor's log records into the run a person watches:
//
//	[2/3] index_email   found invalid index, dropping and rebuilding   ✓ 6m01s
//
// It is a [slog.Handler], so the executor stays presentation-free and a
// library caller who passes their own logger gets the same records as JSON.
// Steps the catalog already shows as done render as nothing at all: the
// display shows what this run did, and the summary accounts for the rest.
//
// On a terminal the current step animates in place with its elapsed time.
// Anywhere else, lines are only appended, so a CI log stays readable.
type renderer struct {
	mu      sync.Mutex
	out     io.Writer
	tty     bool
	animate bool

	// The step being drawn, if any.
	active  bool
	header  string
	note    string
	live    string
	started time.Time
	frame   int

	// flushed says the non-terminal path has printed the header already,
	// which happens as soon as there is an annotation worth a line of its own.
	flushed bool

	spinning bool
}

// newRenderer builds the display for out, animating only when out is a
// terminal.
func newRenderer(out io.Writer) *renderer {
	tty := isTerminal(out)

	return &renderer{out: out, tty: tty, animate: tty}
}

// isTerminal reports whether a person is watching the writer.
func isTerminal(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}

	info, err := file.Stat()

	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// Enabled admits the informational records the display is made of.
func (r *renderer) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo
}

// WithAttrs is required by the interface; the executor does not use it.
func (r *renderer) WithAttrs([]slog.Attr) slog.Handler { return r }

// WithGroup is required by the interface; the executor does not use it.
func (r *renderer) WithGroup(string) slog.Handler { return r }

// Handle renders one record.
func (r *renderer) Handle(_ context.Context, rec slog.Record) error {
	attrs := map[string]slog.Value{}

	rec.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Resolve()

		return true
	})

	r.mu.Lock()
	defer r.mu.Unlock()

	switch rec.Message {
	case "step running":
		r.begin(fmt.Sprintf("[%d/%d] %s",
			attrs["position"].Int64(), attrs["total"].Int64(), attrs["step"].String()))

	case "repairing partial step":
		r.annotate(describeRepair(attrs["kind"].String()))

	case "backfill resuming":
		r.annotate("resuming from id=" + grouped(attrs["cursor"].Int64()))

	case "backfill progress":
		r.progress(fmt.Sprintf("id=%s (%s rows)",
			grouped(attrs["cursor"].Int64()), grouped(attrs["rows"].Int64())))

	case "step done":
		r.finish(attrs["status"].String(),
			time.Duration(attrs["duration_ms"].Int64())*time.Millisecond)

	default:
		// Anything else at warning strength is shown; the rest belongs to the
		// JSON logs, not to the display.
		if rec.Level >= slog.LevelWarn {
			r.warn(rec.Message, attrs)
		}
	}

	return nil
}

// describeRepair words a repair by what it does for that kind of step.
func describeRepair(kind string) string {
	if kind == string(step.KindDDLNoTx) {
		return "found invalid index, dropping and rebuilding"
	}

	return "repairing what an earlier attempt left behind"
}

// begin opens a step's line.
func (r *renderer) begin(header string) {
	r.active = true
	r.header = header
	r.note = ""
	r.live = ""
	r.flushed = false
	r.started = time.Now()

	if r.tty {
		r.draw()
		r.spin()
	}
}

// annotate attaches the note that will be part of the step's final line, and
// on an append-only display gives it a line of its own as it happens.
func (r *renderer) annotate(note string) {
	r.note = note

	if r.tty {
		r.draw()

		return
	}

	r.flushHeader()
	fmt.Fprintf(r.out, "      %s\n", note)
}

// progress updates the transient text beside the spinner. It never reaches an
// append-only display: a backfill reports every batch, and a line per batch
// would drown the log it is meant to help.
func (r *renderer) progress(live string) {
	if !r.tty {
		return
	}

	r.live = live
	r.draw()
}

// finish closes the step's line with its outcome.
func (r *renderer) finish(status string, elapsed time.Duration) {
	if !r.active {
		return
	}

	r.active = false

	// A skipped step renders as nothing: the display shows what the run did.
	if status == "skipped" {
		r.clearLine()

		return
	}

	mark := "✓"
	if status == "failed" {
		mark = "✗"
	}

	line := "  " + r.header
	if r.note != "" {
		line += "   " + r.note
	}

	line += "   " + mark + " " + fmtDuration(elapsed)

	if r.tty {
		r.clearLine()
		fmt.Fprintln(r.out, line)

		return
	}

	// The header already has its own line when an annotation forced it out;
	// only the outcome is left to print.
	if r.flushed {
		fmt.Fprintf(r.out, "      %s %s\n", mark, fmtDuration(elapsed))

		return
	}

	fmt.Fprintln(r.out, line)
}

// warn surfaces a record the run should not finish without a person seeing.
func (r *renderer) warn(msg string, attrs map[string]slog.Value) {
	if r.tty && r.active {
		r.clearLine()
	}

	line := "  ! " + msg
	if s, ok := attrs["step"]; ok {
		line += " (" + s.String() + ")"
	}

	fmt.Fprintln(r.out, line)

	if r.tty && r.active {
		r.draw()
	}
}

// flushHeader prints the step's header once, for the append-only display.
func (r *renderer) flushHeader() {
	if r.flushed {
		return
	}

	r.flushed = true
	fmt.Fprintf(r.out, "  %s\n", r.header)
}

// draw repaints the in-place line on a terminal.
func (r *renderer) draw() {
	text := r.note
	if r.live != "" {
		text = r.live
	}

	line := "  " + r.header
	if text != "" {
		line += "   " + text
	}

	frame := spinnerFrames[r.frame%len(spinnerFrames)]

	fmt.Fprintf(r.out, "\r\x1b[2K%s   %c %s", line, frame, fmtDuration(time.Since(r.started)))
}

// clearLine erases the in-place line on a terminal.
func (r *renderer) clearLine() {
	if r.tty {
		fmt.Fprint(r.out, "\r\x1b[2K")
	}
}

// spin starts the animation once. It only ever draws while a step is active,
// so a finished run cannot write over whatever the shell prints next.
func (r *renderer) spin() {
	if r.spinning || !r.animate {
		return
	}

	r.spinning = true

	go func() {
		ticker := time.NewTicker(spinnerInterval)
		defer ticker.Stop()

		for range ticker.C {
			r.mu.Lock()

			if r.active {
				r.frame++
				r.draw()
			}

			r.mu.Unlock()
		}
	}()
}

// fmtDuration renders an elapsed time the way a person reads one: millisecond
// noise below a second, one decimal below ten, and minutes past sixty.
func fmtDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return strconv.FormatInt(d.Milliseconds(), 10) + "ms"

	case d < 10*time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())

	case d < time.Minute:
		return strconv.Itoa(int(d.Round(time.Second).Seconds())) + "s"

	default:
		minutes := int(d.Minutes())
		seconds := int(d.Round(time.Second).Seconds()) % 60

		if seconds == 0 {
			return strconv.Itoa(minutes) + "m"
		}

		return fmt.Sprintf("%dm%02ds", minutes, seconds)
	}
}

// grouped renders 4210000 as 4,210,000, which is how the design's success
// transcript writes a key, and how a person reads one.
func grouped(n int64) string {
	digits := strconv.FormatInt(n, 10)

	sign := ""
	if digits[0] == '-' {
		sign, digits = "-", digits[1:]
	}

	var out []byte

	for i, digit := range []byte(digits) {
		if i > 0 && (len(digits)-i)%3 == 0 {
			out = append(out, ',')
		}

		out = append(out, digit)
	}

	return sign + string(out)
}
