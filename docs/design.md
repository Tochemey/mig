# Design

This document records the decisions mig is built on and the invariants any change must preserve. The [README](../README.md) explains how to use the tool; this document explains why it is shaped the way it is. Read it before changing the executor, the lease, or a step kind.

## Contents

- [Premise](#premise)
- [Scope](#scope)
- [Architecture](#architecture)
- [Migration format](#migration-format)
- [The ledger](#the-ledger)
- [Leases and fencing](#leases-and-fencing)
- [Sessions](#sessions)
- [Steps](#steps)
- [The executor](#the-executor)
- [Observability](#observability)
- [Commands](#commands)
- [Adoption](#adoption)
- [Down migrations](#down-migrations)
- [Testing](#testing)
- [Review checklist](#review-checklist)
- [The definition of success](#the-definition-of-success)

## Premise

Migration tools treat their version table as the source of truth: "we are at version 7, apply 8." That works only while DDL is transactional, because the version write can ride in the same commit as the DDL.

`CREATE INDEX CONCURRENTLY` cannot run in a transaction. Neither can `ALTER TYPE ... ADD VALUE`, `DROP INDEX CONCURRENTLY`, or a multi-hour chunked backfill. For those, the DDL commit and the ledger commit are two separate events, and a crash between them leaves the ledger lying. Existing tools respond by marking the database dirty and halting, an honest admission that they have no idea what state the database is in.

mig inverts this. **The catalog is the truth; the ledger is a hint.**

Before any step runs, mig asks the database what its actual state is and acts on the answer. The ledger keeps what the catalog cannot know: backfill cursors, attempt counts, lease ownership, prior errors. It is never permitted to authorize an action.

> Every step must converge to `succeeded` from an arbitrary, unknown starting state, using only the catalog and its own checkpoint.

This sentence governs every decision below. If a proposed change requires trusting the ledger to decide whether work is needed, the change is wrong.

## Scope

mig deliberately does not do these:

- Declarative schema diffing. Hand-written, ordered, reviewable SQL is the positioning.
- Any database other than Postgres. The design leans on the Postgres catalog, its grammar, and its locking behavior at every layer.
- Executing down migrations (see [Down migrations](#down-migrations)).
- Lock and hazard linting. Planned for a later version, where it can emit fixes as steps into a runtime that already works.
- ORM integration, schema DSLs, or migrations written in Go.
- Parallel step execution.

## Architecture

mig is library-first. `pkg/mig` is the whole command surface (`Up`, `Verify`, `Pending`, `Status`, `Plan`, `Import`, `Fingerprint`, `Describe`) and the CLI is a thin wrapper over it, so the binary and the library cannot drift apart. Teams embed `Verify` in their service binary; that path must never require the CLI.

```
cmd/mig/             CLI entrypoint
pkg/mig/             public API, the whole command surface
internal/cli/        cobra command tree, flag parsing, the progress renderer
internal/plan/       migration directory scan: annotations, steps, checksums
internal/parse/      pg_query_go wrappers, statement classification
internal/predicate/  catalog predicates and inference
internal/catalog/    catalog introspection, schema fingerprinting
internal/ledger/     ledger schema, fenced reads and writes
internal/lease/      acquisition, heartbeat, fence tokens
internal/session/    pinned connections, GUCs, pooler detection, lock retry
internal/step/       step interfaces and the three kinds
internal/exec/       the executor loop
internal/throttle/   adaptive backfill pacing
internal/importer/   goose and golang-migrate adapters
internal/crash/      fault injection points for the kill tests
test/harness/        containers, template clone, subprocess control, TCP proxy
test/kill/           recovery matrices, partition tests, seeded fuzz
test/faultdb/        a database driver that fails on command
```

The stack: `database/sql` over the pgx stdlib driver, `pg_query_go` for SQL parsing (the real Postgres grammar, never regex), cobra for the CLI, `log/slog` for logging, testcontainers for the test suite. No ORM, no `init()` side effects, no package-level mutable state. Every exported function takes `context.Context` first; errors are wrapped with `%w`.

## Migration format

Migrations are annotated SQL files, named `<version>_<name>.sql` with a zero-padded timestamp version, ordered lexicographically. Annotations are comments of the form `-- +mig <annotation>`:

| Annotation               | Meaning                                                                |
|--------------------------|------------------------------------------------------------------------|
| `step: <name>`           | starts a new step; the name must be unique within the migration        |
| `notx`                   | run outside a transaction (required for `CONCURRENTLY` and friends)    |
| `backfill: k=v ...`      | chunked resumable step; keys: `table`, `key`, `batch`, `max_lag_bytes` |
| `satisfied: <predicate>` | override the inferred precondition                                     |
| `no_lock_timeout`        | suppresses the default `lock_timeout`; must be explicit                |

SQL before any `step:` annotation belongs to an implicit step. Multi-statement steps are permitted; all statements of a transactional step share one transaction. The [README](../README.md) documents the format from the user's side, including the full inference table.

### Predicate inference

Each step's SQL is parsed with `pg_query_go` and a `Satisfied` predicate is inferred from the statement: an index build infers "the index exists, is valid and is ready", a dropped column infers "the column is absent", and so on for the statement shapes `internal/predicate` covers. Inference is the ergonomic core: the user should almost never write `satisfied:` by hand.

When inference fails, behavior depends on the step kind:

- **Transactional steps** need no predicate. The ledger write shares the DDL transaction, so the ledger is atomic with the work and may be trusted as a fallback.
- **Non-transactional steps refuse to run.** A non-transactional step with no way to check its own state cannot converge, so the plan fails with an error telling the user to add `satisfied:`. This is a feature: it is the tool refusing to be in the situation every other tool ships in.

The escape hatch is `satisfied: sql(SELECT ...)`, any expression returning a single boolean.

### Checksums

Each step gets a SHA-256 of its normalized SQL, using the `pg_query_go` fingerprint so whitespace and comment edits do not trip it. A checksum mismatch on an already succeeded step is a hard error unless `--allow-drift` is passed, and even then it is surfaced as a warning the run cannot finish without.

## The ledger

The ledger lives in schema `mig`, created on first run. The authoritative DDL is in `internal/ledger`; this is its shape:

```sql
CREATE SCHEMA IF NOT EXISTS mig;

CREATE TABLE IF NOT EXISTS mig.migrations (
  id           text PRIMARY KEY,              -- '20240817120000_add_email'
  name         text NOT NULL,
  status       text NOT NULL,                 -- pending|running|succeeded|failed
  started_at   timestamptz,
  finished_at  timestamptz
);

CREATE TABLE IF NOT EXISTS mig.steps (
  migration_id text NOT NULL REFERENCES mig.migrations(id),
  idx          int  NOT NULL,
  name         text NOT NULL,
  kind         text NOT NULL,                 -- ddl_tx|ddl_notx|backfill
  checksum     bytea NOT NULL,
  status       text NOT NULL,                 -- pending|running|succeeded|failed
  attempts     int  NOT NULL DEFAULT 0,
  checkpoint   jsonb,                         -- backfill cursor
  error        text,
  started_at   timestamptz,
  finished_at  timestamptz,
  PRIMARY KEY (migration_id, idx)
);

CREATE TABLE IF NOT EXISTS mig.lease (
  id    int PRIMARY KEY DEFAULT 1 CHECK (id = 1),
  owner text,                                 -- host/pid/random suffix
  fence bigint NOT NULL DEFAULT 0             -- monotonic, incremented per acquisition
);

CREATE TABLE IF NOT EXISTS mig.lease_expiry (
  id           int PRIMARY KEY DEFAULT 1 CHECK (id = 1),
  expires_at   timestamptz,
  heartbeat_at timestamptz
);
```

The `status` columns are diagnostic only. Neither `failed` nor `running` may ever be used to decide whether work is required. There is no dirty flag and no force command; if a change seems to need one, the reconciliation path is incomplete.

Ownership and expiry are two rows on purpose. Every ledger write locks the ownership row for the length of its transaction, and a backfill's transaction lasts as long as its batch. If the heartbeat wrote that same row it would queue behind the batch and the holder would give up a lease it still owns, under exactly the load the lease exists to survive. So `mig.lease` holds owner and fence and is locked by the write guard, while `mig.lease_expiry` holds the heartbeat columns and is written by renewal. A long batch blocks a takeover, which is correct, without blocking renewal.

## Leases and fencing

An advisory lock is right for a 200ms `ALTER` and wrong for a six-hour backfill: it dies with the session and cannot be resumed. mig uses a lease.

Acquisition is one transaction of three statements: claim, take, start expiry. The claim is:

```sql
SELECT l.owner IS NULL OR e.expires_at IS NULL OR e.expires_at < now()
  FROM mig.lease l
  JOIN mig.lease_expiry e ON e.id = l.id
 WHERE l.id = 1
   FOR UPDATE OF l, e
```

Both rows are locked, not just the lease, and this is load-bearing. Under Read Committed, a waiter that blocks on a locked row re-reads, when the lock clears, only the rows it locked; the rest of the join still comes from its original snapshot. With the expiry row unlocked, a loser re-reads the winner's owner beside the expiry as it stood before the winner wrote it, sees NULL, concludes the lease lapsed, and takes a lease someone else holds. Expiry is judged by the server's clock, so runners whose own clocks disagree still agree on whether a lease lapsed.

When the lease is held, behavior is `--on-locked=wait|fail`, default wait with a timeout.

**Heartbeat.** A goroutine renews at TTL/3. Each renewal attempt is bounded by the heartbeat interval, because a network partition is not a crash: the socket stays open, the read never returns, and an unbounded attempt sits inside a dead call while the lease expires underneath it. The holder checks remaining validity after each attempt and stops working before the lease expires, never after. Releasing the lease is likewise bounded by the TTL, since waiting longer than the lease lasts achieves nothing.

**Fencing.** Acquisition increments a monotonic fence token. Every ledger mutation goes through one guard that opens a transaction, locks the ownership row, and checks owner and fence. Zero rows means `ErrFenced`: abort immediately with a non-zero exit. This is what stops a runner frozen by a GC pause or a VM stall from waking up and clobbering its successor.

## Sessions

All step execution happens on a pinned `*sql.Conn`, never on `*sql.DB`. Session state (`SET`, advisory locks, `search_path`) is meaningless on a pooled handle, and getting this wrong produces failures that appear only under concurrency.

On acquiring a conn, mig sets `application_name` (so an operator can see who is holding a lock), `lock_timeout` (3s default, suppressed only by an explicit `no_lock_timeout`), `statement_timeout` (30s for transactional steps, unlimited for non-transactional ones, because index builds are long), and `idle_in_transaction_session_timeout` (30s).

Two pools: a control pool for ledger and lease, and a work pool for backfill batches. A backfill saturating the pool must never starve the heartbeat; spurious lease loss would arrive under exactly the load where it hurts most.

**Lock retry.** On `55P03 lock_not_available`, retry with exponential backoff. Postgres lock requests queue in order, so a migration waiting behind a long `SELECT` blocks every later query on that table. The migration is not what takes the outage; waiting for the lock is. A short `lock_timeout` plus retry sheds the queue instead of holding it.

**Pooler detection.** Under transaction pooling, session state silently vanishes. At startup, mig sets `application_name` on a pinned conn and reads it back in a separate call; a mismatch means transaction pooling, and mig refuses to run with an error naming the problem and suggesting a direct connection. It does not attempt to work around it.

## Steps

The step interfaces in `internal/step` encode the invariant: transactional steps commit their ledger write with their DDL; non-transactional ones cannot, and must therefore be reconcilable.

- `Step` is the base: `Meta`, `Satisfied`, `Repair`. `Satisfied` reports whether the work is already done, judged only by the catalog; it may never consult the ledger, and must be safe to call at any time. `Repair` returns the database from a known-partial state to one `Apply` can run from; it must be idempotent and safe to interrupt, because the kill tests kill the process during it.
- `TxStep` applies inside a caller-supplied transaction, and the executor writes the ledger row in that same transaction, so no reconciliation window exists.
- `NoTxStep` applies outside any transaction. A crash between `Apply` returning and the ledger write is expected, and is recovered by `Satisfied`.
- `ResumableStep` runs long and checkpoints as it goes, through a fenced environment the executor supplies.

The rule, enforced in review: **`Satisfied` may not read `mig.*`.**

### `ddl_tx`

`ApplyTx` executes the statements. `Satisfied` uses the inferred predicate when there is one and otherwise returns false: the ledger write is atomic here, so a false negative just re-runs inside a transaction that will fail or no-op, which is acceptable and safe.

### `ddl_notx`

`Apply` executes statements on the pinned conn. A predicate is required; the plan refuses otherwise. `Repair` knows the partial states each statement kind can leave: an interrupted `CREATE INDEX CONCURRENTLY` leaves an invalid index, so repair drops it with `DROP INDEX CONCURRENTLY IF EXISTS` and lets the build run again; statements that cannot be left partial repair as no-ops. Repair must itself be crash-safe: always `IF EXISTS`, never a bare `DROP`.

### `backfill`

Keyset pagination only. `OFFSET` is forbidden: it degrades quadratically and skips rows under concurrent writes. Each batch covers `(cursor, cursor + batch]`, low bound exclusive, high bound inclusive, and the user's SQL sees the range as `:cursor_lo` and `:cursor_hi`.

**The batch and its checkpoint commit in one transaction.** This is a correctness requirement, not an optimization: it makes the "committed the work but lost the cursor" crash structurally impossible. Index builds cannot be made atomic; backfill checkpoints can, so they must be. The consequence is that the fencing guard applies inside the work transaction too, the same ownership-row lock at the top.

The throttle is a closed loop, not a fixed sleep. It measures what the last batch cost and what the replicas can absorb: when batch latency exceeds the target or replication lag exceeds `max_lag_bytes`, the next batch halves; on recovery it grows back by a quarter, clamped between 100 and 50,000 rows. The lag source reads `pg_stat_replication` and treats no replicas as zero lag; tests drive the loop with a mocked source.

## The executor

`internal/exec` runs one migration at a time, one step at a time, in order. For each step:

1. **Catalog first, always.** `Satisfied` is called before anything else, unconditionally. Not "if the ledger looks suspicious": always. These are cheap catalog queries, and this line is the product.
2. If the ledger records a prior attempt, the database may be partially mutated: run `Repair`, then check `Satisfied` again, because repair may have completed the step.
3. Apply, by kind: a `TxStep` commits its ledger row inside its own transaction; a `NoTxStep` applies under lock retry and then must pass `Satisfied` as a postcondition; a `ResumableStep` runs with fenced checkpoints and the same postcondition.

Two properties must survive any refactor:

1. `Satisfied` is called first, unconditionally.
2. A `TxStep`'s ledger write happens inside the step's own transaction. Move it outside and the tool silently regains the bug it exists to fix; the transactional recovery tests exist to catch exactly this.

On any step error: record the error and `failed` status, release the lease, exit non-zero. There is no force command. The next run reconciles.

## Observability

The executor emits one structured record per step transition through `slog`: `step running`, `step done`, `repairing partial step`, `backfill resuming`, `backfill progress`. The CLI installs a renderer on stderr that turns those records into the progress display: animated in place on a terminal, append-only anywhere else, and silent for steps the catalog already shows as done. `--verbose` swaps the renderer for JSON logs carrying the same records, and a library caller who passes their own `slog.Logger` gets the records raw.

The machine-readable summary goes to stdout on exit, and it is a contract, not a convenience; the test suite asserts on it:

```json
{"migrations":1,"steps_total":4,"applied":2,"skipped":2,"repaired":1,
 "duration_ms":362104,"steps":[{"name":"index_email","status":"succeeded",
 "attempts":2,"repaired":true,"duration_ms":361002}]}
```

`applied == 0` on a second run is the convergence assertion. Tests parse the summary, never the human-readable display.

## Commands

| Command                                   | Behavior                                                               |
|-------------------------------------------|------------------------------------------------------------------------|
| `mig up`                                  | acquire the lease, apply pending steps, converge                       |
| `mig verify`                              | read-only, no lease; exit 0 if fully applied, 3 if pending, 1 on error |
| `mig status`                              | per-step state, attempts, checkpoint progress; human table or `--json` |
| `mig plan`                                | parse and print inferred predicates and step kinds; no database writes |
| `mig import --from goose\|golang-migrate` | adopt an existing history (see [Adoption](#adoption))                  |
| `mig fingerprint`                         | emit the canonical schema fingerprint, used by tests and drift checks  |

`verify` is the production startup path. The migrate-on-boot pattern is a trap: every replica races at startup, there is no leader election, and a slow migration collides with the readiness probe. The correct shape is apply as a separate job (`mig up`) and verify in the service binary: fail fast if migrations are pending, never apply them. `mig.Verify` exists to make that one line of code.

Exit codes are part of the contract: 0 converged, 1 error, 3 pending, 4 lease held. Configuration comes from flags and environment variables; there is no config file.

## Adoption

Nobody migrates their migrations, so interoperation is the distribution strategy. `mig import --from goose|golang-migrate` reads the existing version table, synthesizes a migration row with a single succeeded step for each applied version, and adopts the existing `.sql` files in place, with no file rewriting. It prints exactly what was adopted and what the next run will re-check.

After import, existing files run unchanged; new migrations may use steps, backfills, and predicates. A dirty flag in the source tool's table maps to: ignore it, reconcile from the catalog, report what was found.

## Down migrations

mig does not execute rollbacks. They are rarely tested, frequently lossy, and in a real incident nobody uses them; you roll forward. The version of this feature with real value is a CI check that applies up, down, up against an ephemeral database and diffs the schema fingerprint; that is future work, and the reason `mig fingerprint` exists.

## Testing

The suite's job is to prove convergence from states no unit test reaches naturally, so it manufactures them:

- **Kill tests** (`test/kill`) spawn the migrator as a real subprocess and SIGKILL it at fault points injected through `internal/crash`: after the DDL, before the ledger write, mid-repair, mid-batch. The harness waits for the server to notice the dead client before rerunning, because a Postgres backend can outlive its process and finish the index build.
- **Partition tests** route the runner through an in-process TCP proxy (`test/harness`) whose cut stops delivery in both directions while leaving every socket open, because a partition is not a crash: reads hang rather than fail, which is what caught the unbounded heartbeat.
- **Seeded fuzz** drives random kill schedules through the same harness. Every failure prints its seed, and the seed reproduces the run exactly. A nightly workflow runs the fuzz across the supported Postgres majors.
- **Driver faults** (`test/faultdb`) fail individual database calls to reach the executor branches a process kill cannot.

Tests assert on the catalog and the stdout summary, never on log text.

## Review checklist

Reject any change where:

- `Satisfied` reads `mig.*` instead of the catalog
- a ledger write bypasses the fenced helper
- a `TxStep` ledger write happens outside the step's transaction
- a `Repair` uses a bare `DROP` rather than `IF EXISTS`
- a backfill uses `OFFSET`, or commits a batch without its checkpoint
- step execution runs on `*sql.DB` instead of a pinned `*sql.Conn`
- a new dirty, force, or skip flag appears
- `lock_timeout` is unset without an explicit `no_lock_timeout` annotation
- a crash point is removed to make a test pass

## The definition of success

Success is not "all tests pass." It is this transcript:

```
$ mig up
  [2/3] index_email   ⣾ 4m12s
  ^C

$ mig up
  [2/3] index_email
      found invalid index, dropping and rebuilding
      ✓ 6m01s
  [3/3] fill_email
      resuming from id=4,210,000
      ✓ 22m
```

A run is killed in the middle of an index build, and the rerun finds the damage, names it, repairs it, and finishes, with no flag and no human decision. In the same situation golang-migrate prints `Dirty database version 2. Fix and force version.` and stops, and "fix" means a person inspecting the database and deciding by hand which version to force. Any change that would make the second `mig up` need a question answered before it can run has broken the product.
