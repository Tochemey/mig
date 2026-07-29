# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- *cmd/mig up*: applies a directory of annotated SQL and converges on it. Ordered by a zero-padded timestamp in the file name, one lease per run, one pinned connection per step. A machine-readable summary goes to stdout and structured logs to stderr, so the two never interleave. Exit codes distinguish a failure from another runner holding the lease.

- *internal/exec*: the executor. Before anything else it asks the catalog whether the step's work is already present, unconditionally rather than when the ledger looks suspicious, and skips the step if so. Only then does it consult the ledger, and only to decide whether an earlier attempt left the database part-way through and needs repairing first. Attempts are recorded before the work starts, so a crash during the work leaves evidence. After the work it re-checks the predicate: a step that reports success without changing the catalog fails rather than being recorded as done.

- *internal/step*: the *Step* and *NoTxStep* interfaces, and *DDLNoTx*. A non-transactional step cannot commit its ledger row with its work, so it carries a predicate that recognizes finished work in the catalog and a *Repair* that clears a partial one. *Repair* drops an index left invalid by an interrupted concurrent build — Postgres will not resume one — and is itself re-entrant, using *IF EXISTS* and re-reading the catalog rather than trusting a previous call. Constructing a non-transactional step without a predicate fails with *ErrNoPredicate*.

- *internal/parse*: statement classification through the real Postgres grammar, so quoted identifiers, embedded comments, partial indexes and multi-line statements are read as the server reads them. *Checksum* hashes the statement's fingerprint rather than its text, so reformatting a migration or editing its comments does not register as drift while a changed index, table, column or *UNIQUE* does.

- *internal/predicate*: inference from parsed SQL. *CREATE INDEX* is satisfied when the index exists and is both valid and ready; *DROP INDEX* when it is gone. A step is satisfied only when all of its statements are, and one unclassified statement makes the whole step uninferable. *SQL* provides the *satisfied: sql(...)* escape hatch.

- *internal/plan*: reads annotated SQL into ordered steps. Annotations: *step:*, *notx*, *satisfied:* and *no_lock_timeout*. SQL before the first *step:* becomes an implicit step, so an unannotated file is a valid single-step migration. Duplicate versions, duplicate step names, unknown annotations and empty steps are rejected at load.

- *internal/catalog*: index introspection resolved through *to_regclass*, so a name is looked up exactly as the statement that created it would resolve it. *Fingerprint* hashes columns, indexes (with *indisvalid* and *indisready*), constraints (with *convalidated*), sequences, ownership and grants into a canonical form; *Describe* returns the same content readably so a mismatch can be diffed. System schemas are excluded, since TOAST index names embed the OID of the table they serve.

- *internal/session*: pinned-connection setup. Session state on a pooled handle is meaningless, so every step runs on its own *sql.Conn*. *DetectPooling* probes for a transaction-pooling pooler in two round trips and refuses to run behind one rather than working around it. *WithLockRetry* retries only *55P03*, backing off from one second with jitter.

- Recovery tests over the migrator as a spawned process, covering each point at which it can die during a concurrent index build: before the lease, after the lease, once recorded as running, before the DDL, after the DDL commits but before the ledger write, after the step succeeds, during an interrupted build, and inside the repair itself. Each asserts a schema and data fingerprint equal to an uninterrupted run, no invalid indexes or unvalidated constraints, no lease still held, and — the assertion that catches a migrator which reapplies its work forever — that a further run applies and repairs nothing.

- *internal/ledger*: the ledger schema and the fenced write path. *EnsureSchema* creates schema *mig* with the *migrations*, *steps* and *lease* tables, and is safe to call from several runners at once — it holds *pg_advisory_xact_lock* for the duration, because *CREATE ... IF NOT EXISTS* still lets two simultaneous creators collide on the catalog's unique index. Every mutation goes through *Write* or *Guard*, each of which begins with *SELECT ... FOR UPDATE* on the lease row and returns *ErrFenced* when the caller's token no longer matches. *Guard* is exported separately for the writes that must ride in a caller-owned transaction: a transactional DDL step commits its ledger row with its DDL, and a backfill batch commits its cursor with the rows it covers. Ledger transactions are pinned to Read Committed rather than inheriting *default_transaction_isolation*, since under a snapshot isolation level *SELECT ... FOR UPDATE* against a row a successor has already updated aborts with a serialisation failure instead of returning no rows, reporting a final *ErrFenced* as a retryable error. Row helpers: *UpsertMigration*, *SetMigrationStatus*, *LoadMigration*.

- *internal/lease*: lease acquisition with a monotonic fence token. *Acquire* takes the lease when it is free or expired and increments *fence*, judging expiry by the server's clock so runners with disagreeing clocks still agree on whether a lease has lapsed; *Config.OnLocked* selects *Wait* or *Fail*, and an empty *Config.Owner* is rejected with *ErrNoOwner*. *Keepalive* renews at TTL/3 and returns a context cancelled with *ErrLost* as its cause, so losing the lease aborts the work rather than being logged; it gives up while validity remains, never after expiry. *Release* is itself fenced, so a superseded runner cannot hand away a lease its successor is using. *NewOwner* combines host, pid and a random suffix.

- *internal/crash*: named fault injection points armed by *MIG_CRASH_AT*. They are compiled into the shipping binary and gated on the environment rather than a build tag, so the tested program is the one that ships, and they terminate with SIGKILL so no deferred function runs and no buffered write lands.

- *test/harness*: Postgres and process control for the recovery tests. One container per test package, with per-test isolation by cloning a seeded template database rather than re-seeding. *Build* and *Start* spawn the runner under test as a real child process in its own process group, with *Kill*, *Freeze* and *Thaw* for SIGKILL, SIGSTOP and SIGCONT. *WaitBackendsGone* blocks until a killed child's server-side backend has actually disappeared, and the container sets *client_connection_check_interval=100ms* — without it a backend only notices its client died on its next socket write, so one blocked in a long *CREATE INDEX CONCURRENTLY* outlives the kill and finishes the index.

- *test/kill*: recovery tests over spawned processes. Several runners starting together admit exactly one, with the rest exiting cleanly and the fence advancing once; a waiting runner starts only after the first has finished, checked against the server's clock. A runner frozen with SIGSTOP past its lease expiry, resumed after a successor has taken over, is rejected by the fence, exits non-zero, and leaves its ledger row untouched. A runner killed mid-work does not lock the database out.

- *Makefile*: *make cover* merges two coverage sources. Parent test binaries report through *-coverprofile*; binaries spawned as children are built with *-cover* and report into *MIG_COVERDIR*. Counting only the first would report the migrator — which only ever executes in a child process — as untested.

### Changed

- *.golangci.yml*: dropped *modules-download-mode: vendor*, which conflicted with *vendor* being ignored in *.gitignore*.
