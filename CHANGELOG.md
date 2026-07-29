# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- *internal/ledger*: the ledger schema and the fenced write path. *EnsureSchema* creates schema *mig* with the *migrations*, *steps* and *lease* tables, and is safe to call from several runners at once — it holds *pg_advisory_xact_lock* for the duration, because *CREATE ... IF NOT EXISTS* still lets two simultaneous creators collide on the catalog's unique index. Every mutation goes through *Write* or *Guard*, each of which begins with *SELECT ... FOR UPDATE* on the lease row and returns *ErrFenced* when the caller's token no longer matches. *Guard* is exported separately for the writes that must ride in a caller-owned transaction: a transactional DDL step commits its ledger row with its DDL, and a backfill batch commits its cursor with the rows it covers. Ledger transactions are pinned to Read Committed rather than inheriting *default_transaction_isolation*, since under a snapshot isolation level *SELECT ... FOR UPDATE* against a row a successor has already updated aborts with a serialisation failure instead of returning no rows, reporting a final *ErrFenced* as a retryable error. Row helpers: *UpsertMigration*, *SetMigrationStatus*, *LoadMigration*.

- *internal/lease*: lease acquisition with a monotonic fence token. *Acquire* takes the lease when it is free or expired and increments *fence*, judging expiry by the server's clock so runners with disagreeing clocks still agree on whether a lease has lapsed; *Config.OnLocked* selects *Wait* or *Fail*, and an empty *Config.Owner* is rejected with *ErrNoOwner*. *Keepalive* renews at TTL/3 and returns a context cancelled with *ErrLost* as its cause, so losing the lease aborts the work rather than being logged; it gives up while validity remains, never after expiry. *Release* is itself fenced, so a superseded runner cannot hand away a lease its successor is using. *NewOwner* combines host, pid and a random suffix.

- *internal/crash*: named fault injection points armed by *MIG_CRASH_AT*. They are compiled into the shipping binary and gated on the environment rather than a build tag, so the tested program is the one that ships, and they terminate with SIGKILL so no deferred function runs and no buffered write lands.

- *test/harness*: Postgres and process control for the recovery tests. One container per test package, with per-test isolation by cloning a seeded template database rather than re-seeding. *Build* and *Start* spawn the runner under test as a real child process in its own process group, with *Kill*, *Freeze* and *Thaw* for SIGKILL, SIGSTOP and SIGCONT. *WaitBackendsGone* blocks until a killed child's server-side backend has actually disappeared, and the container sets *client_connection_check_interval=100ms* — without it a backend only notices its client died on its next socket write, so one blocked in a long *CREATE INDEX CONCURRENTLY* outlives the kill and finishes the index.

- *test/kill*: recovery tests over spawned processes. Several runners starting together admit exactly one, with the rest exiting cleanly and the fence advancing once; a waiting runner starts only after the first has finished, checked against the server's clock. A runner frozen with SIGSTOP past its lease expiry, resumed after a successor has taken over, is rejected by the fence, exits non-zero, and leaves its ledger row untouched. A runner killed mid-work does not lock the database out.

- *Makefile*: *make cover* merges two coverage sources. Parent test binaries report through *-coverprofile*; binaries spawned as children are built with *-cover* and report into *MIG_COVERDIR*. Counting only the first would report the migrator — which only ever executes in a child process — as untested.

### Changed

- *.golangci.yml*: dropped *modules-download-mode: vendor*, which conflicted with *vendor* being ignored in *.gitignore*.
