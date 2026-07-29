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

// Package lease provides mutual exclusion between runners.
//
// An advisory lock dies with its session, so it cannot protect work that
// outlives a connection and cannot be handed to whoever resumes that work. A
// lease is a row with an expiry, so it survives a reconnect and can be taken
// over once it lapses.
//
// Takeover is safe because every acquisition increments a monotonic fence
// token that guards every ledger write. A runner frozen past its expiry finds
// its writes rejected rather than overwriting its successor's.
package lease

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/tochemey/mig/internal/ledger"
)

const (
	// DefaultTTL is how long a lease stays valid without a heartbeat.
	DefaultTTL = 30 * time.Second

	// DefaultWaitTimeout bounds how long an acquisition waits for a lease that
	// somebody else holds.
	DefaultWaitTimeout = 5 * time.Minute

	// renewDivisor sets the heartbeat interval to TTL/3.
	renewDivisor = 3

	// waitPollInterval is how often a waiting acquisition retries.
	waitPollInterval = 250 * time.Millisecond
)

var (
	// ErrNoOwner reports an acquisition attempted without an owner identifier.
	ErrNoOwner = errors.New("lease owner is empty")

	// ErrLocked reports that another runner holds the lease.
	ErrLocked = errors.New("lease held by another runner")

	// ErrLost reports that the lease is no longer safely held, and that the
	// holder must stop working.
	ErrLost = errors.New("lease lost")
)

// OnLocked is what an acquisition does when the lease is already held.
type OnLocked string

const (
	// Wait blocks until the lease frees up or the wait times out.
	Wait OnLocked = "wait"

	// Fail returns [ErrLocked] straight away.
	Fail OnLocked = "fail"
)

// Config parameterises [Acquire].
type Config struct {
	// Owner identifies this runner. Use [NewOwner] unless a test needs a
	// deterministic value.
	Owner string

	// TTL is how long the lease stays valid without a heartbeat. Zero means
	// [DefaultTTL].
	TTL time.Duration

	// OnLocked selects the behaviour when another runner holds the lease. Empty
	// means [Wait].
	OnLocked OnLocked

	// WaitTimeout bounds waiting. Zero means [DefaultWaitTimeout].
	WaitTimeout time.Duration
}

// withDefaults fills in the zero values.
func (c Config) withDefaults() Config {
	if c.TTL <= 0 {
		c.TTL = DefaultTTL
	}

	if c.OnLocked == "" {
		c.OnLocked = Wait
	}

	if c.WaitTimeout <= 0 {
		c.WaitTimeout = DefaultWaitTimeout
	}

	return c
}

// Lease is a held lease. Its fence must guard every ledger write made while it
// is held.
type Lease struct {
	db    *sql.DB
	fence ledger.Fence
	ttl   time.Duration

	// renewedAt is the local time of the last successful renewal, touched only
	// by the keepalive goroutine.
	renewedAt time.Time
}

// acquireQuery takes the lease when it is free or expired, and bumps the
// fence. Expiry is judged by the server's clock so that runners whose own
// clocks disagree still agree on whether a lease has lapsed.
const acquireQuery = `
UPDATE mig.lease
   SET owner        = $1,
       fence        = fence + 1,
       expires_at   = now() + make_interval(secs => $2),
       heartbeat_at = now()
 WHERE id = 1
   AND (owner IS NULL OR expires_at < now())
RETURNING fence`

// NewOwner builds an owner identifier from host, process and a random suffix.
// The random suffix keeps two processes that share a pid in different
// containers from looking like the same runner.
func NewOwner() string {
	host, _ := os.Hostname()

	return fmt.Sprintf("%s/%d/%s", host, os.Getpid(), rand.Text())
}

// Acquire takes the lease, waiting or failing per cfg when another runner holds
// it. The ledger schema must already exist.
func Acquire(ctx context.Context, db *sql.DB, cfg Config) (*Lease, error) {
	// Fencing stays sound with an empty owner, since the token alone
	// distinguishes holders, but the ledger can no longer say who holds it.
	if cfg.Owner == "" {
		return nil, ErrNoOwner
	}

	cfg = cfg.withDefaults()
	deadline := time.Now().Add(cfg.WaitTimeout)

	ticker := time.NewTicker(waitPollInterval)
	defer ticker.Stop()

	for {
		fence, ok, err := tryAcquire(ctx, db, cfg)
		if err != nil {
			return nil, err
		}

		if ok {
			return &Lease{
				db:        db,
				fence:     ledger.Fence{Owner: cfg.Owner, Token: fence},
				ttl:       cfg.TTL,
				renewedAt: time.Now(),
			}, nil
		}

		if cfg.OnLocked == Fail {
			return nil, ErrLocked
		}

		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("%w: still held after %s", ErrLocked, cfg.WaitTimeout)
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for lease: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// tryAcquire makes one attempt, reporting whether the lease was taken.
func tryAcquire(ctx context.Context, db *sql.DB, cfg Config) (int64, bool, error) {
	var fence int64

	err := db.QueryRowContext(ctx, acquireQuery, cfg.Owner, cfg.TTL.Seconds()).Scan(&fence)

	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}

	if err != nil {
		return 0, false, fmt.Errorf("acquire lease: %w", err)
	}

	return fence, true, nil
}

// Fence returns the token that guards this holder's ledger writes.
func (l *Lease) Fence() ledger.Fence {
	return l.fence
}

// TTL returns the lease's validity window.
func (l *Lease) TTL() time.Duration {
	return l.ttl
}

// releaseQuery returns a row only when the caller still holds the lease, which
// enforces the fence without a second round trip.
const releaseQuery = `
UPDATE mig.lease
   SET owner = NULL, expires_at = NULL, heartbeat_at = NULL
 WHERE id = 1 AND owner = $1 AND fence = $2
RETURNING fence`

// Release hands the lease back so the next runner need not wait out the TTL.
//
// It is itself fenced. A superseded runner releasing a lease it no longer
// holds would hand a third runner a lease the real holder is still using;
// releasing what the caller no longer owns returns [ledger.ErrFenced].
func (l *Lease) Release(ctx context.Context) error {
	var fence int64

	err := l.db.QueryRowContext(ctx, releaseQuery, l.fence.Owner, l.fence.Token).Scan(&fence)

	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("release lease %s: %w", l.fence, ledger.ErrFenced)
	}

	if err != nil {
		return fmt.Errorf("release lease %s: %w", l.fence, err)
	}

	return nil
}
