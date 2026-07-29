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

// Package harness provides the Postgres and process control the recovery tests
// are built on.
//
// It runs one container per test package and isolates tests by cloning a
// seeded template database. The migrator under test runs as a real child
// process, since SIGKILL cannot be observed in-process, and every restart
// waits for the killed child's server-side backend to disappear first. See
// [Harness.WaitBackendsGone].
//
// It uses process groups and Unix signals, so it runs on Unix-like systems
// only.
package harness

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver, per the design's stack
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// DefaultImage pins the Postgres major version. Recovery behaviour for
// concurrently-built indexes is version-sensitive, so it is pinned rather than
// floating; use [WithImage] to run against another major.
const DefaultImage = "postgres:17-alpine"

const (
	// adminDatabase is the maintenance database. Creating, cloning and dropping
	// a database requires a connection to a different database.
	adminDatabase = "postgres"

	pgRole     = "mig"
	pgPassword = "mig"

	// maintenanceConns caps the maintenance pool, which only runs catalog
	// queries and DDL against pg_database.
	maintenanceConns = 4
)

// Harness owns one Postgres container and the databases created inside it.
type Harness struct {
	container *tcpostgres.PostgresContainer
	base      *url.URL
	admin     *sql.DB
	binDir    string
	seq       atomic.Uint64
}

type config struct {
	image string
}

// Option configures [New].
type Option func(*config)

// WithImage overrides the Postgres image.
func WithImage(image string) Option {
	return func(c *config) {
		c.image = image
	}
}

// New starts a Postgres container and opens the maintenance connection.
//
// One container serves a whole test package; per-test isolation comes from
// [Harness.Clone].
func New(ctx context.Context, opts ...Option) (*Harness, error) {
	cfg := config{image: DefaultImage}

	for _, o := range opts {
		o(&cfg)
	}

	container, err := tcpostgres.Run(ctx, cfg.image,
		tcpostgres.WithDatabase(adminDatabase),
		tcpostgres.WithUsername(pgRole),
		tcpostgres.WithPassword(pgPassword),
		testcontainers.WithCmd("postgres",
			// Durability is worthless in a container that is discarded after
			// the package, and expensive when seeding millions of rows.
			"-c", "fsync=off",
			"-c", "full_page_writes=off",
			"-c", "synchronous_commit=off",
			"-c", "max_connections=200",
			// Without this, a backend only notices its client died when it next
			// writes to the socket. A backend blocked in a long CREATE INDEX
			// CONCURRENTLY would outlive the kill and finish the index.
			"-c", "client_connection_check_interval=100ms",
		),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		return nil, fmt.Errorf("start postgres %q: %w", cfg.image, err)
	}

	h := &Harness{container: container}

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return nil, h.abort(ctx, fmt.Errorf("container connection string: %w", err))
	}

	base, err := url.Parse(dsn)
	if err != nil {
		return nil, h.abort(ctx, fmt.Errorf("parse dsn %q: %w", dsn, err))
	}

	h.base = base

	admin, err := sql.Open("pgx", h.DSN(adminDatabase))
	if err != nil {
		return nil, h.abort(ctx, fmt.Errorf("open maintenance connection: %w", err))
	}

	admin.SetMaxOpenConns(maintenanceConns)
	h.admin = admin

	if err := admin.PingContext(ctx); err != nil {
		return nil, h.abort(ctx, fmt.Errorf("ping maintenance connection: %w", err))
	}

	return h, nil
}

// DSN returns the connection string for a database inside the container.
func (h *Harness) DSN(database string) string {
	u := *h.base
	u.Path = "/" + database

	return u.String()
}

// Admin returns the maintenance connection, for inspecting the cluster rather
// than a single database.
func (h *Harness) Admin() *sql.DB {
	return h.admin
}

// Open connects to a database inside the container. The caller closes the pool,
// and must do so before that database can be used as a clone template.
func (h *Harness) Open(ctx context.Context, database string) (*sql.DB, error) {
	db, err := sql.Open("pgx", h.DSN(database))
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", database, err)
	}

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping %q: %w", database, err)
	}

	return db, nil
}

// Close shuts the maintenance connection and terminates the container.
func (h *Harness) Close(ctx context.Context) error {
	if h.admin != nil {
		if err := h.admin.Close(); err != nil {
			_ = h.container.Terminate(ctx)
			return fmt.Errorf("close maintenance connection: %w", err)
		}
	}

	if err := h.container.Terminate(ctx); err != nil {
		return fmt.Errorf("terminate container: %w", err)
	}

	return nil
}

// abort tears the harness down after a failed [New] and returns cause.
func (h *Harness) abort(ctx context.Context, cause error) error {
	if err := h.Close(ctx); err != nil {
		return fmt.Errorf("%w (cleanup also failed: %w)", cause, err)
	}

	return cause
}

// quoteIdent quotes a SQL identifier. Postgres does not accept bind parameters
// where an identifier is expected, so database names in DDL are interpolated.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
