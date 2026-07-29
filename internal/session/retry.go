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

package session

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// lockNotAvailable is the SQLSTATE Postgres raises when lock_timeout expires.
const lockNotAvailable = "55P03"

// Retry bounds how often a step waits for a lock it could not get.
type Retry struct {
	// Attempts is the total number of tries, including the first.
	Attempts int

	// Base is the delay after the first failure; it doubles thereafter.
	Base time.Duration

	// Jitter is the fraction by which each delay is randomised, spreading out
	// runners that started together.
	Jitter float64
}

// DefaultRetry backs off from one second, doubling, up to ten tries.
func DefaultRetry() Retry {
	return Retry{Attempts: 10, Base: time.Second, Jitter: 0.2}
}

// WithLockRetry runs work, retrying while Postgres reports it could not take a
// lock in time.
//
// Only lock acquisition is retried. Every other failure is returned at once,
// because retrying a statement that failed on its own terms just fails again
// more slowly.
func WithLockRetry(ctx context.Context, retry Retry, work func(context.Context) error) error {
	delay := retry.Base

	for attempt := 1; ; attempt++ {
		err := work(ctx)
		if err == nil || !isLockNotAvailable(err) {
			return err
		}

		if attempt >= retry.Attempts {
			return fmt.Errorf("gave up waiting for a lock after %d attempts: %w", attempt, err)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for a lock: %w", ctx.Err())
		case <-time.After(jittered(delay, retry.Jitter)):
		}

		delay *= 2
	}
}

// isLockNotAvailable reports whether err is a lock timeout.
func isLockNotAvailable(err error) bool {
	var pgErr *pgconn.PgError

	return errors.As(err, &pgErr) && pgErr.Code == lockNotAvailable
}

// jittered spreads a delay by ±fraction.
func jittered(delay time.Duration, fraction float64) time.Duration {
	if fraction <= 0 {
		return delay
	}

	spread := float64(delay) * fraction

	//nolint:gosec // G404: jitter needs spread between runners, not unpredictability.
	offset := (rand.Float64()*2 - 1) * spread

	jittered := time.Duration(float64(delay) + offset)
	if jittered < 0 {
		return 0
	}

	return jittered
}
