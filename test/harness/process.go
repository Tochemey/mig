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

package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// outputPollInterval is how often [Process.WaitOutput] re-reads the captured
// streams while waiting for a marker.
const outputPollInterval = 10 * time.Millisecond

// coverPkg is the pattern whose coverage is recorded in spawned binaries.
const coverPkg = "github.com/tochemey/mig/..."

// coverDir is where spawned binaries write their coverage counters. [Main]
// sets it before any test runs, and nothing writes to it afterwards.
var coverDir string

// Build compiles the main package at importPath into dir and returns the path
// to the executable.
//
// When the parent runs under coverage the child is built instrumented, since
// the migrator only ever executes in a child process.
func Build(ctx context.Context, dir, importPath string) (string, error) {
	bin := filepath.Join(dir, path.Base(importPath))

	args := []string{"build", "-o", bin}
	if coverDir != "" {
		args = append(args, "-cover", "-coverpkg="+coverPkg)
	}

	//nolint:gosec // G204: the import path comes from the test, not from input.
	cmd := exec.CommandContext(ctx, "go", append(args, importPath)...)

	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build %q: %w: %s", importPath, err, out)
	}

	return bin, nil
}

// Process is a spawned child under the harness's control.
//
// The migrator under test is a real OS process because deferred functions,
// connection cleanup and sql.DB finalizers all misrepresent what a SIGKILL
// does in production.
type Process struct {
	cmd    *exec.Cmd
	stdout *syncBuffer
	stderr *syncBuffer
	done   chan struct{}
	code   int
	waited error
}

// Start spawns bin with args and the additional environment entries in env,
// which are appended to the parent's environment as "KEY=VALUE" strings.
//
// The child gets its own process group, so signals reach any descendants and a
// killed child leaves no orphans.
func Start(bin string, args, env []string) (*Process, error) {
	//nolint:gosec // G204: the binary and arguments come from the test, not from input.
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Set on the child only. The parent's coverage is written through
	// -coverprofile, and the two mechanisms interfere.
	if coverDir != "" {
		cmd.Env = append(cmd.Env, "GOCOVERDIR="+coverDir)
	}

	p := &Process{
		cmd:    cmd,
		stdout: &syncBuffer{},
		stderr: &syncBuffer{},
		done:   make(chan struct{}),
	}

	cmd.Stdout = p.stdout
	cmd.Stderr = p.stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %q: %w", bin, err)
	}

	go func() {
		defer close(p.done)

		err := cmd.Wait()

		p.code = cmd.ProcessState.ExitCode()

		// A non-zero exit or a fatal signal is an expected outcome, not a
		// harness failure.
		var exitErr *exec.ExitError
		if err != nil && !errors.As(err, &exitErr) {
			p.waited = err
		}
	}()

	return p, nil
}

// PID returns the child's process identifier, which is also its process group.
func (p *Process) PID() int {
	return p.cmd.Process.Pid
}

// Signal delivers sig to the child's whole process group.
func (p *Process) Signal(sig syscall.Signal) error {
	if err := syscall.Kill(-p.PID(), sig); err != nil {
		return fmt.Errorf("signal %s to process group %d: %w", sig, p.PID(), err)
	}

	return nil
}

// Kill terminates the child with SIGKILL: no deferred functions, no flushed
// buffers, no graceful connection teardown.
func (p *Process) Kill() error {
	return p.Signal(syscall.SIGKILL)
}

// Freeze suspends the child with SIGSTOP, standing in for a stop-the-world GC
// pause or a stalled VM. Pair it with [Process.Thaw] to drive a runner that
// wakes after its lease has been taken.
func (p *Process) Freeze() error {
	return p.Signal(syscall.SIGSTOP)
}

// Thaw resumes a frozen child with SIGCONT.
func (p *Process) Thaw() error {
	return p.Signal(syscall.SIGCONT)
}

// Exited reports whether the child has terminated.
func (p *Process) Exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// Wait blocks until the child exits and returns its exit code, which is -1 when
// it died from a signal. Exceeding timeout is an error; a non-zero exit is not.
func (p *Process) Wait(timeout time.Duration) (int, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-p.done:
		return p.code, p.waited

	case <-timer.C:
		return 0, fmt.Errorf("process %d still running after %s", p.PID(), timeout)
	}
}

// WaitOutput blocks until marker appears on the child's stdout or stderr, so a
// test can synchronise on the child reaching a known state rather than sleep.
func (p *Process) WaitOutput(marker string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	ticker := time.NewTicker(outputPollInterval)
	defer ticker.Stop()

	for {
		stdout, stderr := p.stdout.String(), p.stderr.String()

		if strings.Contains(stdout, marker) || strings.Contains(stderr, marker) {
			return nil
		}

		if p.Exited() {
			return fmt.Errorf("process %d exited before printing %q\nstdout: %s\nstderr: %s",
				p.PID(), marker, stdout, stderr)
		}

		if !time.Now().Before(deadline) {
			return fmt.Errorf("process %d did not print %q within %s\nstdout: %s\nstderr: %s",
				p.PID(), marker, timeout, stdout, stderr)
		}

		<-ticker.C
	}
}

// Stdout returns everything the child has written to stdout so far.
func (p *Process) Stdout() string {
	return p.stdout.String()
}

// Stderr returns everything the child has written to stderr so far.
func (p *Process) Stderr() string {
	return p.stderr.String()
}

// Cleanup kills a child that is still running and waits for it. Register it
// with t.Cleanup so a failing test cannot leak a process holding a lease.
func (p *Process) Cleanup() {
	if p.Exited() {
		return
	}

	// SIGKILL terminates a child even while it is stopped, so a frozen process
	// needs no SIGCONT first.
	_ = p.Kill()

	<-p.done
}

// syncBuffer collects a child's output. os/exec writes from its own goroutine
// while tests read concurrently.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write appends to the buffer.
func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

// String returns everything written so far.
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}
