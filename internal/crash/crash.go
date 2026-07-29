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

// Package crash provides named fault injection points for recovery tests.
//
// The points are gated on an environment variable rather than a build tag, so
// the tested binary is the one that ships.
package crash

import (
	"os"
	"syscall"
)

// EnvPoint names the environment variable that arms a crash point.
const EnvPoint = "MIG_CRASH_AT"

// The named crash points.
const (
	BeforeLeaseAcquire = "before_lease_acquire"
	AfterLeaseAcquire  = "after_lease_acquire"
	BeforeLeaseRelease = "before_lease_release"

	// BeforeStepRunning fires with the ledger claiming work that never started.
	BeforeStepRunning = "before_step_running"

	// BeforeApply fires with the step marked running and no DDL issued.
	BeforeApply = "before_apply"

	// AfterApply fires with the DDL committed and the ledger still saying
	// running. No transaction can close this window, which is why the catalog
	// rather than the ledger decides whether work remains.
	AfterApply = "after_apply"

	// DuringRepair fires part-way through clearing a partial state, since the
	// recovery path is code too and can itself be killed.
	DuringRepair = "during_repair"
)

// At terminates the process when point is the armed crash point, and does
// nothing otherwise.
//
// It uses SIGKILL rather than os.Exit so that no deferred function runs, no
// buffered write lands, and no connection is closed gracefully.
func At(point string) {
	armed := os.Getenv(EnvPoint)
	if armed == "" || armed != point {
		return
	}

	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
}
