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

package lease

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"
)

// renewQuery extends the lease, and only for the runner that still holds it.
// Matching no row means the lease was taken and retrying cannot recover it.
//
// Ownership is read rather than written. A reader never queues behind the row
// lock a ledger write holds, so renewal keeps running while a batch that may
// last minutes is in flight.
const renewQuery = `
UPDATE mig.lease_expiry
   SET expires_at   = now() + make_interval(secs => $3),
       heartbeat_at = now()
 WHERE id = 1
   AND EXISTS (SELECT 1 FROM mig.lease WHERE id = 1 AND owner = $1 AND fence = $2)
RETURNING id`

// Keepalive renews the lease in the background and returns a context that is
// cancelled once the lease is no longer safely held, plus a function that
// stops renewing.
//
// The executor runs under the returned context, so losing the lease aborts the
// work instead of merely logging it. [context.Cause] reports [ErrLost].
//
// The holder gives up while validity remains, never after expiry, because a
// runner working past its expiry races whoever took over from it.
//
// Call it at most once per lease. The returned stop function is idempotent.
func (l *Lease) Keepalive(ctx context.Context) (context.Context, func()) {
	renewCtx, cancel := context.WithCancelCause(ctx)
	stop := make(chan struct{})
	stopped := make(chan struct{})

	go func() {
		defer close(stopped)

		l.renew(renewCtx, cancel, stop)
	}()

	var once sync.Once

	return renewCtx, func() {
		once.Do(func() {
			close(stop)
			<-stopped
			cancel(nil)
		})
	}
}

// renew is the heartbeat loop.
func (l *Lease) renew(ctx context.Context, cancel context.CancelCauseFunc, stop <-chan struct{}) {
	interval := l.ttl / renewDivisor

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-ticker.C:
		}

		err := l.renewOnce(ctx)
		if err == nil {
			l.renewedAt = time.Now()
			continue
		}

		// Taken by someone else. The fence is stale from this instant, so there
		// is nothing to retry.
		if errors.Is(err, ErrLost) {
			cancel(err)
			return
		}

		// A transient error is tolerable only while enough validity remains to
		// recover in. time.Since reads the monotonic clock, so a wall-clock
		// adjustment cannot make this deadline arrive late.
		if time.Since(l.renewedAt) >= l.ttl-interval {
			cancel(fmt.Errorf("%w: %w", ErrLost, err))
			return
		}
	}
}

// renewOnce extends the lease by one TTL, reporting [ErrLost] when the lease
// has been taken.
func (l *Lease) renewOnce(ctx context.Context) error {
	var id int

	err := l.db.QueryRowContext(ctx, renewQuery, l.fence.Owner, l.fence.Token, l.ttl.Seconds()).Scan(&id)

	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: fence %s no longer holds it", ErrLost, l.fence)
	}

	if err != nil {
		return fmt.Errorf("renew lease %s: %w", l.fence, err)
	}

	return nil
}
