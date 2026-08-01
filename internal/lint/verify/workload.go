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
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// The defaults a config leaves out.
const (
	defaultBaseline = 30 * time.Second
	defaultSettle   = 30 * time.Second
	defaultRate     = 50
	defaultSlowRead = 2 * time.Second
)

// slowReadClass is the name the long-running reader's samples are kept under,
// separate from the fast classes it must not be averaged into.
const slowReadClass = "slow_read"

// Config is the workload, as it is written in the file.
type Config struct {
	// Setup builds the schema and the rows the migration will run against.
	Setup []string `yaml:"setup"`

	// Keys is the range a query's bound parameter is drawn from, which is
	// the primary key space the setup created.
	Keys int `yaml:"keys"`

	// Queries is the fast traffic: the reads and writes an application makes
	// while a migration is running.
	Queries []Query `yaml:"queries"`

	// SlowRead is the long-running reader, and it is required.
	//
	// The catastrophe a migration causes is rarely the DDL itself: it is the
	// DDL queueing behind a slow query and, because Postgres grants locks in
	// order, blocking everything that arrives afterwards. A workload without
	// a slow reader reports all clear on migrations that take a site down.
	SlowRead SlowRead `yaml:"slow_read"`

	// Baseline is how long to measure before the migration, Settle how long
	// to keep measuring after it.
	Baseline time.Duration `yaml:"baseline"`
	Settle   time.Duration `yaml:"settle"`
}

// Query is one class of fast traffic.
type Query struct {
	Name string `yaml:"name"`
	SQL  string `yaml:"sql"`

	// Key binds $1 to a key drawn from the configured range.
	Key bool `yaml:"key"`

	// Rate is how many times a second to run it.
	Rate int `yaml:"rate"`
}

// SlowRead is the long query that makes lock queueing visible.
type SlowRead struct {
	SQL   string        `yaml:"sql"`
	Every time.Duration `yaml:"every"`
}

// ParseConfig reads a workload file.
func ParseConfig(data []byte) (Config, error) {
	var config Config

	if err := yaml.Unmarshal(data, &config); err != nil {
		return Config{}, fmt.Errorf("read workload: %w", err)
	}

	config.applyDefaults()

	return config, config.validate()
}

// applyDefaults fills in what the file left out.
func (c *Config) applyDefaults() {
	if c.Baseline <= 0 {
		c.Baseline = defaultBaseline
	}

	if c.Settle <= 0 {
		c.Settle = defaultSettle
	}

	if c.SlowRead.Every <= 0 {
		c.SlowRead.Every = defaultSlowRead
	}

	for i := range c.Queries {
		if c.Queries[i].Rate <= 0 {
			c.Queries[i].Rate = defaultRate
		}
	}
}

// validate refuses a workload that cannot measure what it claims to.
func (c Config) validate() error {
	if len(c.Setup) == 0 {
		return errors.New("workload: setup builds the schema the migration runs against, and is required")
	}

	if len(c.Queries) == 0 {
		return errors.New("workload: queries are the traffic being measured, and are required")
	}

	for _, query := range c.Queries {
		if query.Name == "" || query.SQL == "" {
			return fmt.Errorf("workload: query %q needs a name and sql", query.Name)
		}

		if query.Key && c.Keys <= 0 {
			return fmt.Errorf("workload: query %q binds a key, so keys must say how many there are",
				query.Name)
		}
	}

	// The design's review checklist rejects a workload without one, and so
	// does this: without a slow reader the lock-queue hazards never
	// reproduce, and the harness reports all clear on the migrations that
	// matter most.
	if c.SlowRead.SQL == "" {
		return errors.New("workload: slow_read is required, because a migration queued behind a " +
			"long read is what blocks everything arriving after it")
	}

	return nil
}

// Workload is the traffic running against the database under test.
type Workload struct {
	db     *sql.DB
	config Config

	mu      sync.Mutex
	classes map[string]*Latency

	wg   sync.WaitGroup
	stop context.CancelFunc
}

// Start begins the traffic. Every class runs on its own goroutine, and the
// pool is the caller's: the migration must not share it, because a pool run
// dry looks exactly like lock contention and would be reported as it.
func Start(ctx context.Context, db *sql.DB, config Config) *Workload {
	ctx, stop := context.WithCancel(ctx)

	w := &Workload{db: db, config: config, classes: map[string]*Latency{}, stop: stop}

	for _, query := range config.Queries {
		w.run(ctx, query.Name, query.SQL, query.Key, time.Second/time.Duration(query.Rate))
	}

	w.run(ctx, slowReadClass, config.SlowRead.SQL, false, config.SlowRead.Every)

	return w
}

// run drives one class until the context ends.
func (w *Workload) run(ctx context.Context, name, query string, key bool, every time.Duration) {
	w.wg.Go(func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Published as it is measured, not at the end: the baseline
				// window is taken while the workers are still running.
				w.record(name, w.once(ctx, query, key))
			}
		}
	})
}

// record files one observation under its class.
func (w *Workload) record(name string, took time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.classes[name] == nil {
		w.classes[name] = &Latency{}
	}

	w.classes[name].Record(took)
}

// once runs the query and reports how long the client waited.
//
// A failure is timed like a success on purpose: a statement the migration
// made fail waited for its refusal, and dropping it would flatter the very
// window this exists to measure. The error itself is not the finding; the
// latency is.
func (w *Workload) once(ctx context.Context, query string, key bool) time.Duration {
	args := []any{}
	if key {
		// Traffic wants to be spread over the keys, not to be unguessable.
		//nolint:gosec // G404: choosing which row to read needs no cryptographic randomness.
		args = append(args, 1+rand.IntN(w.config.Keys))
	}

	started := time.Now()

	rows, err := w.db.QueryContext(ctx, query, args...)
	if err == nil {
		for rows.Next() { //nolint:revive // the rows are drained, not read: the wait is the measurement.
		}

		_ = rows.Close()
	}

	return time.Since(started)
}

// Take returns what has been measured so far and starts a fresh window, which
// is how the baseline is separated from the run.
func (w *Workload) Take() Window {
	w.mu.Lock()
	defer w.mu.Unlock()

	taken := w.classes
	w.classes = map[string]*Latency{}

	return newWindow(taken)
}

// Stop ends the traffic and returns the window since the last [Workload.Take].
func (w *Workload) Stop() Window {
	w.stop()
	w.wg.Wait()

	return w.Take()
}
