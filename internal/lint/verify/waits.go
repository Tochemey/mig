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

package verify

import (
	"context"
	"database/sql"
	"sort"
	"sync"
	"time"
)

// sampleEvery paces the sampler. Fifty milliseconds is fine enough to catch a
// wait worth reporting and coarse enough that the sampling is not part of the
// load being measured.
const sampleEvery = 50 * time.Millisecond

// waitQuery reads what the other backends on this database are doing.
//
// The lock join is a left join on purpose: a backend waiting on a relation
// lock is the case this exists for, and one waiting on anything else still
// counts towards the attribution.
const waitQuery = `
SELECT coalesce(a.wait_event_type, ''), coalesce(a.wait_event, ''),
       coalesce(l.relation::regclass::text, ''),
       extract(epoch FROM now() - a.query_start)
  FROM pg_stat_activity a
  LEFT JOIN pg_locks l ON l.pid = a.pid AND NOT l.granted AND l.locktype = 'relation'
 WHERE a.datname = current_database()
   AND a.pid <> pg_backend_pid()
   AND a.state = 'active'`

// Wait is one thing the server was seen waiting on, and how often.
type Wait struct {
	// Event is "Lock:relation" or "IO:DataFileRead", as pg_stat_activity
	// spells the pair.
	Event string

	// Samples is how many readings caught a backend on this event, and Share
	// is that as a fraction of every active backend seen, waiting or not.
	Samples int
	Share   float64
}

// Block is the longest a statement was seen waiting on a relation lock.
type Block struct {
	For      time.Duration
	Relation string
}

// Sampler watches what the server is waiting on while the workload runs.
//
// It is the other half of the measurement: the client's latency says queries
// got slower, and this says what they were waiting on.
type Sampler struct {
	db *sql.DB

	mu       sync.Mutex
	events   map[string]int
	samples  int
	blocked  int
	longest  Block
	failures int

	wg   sync.WaitGroup
	stop context.CancelFunc
}

// Watch starts sampling until the returned sampler is stopped.
func Watch(ctx context.Context, db *sql.DB) *Sampler {
	ctx, stop := context.WithCancel(ctx)

	s := &Sampler{db: db, events: map[string]int{}, stop: stop}

	s.wg.Go(func() {
		ticker := time.NewTicker(sampleEvery)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.sample(ctx)
			}
		}
	})

	return s
}

// sample takes one reading.
//
// A reading that fails is counted rather than returned: the sampler runs
// beside the thing being measured, and a migration is not going to be
// abandoned because one poll lost a race with a backend that ended. The count
// reaches the report, so a sampler that saw nothing cannot pass for a quiet
// server.
func (s *Sampler) sample(ctx context.Context) {
	readings, err := s.read(ctx)
	if err != nil {
		s.fail()

		return
	}

	s.fold(readings)
}

// read takes one reading of what the other backends are waiting on.
func (s *Sampler) read(ctx context.Context) ([]reading, error) {
	rows, err := s.db.QueryContext(ctx, waitQuery)
	if err != nil {
		return nil, err
	}

	readings, err := readWaits(rows)

	return readings, firstError(err, rows.Close())
}

// reading is one active backend at one instant.
type reading struct {
	event    string
	relation string
	waiting  time.Duration
}

// readWaits turns one answer to the wait query into readings.
func readWaits(rows *sql.Rows) ([]reading, error) {
	var readings []reading

	for rows.Next() {
		var (
			kind, event, relation string
			seconds               float64
		)

		if err := rows.Scan(&kind, &event, &relation, &seconds); err != nil {
			return nil, err
		}

		read := reading{
			relation: relation,
			waiting:  time.Duration(seconds * float64(time.Second)),
		}

		if kind != "" {
			read.event = kind + ":" + event
		}

		readings = append(readings, read)
	}

	return readings, rows.Err()
}

// fold adds one instant's readings to the totals.
func (s *Sampler) fold(readings []reading) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, read := range readings {
		s.samples++

		if read.event == "" {
			continue
		}

		s.events[read.event]++

		if read.relation == "" {
			continue
		}

		s.blocked++

		if read.waiting > s.longest.For {
			s.longest = Block{For: read.waiting, Relation: read.relation}
		}
	}
}

// fail records a reading that could not be taken.
func (s *Sampler) fail() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.failures++
}

// Stop ends the sampling and reports what was seen: the events by share, how
// many readings found a backend blocked on a relation lock, the longest such
// wait, and how many readings could not be taken at all.
func (s *Sampler) Stop() (waits []Wait, blocked int, longest Block, failures int) {
	s.stop()
	s.wg.Wait()

	s.mu.Lock()
	defer s.mu.Unlock()

	for event, count := range s.events {
		share := 0.0
		if s.samples > 0 {
			share = float64(count) / float64(s.samples)
		}

		waits = append(waits, Wait{Event: event, Samples: count, Share: share})
	}

	// Loudest first, and by name where two are equally loud, so a report of
	// the same run reads the same way twice.
	sort.Slice(waits, func(i, j int) bool {
		if waits[i].Samples != waits[j].Samples {
			return waits[i].Samples > waits[j].Samples
		}

		return waits[i].Event < waits[j].Event
	})

	return waits, s.blocked, s.longest, s.failures
}

// firstError returns the first error of the two, so a read failure is not
// hidden by a close that went fine.
func firstError(first, second error) error {
	if first != nil {
		return first
	}

	return second
}
