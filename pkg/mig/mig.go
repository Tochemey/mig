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

// Package mig applies and checks Postgres migrations that can be killed and
// re-run.
//
// Everything the command line does is here, so a program can migrate without
// shelling out to a binary. The two halves are meant to be used from different
// places. Applying is a job: one runner, holding a lease, converging the
// database. Checking is what the service does as it starts, and it must never
// apply anything:
//
//	if err := mig.Verify(ctx, db, migrations); err != nil {
//	    log.Fatal("migrations pending; refusing to start", "err", err)
//	}
//
// Migrating on boot from the service binary is the shape to avoid. Every
// replica races, none of them is elected to do it, and a migration that takes
// minutes collides with the readiness probe. Apply as a job, verify at startup.
package mig
