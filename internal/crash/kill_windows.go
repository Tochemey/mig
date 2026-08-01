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

//go:build windows

package crash

import "os"

// kill ends this process at once.
//
// Windows has no SIGKILL. [os.Process.Kill] is TerminateProcess, which stops
// every thread where it stands: no deferred function runs, no buffered write
// lands, and no connection is closed gracefully, which is what the fault
// injection needs.
//
// The one thing this must not do is return. TerminateProcess terminates the
// calling thread, so it does not, but it is documented as asynchronous where
// the signal is not, and a caller that was told to die must not carry on to
// the next statement whatever the kernel does. os.Exit closes the gap by the
// same means and to the same effect: it runs no deferred function and flushes
// nothing either, and its code matches the one TerminateProcess leaves.
func kill() {
	if self, err := os.FindProcess(os.Getpid()); err == nil {
		_ = self.Kill()
	}

	os.Exit(1)
}
