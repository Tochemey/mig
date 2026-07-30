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

package kill_test

import (
	"math/rand/v2"
	"os"
	"strconv"
	"testing"
	"time"
)

// The named cells kill at points somebody thought of. This kills at a moment
// nobody chose, which is the only way to reach the ones nobody thought of.
//
// Randomness that cannot be replayed is worth little: a failure has to be
// reproducible before it can be fixed. Every iteration derives its delay from a
// seed the failure message prints, and re-running with that seed reproduces it
// exactly.

const (
	// EnvFuzzSeed replays a specific run.
	EnvFuzzSeed = "MIG_FUZZ_SEED"

	// EnvFuzzIterations sets how many kills to make. The nightly job raises it;
	// the default keeps the case affordable on every push.
	EnvFuzzIterations = "MIG_FUZZ_ITERATIONS"

	defaultIterations = 3

	// killWindow bounds when the kill lands. The lower bound is past process
	// start, and the upper bound is inside a build that has been pinned open,
	// so every iteration interrupts real work.
	killWindowLow  = 50 * time.Millisecond
	killWindowHigh = 400 * time.Millisecond
)

// TestFuzzKillTimingConverges kills a run at a random moment and requires the
// database to converge afterwards, exactly as the named cells do.
func TestFuzzKillTimingConverges(t *testing.T) {
	seed := fuzzSeed(t)
	iterations := fuzzIterations(t)

	t.Logf("fuzzing %d iteration(s) from seed %d; replay with %s=%d",
		iterations, seed, EnvFuzzSeed, seed)

	for i := range iterations {
		// Each iteration gets its own stream, so iteration 7 of a run is the
		// same kill whatever happened in the six before it.
		//
		//nolint:gosec // G404: a replayable seed is the requirement; crypto/rand cannot be one.
		source := rand.New(rand.NewPCG(seed, uint64(i)))
		delay := killWindowLow + time.Duration(
			source.Int64N(int64(killWindowHigh-killWindowLow)))

		t.Run(strconv.Itoa(i), func(t *testing.T) {
			// Reported up front: a run that hangs rather than fails still says
			// what to replay.
			t.Logf("seed=%d iteration=%d delay=%s", seed, i, delay)

			fuzzOnce(t, delay)
		})
	}
}

// fuzzOnce runs one kill-and-recover cycle with the kill at delay.
func fuzzOnce(t *testing.T, delay time.Duration) {
	t.Helper()

	r := newRun(t, indexFixture)

	// The build is pinned open, so a kill anywhere in the window lands during
	// it rather than after it. Without this the window would have to be tuned
	// to the machine, and the case would be flaky rather than random.
	release := r.blockIndexBuild()

	proc := r.start("")

	time.Sleep(delay)

	if err := proc.Kill(); err != nil {
		t.Fatalf("kill mig: %v", err)
	}

	if _, err := proc.Wait(30 * time.Second); err != nil {
		t.Fatalf("wait for killed mig: %v", err)
	}

	// Released before draining, since the blocker is itself a backend.
	release()
	r.drain()

	// The assertion bundle every recovery test ends with. A kill at any moment
	// has to leave a database that one further run finishes, matching a run
	// that was never interrupted, with nothing left for the run after that.
	r.converge()
	r.assertMatchesGolden()
	r.assertSecondRunDoesNothing()
}

// fuzzSeed reads the replay seed, or picks one and reports it.
func fuzzSeed(t *testing.T) uint64 {
	t.Helper()

	value := os.Getenv(EnvFuzzSeed)
	if value == "" {
		// Not a fixed default: a seed that never changes only ever explores the
		// same timings, which is the opposite of the point.
		//
		//nolint:gosec // G404: this picks a test seed, not a secret.
		return rand.Uint64()
	}

	seed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		t.Fatalf("%s=%q is not a number", EnvFuzzSeed, value)
	}

	return seed
}

// fuzzIterations reads how many kills to make.
func fuzzIterations(t *testing.T) int {
	t.Helper()

	value := os.Getenv(EnvFuzzIterations)
	if value == "" {
		return defaultIterations
	}

	iterations, err := strconv.Atoi(value)
	if err != nil || iterations < 1 {
		t.Fatalf("%s=%q is not a positive number", EnvFuzzIterations, value)
	}

	return iterations
}
