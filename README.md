<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/logo-dark.svg">
    <img alt="mig" src="assets/logo.svg" width="300">
  </picture>
</p>

<p align="center"><em>The Postgres migration tool you can kill and re-run.</em></p>

<p align="center">
  <a href="https://github.com/Tochemey/mig/actions/workflows/build.yml"><img alt="build" src="https://img.shields.io/github/actions/workflow/status/Tochemey/mig/build.yml?branch=main&label=build"></a>
  <a href="https://codecov.io/gh/Tochemey/mig"><img alt="codecov" src="https://img.shields.io/codecov/c/github/Tochemey/mig/main"></a>
  <a href="LICENSE"><img alt="license" src="https://img.shields.io/github/license/Tochemey/mig"></a>
  <a href="https://human-oss.dev"><img src="https://human-oss.dev/badge.svg" alt="Open Source AI Manifesto" /></a>
</p>

Kill a migration part way through, by SIGKILL, a pod eviction or a lost node, and the fix is to run it again. Before each step, mig asks Postgres whether the work is already there instead of trusting its own record of what ran, so the next run picks up from what the database actually holds and finishes the job.

mig is a command line tool for running migrations as a job, and a Go library, [pkg/mig](pkg/mig), for services that embed their migrations and verify them at startup. Both are one engine: every command is a thin wrapper over the library, so the two cannot drift apart.

It also lints. `mig lint` reports which statements will lock your tables, in what mode, for how long. With `--fix`, it rewrites the ones that will hurt into steps that will not. See [Linting](#linting).

## Contents

- [Premise](#premise)
- [Install](#install)
- [Quick start](#quick-start)
- [How it recovers](#how-it-recovers)
- [Migration format](#migration-format)
  - [Step kinds](#step-kinds)
  - [Inferred conditions](#inferred-conditions)
  - [Backfills](#backfills)
- [Linting](#linting)
  - [What it reports](#what-it-reports)
  - [Connected mode](#connected-mode)
  - [The rules](#the-rules)
  - [Fixing](#fixing)
  - [Silencing a rule](#silencing-a-rule)
  - [The policy file](#the-policy-file)
  - [In CI](#in-ci)
  - [Measuring it for real](#measuring-it-for-real)
- [The command line](#the-command-line)
  - [Common flags](#common-flags)
  - [Lint flags](#lint-flags)
  - [Exit codes](#exit-codes)
- [As a library](#as-a-library)
- [Deploying](#deploying)
- [Adopting an existing history](#adopting-an-existing-history)
  - [From goose](#from-goose)
  - [From golang-migrate](#from-golang-migrate)
- [Contributing](#contributing)

## Premise

Most migration tools treat their own version table as the source of truth: the table says version 7, so apply version 8. That works exactly as long as DDL is transactional, because the version write can ride in the same commit as the DDL it describes.

Postgres has statements that refuse to run inside a transaction: `CREATE INDEX CONCURRENTLY`, `DROP INDEX CONCURRENTLY`, `ALTER TYPE ... ADD VALUE`, and any backfill too large for one commit. For those, the work and the record of it are two separate commits, and a crash between them leaves the record lying. Most tools respond by marking the database dirty and halting, which is an honest admission that they no longer know what state the database is in.

`CREATE INDEX CONCURRENTLY` shows how that plays out. Interrupt the build and Postgres leaves the index in place with `indisvalid = false`, and will not resume it. A tool that trusts its own record either finds no entry and reruns the statement, which fails on the duplicate name, or finds a success entry and skips the step, shipping an index the planner never uses.

**`mig` inverts this. The catalog is the truth; the ledger is a hint.**

- The **catalog** is Postgres's own account of what exists: `pg_class`, `pg_index`, `pg_attribute` and the rest of the system tables. An index or a column is real exactly when the catalog says it is.
- The **ledger** is mig's bookkeeping: a schema named `mig`, created on the first run, holding what the catalog cannot know: each step's attempts, prior errors, a backfill's cursor, and the lease. It records what mig did, which is not the same as what is true.

Before a step runs, mig asks the catalog what the actual state is and acts on the answer. The invalid index above is found, dropped and rebuilt, whatever state the database starts in. The ledger does not authorize that decision: for non-transactional steps and backfills it is only a checkpoint and a trail. The one exception is a transactional step whose ledger row committed in the same transaction as its DDL. There "succeeded" cannot describe work that did not happen, so trusting it is safe. One invariant governs the whole design:

> Every step must converge to `succeeded` from an arbitrary, unknown starting state, using only the catalog and its own checkpoint.

## Install

**Platforms.** Linux and macOS are the tested platforms: every command, the container suite, and the recovery matrices that kill a running migration and check what it left. On Windows the binary and the library build, and CI runs their unit tests there on every change, but the recovery matrices do not run: they drive the migrator as a POSIX process group and use signals Windows has no equivalent of. So Windows is supported as far as it is tested, which is the parser, the loader, the linter and the command surface, and no further. If you are migrating production from Windows, prefer the container image below or WSL2, where the whole suite applies.

**Building mig needs cgo.** It parses SQL with the real Postgres grammar through [pg_query_go](https://github.com/pganalyze/pg_query_go), which is the server's own C parser, so a build needs a C compiler and `CGO_ENABLED=1`. That is the default on a machine that has a compiler, and Go turns cgo off silently when it cannot find one. The build then fails naming neither cgo nor the parser:

```
undefined: pgquery.Parse
```

If you see that, install a C toolchain and set `CGO_ENABLED=1`. On Debian and Ubuntu that is `build-essential`, on Alpine `build-base`, on macOS the Xcode command line tools. Cross-compiling needs a cross C toolchain too, since `GOOS` or `GOARCH` set to something other than the host also turns cgo off by default.

The command line tool:

```sh
go install github.com/tochemey/mig/cmd/mig@latest
```

The library:

```sh
go get github.com/tochemey/mig
```

A service that embeds mig inherits the requirement, and its container build is where that usually surfaces. Two things follow from it. `CGO_ENABLED=0` does not compile at all, whatever the base image. And the binary links libc dynamically, so the runtime image has to carry the same one it was built against: `scratch` and `distroless/static` have none, and a binary built against glibc will not run on Alpine, which ships musl. This repository's [Dockerfile](Dockerfile) is a worked example, cross-compilation included.

The container image, published on every release tag for linux amd64 and arm64. The parser is compiled in, so running it needs no Go toolchain and no C compiler:

```sh
docker run --rm \
  -v "$PWD/migrations:/migrations" \
  -e MIG_DSN="postgres://user:pass@db:5432/app?sslmode=disable" \
  ghcr.io/tochemey/mig:latest up --dir /migrations
```

Requirements for building from source:

- Go 1.26.5 or later, and a C compiler, for the reason above. The parser is why quoted identifiers, embedded comments, partial indexes and multi-line statements are read the way the server reads them rather than guessed at with regular expressions.
- Postgres, to run the tests. The matrices run against 17 and 18 on every change.

## Quick start

Write a migration in `migrations/`. This one is complete, so it runs against an empty database:

```sql
-- migrations/20240817120000_create_users.sql

-- +mig step: create_users
CREATE TABLE users (
    id    bigint PRIMARY KEY,
    email text
);

-- +mig step: index_email
-- +mig notx
CREATE INDEX CONCURRENTLY idx_users_email ON users (email);
```

Print what each step will be judged by, without connecting to a database:

```sh
mig plan --dir migrations
```

```
20240817120000_create_users
  0  create_users  ddl_tx    relation users exists
  1  index_email   ddl_notx  index idx_users_email exists and is valid and ready
```

Apply it:

```sh
mig up --dsn "$DATABASE_URL" --dir migrations
```

```
  [1/2] create_users   ✓ 14ms
  [2/2] index_email   ✓ 10ms
{"migrations":1,"steps_total":2,"applied":2,"skipped":0,"repaired":0,"duration_ms":31,"steps":[...]}
```

The step lines are the progress display, on stderr; on a terminal the running step animates in place with its elapsed time. The JSON line is the summary, on stdout, for a pipeline to parse.

Run it again, or kill the first run part way through and then run it again. Either way the next run does only what is missing. A run with nothing left to apply prints no progress lines on stderr; the summary JSON still goes to stdout:

```json
{"migrations":1,"steps_total":2,"applied":0,"skipped":2,"repaired":0,"duration_ms":14,"steps":[...]}
```

## How it recovers

Kill a run during a concurrent index build, then run it again:

```
  [1/2] index_legacy_email
      found invalid index, dropping and rebuilding
      ✓ 2.5s
```

Kill it again during the backfill that follows, and the third run picks up at the last committed batch:

```
  [2/2] fill_email
      resuming from id=138,000
      ✓ 13s
```

Every step starts the same way: ask the catalog whether the work is already present, and skip it if it is. What happens when work remains depends on the [step kind](#step-kinds):

1. **Transactional (`ddl_tx`).** The DDL and its ledger row commit in one transaction. A crash rolls both back, so there is nothing to repair and no postcondition to re-check. If the ledger already records the step as succeeded (which it can only do if that commit landed), the step is skipped without re-running the SQL. That is the only place the ledger decides.
2. **Non-transactional (`ddl_notx`).** Repair first when an earlier attempt left partial work (an invalid concurrent index is dropped). Record the attempt, then run the statement, then check the catalog again. A step that reports success without changing the catalog fails instead of being recorded as done.
3. **Backfill.** Resume from the last committed cursor. Each batch commits its rows with that cursor. After the walk finishes, the author-supplied `satisfied:` predicate must still hold, or the step fails.

Two things make this safe when more than one runner starts:

- **A fenced lease.** One runner applies at a time and holds a lease carrying a monotonic fence token. Every ledger write re-asserts that token in the same transaction. A runner that was frozen past its expiry, by a GC pause or a stalled VM, finds its writes rejected when it wakes instead of overwriting its successor.
- **Separate pools for control and batch work.** The CLI opens two connections to the same database so a long backfill cannot starve the heartbeat that holds the lease. Library callers that run backfills should set `Options.Work` to a second pool so the same starvation cannot happen.

## Migration format

Migrations are hand-written SQL files named `<version>_<name>.sql` and applied in version order. The version is the leading run of digits: a timestamp by convention, or a sequence number in a history adopted from another tool.

No annotation is mandatory. A plain `.sql` file with none at all is a valid migration: the whole file runs as one transactional step named `step_1`. Annotations come in when a step departs from that default: to split a file into steps, to leave the transaction, or to batch a data change.

An annotation is a comment line starting with `-- +mig`, so a migration file is still valid SQL. It applies to the step it appears in: `step:` starts the next step, and any other annotation before the first `step:` starts an implicit one. An annotation mig does not recognise is an error when the file loads, not a comment to skip, so a typo cannot silently drop an instruction.

| Annotation                                                    | Required                                                                                                                                                                                                                      | Effect                                                                                                                                                                |
| ------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `-- +mig step: <name>`                                        | Never. A file with no `step:` lines is one step named `step_1`.                                                                                                                                                               | Starts a step. The SQL that follows belongs to it.                                                                                                                    |
| `-- +mig notx`                                                | On every step holding a statement Postgres refuses inside a transaction: `CREATE INDEX CONCURRENTLY`, `DROP INDEX CONCURRENTLY`, `ALTER TYPE ... ADD VALUE`, `VALIDATE CONSTRAINT`, and similar. Never inferred from the SQL. | Runs the step outside a transaction.                                                                                                                                  |
| `-- +mig backfill: table=T key=K [batch=N] [max_lag_bytes=N]` | For a multi-batch rewrite of existing rows too large for one transaction. `table` and `key` are required; `key` must be an integer column; the bracketed settings are optional.                                               | A resumable, batched data change of exactly one statement.                                                                                                            |
| `-- +mig satisfied: sql(<expr>)`                              | On every backfill, and on a `notx` step whose condition cannot be inferred.                                                                                                                                                   | Supply the done-condition yourself when it cannot be inferred. The expression must return a single boolean.                                                           |
| `-- +mig no_lock_timeout`                                     | Never.                                                                                                                                                                                                                        | Drop the default `lock_timeout` on this step's session. Does not affect backfill batch transactions, which always keep the default.                                   |
| `-- +mig lint:ignore <rule> reason="<why>"`                   | Never.                                                                                                                                                                                                                        | Silence one [lint](#silencing-a-rule) rule over this step, or over the file when it stands above the first `step:`. The reason is mandatory. The executor ignores it. |

`notx` is the one requirement not caught before run time: mig never changes a step's kind from its SQL, so a `CREATE INDEX CONCURRENTLY` in a step without `notx` fails when Postgres refuses to run it inside the step's transaction. Everything else is checked as the files load, before anything is applied: a backfill missing `table=`, `key=` or `satisfied:`, a `satisfied:` in any form other than `sql(<expr>)`, a `notx` step with no condition, a step with no SQL, and a backfill holding more than one statement are all rejected by `mig plan` and at the start of `mig up`.

### Step kinds

Every step runs as one of three kinds. The names appear in `mig plan`, in the summary's `kind` field and in `mig status`:

| Kind       | How a step gets it          | What it means                                                                                                                                                                |
| ---------- | --------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `ddl_tx`   | The default.                | Runs inside a transaction, and its ledger row commits in that same transaction. An interrupted attempt rolls back whole, so there is never anything to reconcile.            |
| `ddl_notx` | The `notx` annotation.      | Runs outside a transaction, for the statements Postgres refuses inside one. Judged against the catalog, and repaired first when an earlier attempt left partial work behind. |
| `backfill` | The `backfill:` annotation. | A batched rewrite of existing rows over an integer key. Each batch commits together with its cursor, so an interrupted run resumes from the last committed batch.            |

### Inferred conditions

You rarely write a `satisfied:` annotation. mig derives the condition from the statement. The words below are what `mig plan` prints:

| Statement                         | Judged done when                          |
| --------------------------------- | ----------------------------------------- |
| `CREATE INDEX`                    | The index exists and is valid and ready.  |
| `DROP INDEX`                      | The index is absent.                      |
| `ALTER TABLE ADD COLUMN`          | The column exists.                        |
| `ALTER TABLE DROP COLUMN`         | The table exists and the column does not. |
| `ALTER TABLE ADD CONSTRAINT`      | The constraint exists.                    |
| `ALTER TABLE VALIDATE CONSTRAINT` | The constraint exists and is validated.   |
| `CREATE TABLE`                    | The relation exists.                      |
| `ALTER TYPE ADD VALUE`            | The enum has the label.                   |

A step counts as done only when every statement in it does. A non-transactional step whose condition cannot be inferred is rejected by `mig plan` and at the start of a run, before anything is applied.

### Backfills

A backfill rewrites existing rows in short transactions. It does not insert new data; future rows get values from a column default, a trigger, or the application. Use it for a data change too large or too locking for one commit (fill a new column, copy from a legacy column, normalize values) when the run must survive kills without redoing committed work or missing rows.

A small update that fits under a normal lock belongs in a transactional step. DDL that Postgres refuses inside a transaction needs `notx`. A multi-batch rewrite of rows that already exist is a backfill.

A backfill names its table and an integer key column (typically a `bigint` primary key). UUID, text and composite keys are not supported. Write unqualified names; `table=public.users` is quoted as a single identifier and will not resolve.

```sql
-- +mig step: fill_email
-- +mig backfill: table=users key=id batch=5000
-- +mig satisfied: sql(SELECT NOT EXISTS (SELECT 1 FROM users WHERE email IS NULL))
UPDATE users SET email = legacy_email
 WHERE id > :cursor_lo AND id <= :cursor_hi AND email IS NULL;
```

mig substitutes `:cursor_lo` and `:cursor_hi` with each batch's key range. The low bound is exclusive and the high bound inclusive, so write the predicate as `key > :cursor_lo AND key <= :cursor_hi` and consecutive batches cannot overlap.

The run is a loop, not a single statement. mig loads any cursor left in the ledger (or starts just below `min(key)`), reads the key range once, then for each batch applies the statement over the next span of keys and commits that work together with the advanced cursor in one fenced transaction. A kill mid-batch rolls both back; the next run resumes from the last committed cursor. Between batches the throttle resizes the next span from how long the last batch took and how far replication lag has grown, and it may pause when replicas are behind.

Pagination is by key range only. `OFFSET` rescans every row it skips, so its cost grows with the square of the table, and it misses rows when concurrent writes shift what it counts past.

Batch size adapts. Without `batch=` it starts at 1000. It halves when a batch runs longer than 500ms, or when `max_lag_bytes` is set and replica lag (read from `pg_stat_replication` on the control connection) exceeds it, and grows by a quarter when neither happens, staying between 100 and 50,000. Omit `max_lag_bytes` to disable the lag signal: only batch duration drives the size then, and Wait never pauses for replicas. The CLI runs batches on a separate connection pool so a backfill cannot starve the lease heartbeat; see [As a library](#as-a-library) for the equivalent when embedding.

A backfill requires `satisfied:`. The cursor records how far the key walk has got; it does not decide whether work remains. Rows inserted past the high watermark, or filtered out by the statement's `WHERE`, can still need updating after the walk finishes. `satisfied:` is checked after the loop, and the step succeeds only when it reports true.

## Linting

A migration that is correct can still take the site down. `CREATE INDEX` without `CONCURRENTLY` blocks every write to the table for the length of the build, and `ALTER COLUMN TYPE` rewrites the whole table under a lock that blocks reads as well. `mig lint` reads the migration directory and reports which statements do that, for how long, and what it stops while they run.

```sh
mig lint --dir migrations
```

```
20240817120000_add_email.sql:2: warn L003: ADD COLUMN rewrites users under ACCESS EXCLUSIVE (table rewrite: volatile default evaluated per row); add the column nullable, backfill in batches, then constrain it
    ALTER TABLE users ADD COLUMN email text NOT NULL DEFAULT gen_random_uuid()::text;
    ^
    ACCESS EXCLUSIVE on users, held for a table rewrite; blocks reads and writes
    a rewrite is available: mig lint --fix

20240817120000_add_email.sql:5: warn L001: CREATE INDEX without CONCURRENTLY blocks writes to users for the whole build; add CONCURRENTLY and mark the step notx
    CREATE INDEX idx_users_email ON users (email);
    ^
    SHARE on users, held for an index build; blocks writes

2 finding(s): 0 error(s), 2 warning(s)
```

It connects to nothing and writes nothing. The exit code is non-zero only when a finding is an error, so warnings can be read without failing a build.

### What it reports

Every finding carries three things beyond the message:

- **The lock mode**, which decides what is blocked. `SHARE` blocks writes. `ACCESS EXCLUSIVE` blocks reads as well, which means the application stops rather than slows.
- **The duration class**, which decides for how long. Catalog work is over in microseconds. A scan is one pass over the rows, a rewrite is a full copy of the table, and an index build is longer than either.
- **The severity**, which is mode and duration weighed against the size of the table.

Mode alone is not the hazard. `ALTER TABLE ... ADD COLUMN c text` takes `ACCESS EXCLUSIVE` and holds it for microseconds, which is routine. `ALTER TABLE ... ALTER COLUMN id TYPE bigint` takes the same lock and holds it for a full rewrite, which on a large table is an outage. A tool that calls both an "ALTER TABLE warning" gets muted within a week, so this one does not.

Severity follows from that. A hazard that is wrong at any scale, such as `CONCURRENTLY` inside a transaction or a `TRUNCATE`, is always an error. A hazard whose cost is the size of the table is a warning offline, because nothing offline knows that size.

### Connected mode

With `--dsn`, the linter reads the catalog and grades findings by what is actually in the tables:

```sh
mig lint --dir migrations --dsn "$REPLICA_URL"
```

```
20240817120000_widen.sql:2: error L004: changing a column type rewrites users under ACCESS EXCLUSIVE; add a new column, backfill, swap reads, then drop the old one
    ALTER TABLE users ALTER COLUMN id TYPE numeric;
    ^
    ACCESS EXCLUSIVE on users (2.0M rows, 134.9 MB), held for a table rewrite; blocks reads and writes
    estimated 1.2s to 3.3s, from this server's measured throughput; a cold or busy table is slower
    a rewrite is available: mig lint --fix
```

The same statement against a twelve-row lookup table is reported as `info`. A rewrite of a small table is not worth failing a build over, and treating it as one costs the tool its credibility on every finding after it.

Connected mode also supplies three things offline cannot:

- **The target version** is the server's own, so version-conditional rules match the database you are actually going to deploy to.
- **The estimate** comes from measuring this server once per run, with a probe that builds a 200,000-row table and rolls it back, then scaling by the size of the table each finding names. It is a range with a stated method, not a promise. A read-only standby refuses the probe, in which case the findings and sizes are all still reported and the run says why the estimates are missing.
- **The real primary key**, so a generated backfill pages by the key the table actually has instead of assuming `id`.

Point it at a production-shaped replica or a restored snapshot. It changes no schema, but it is real work on a real server.

### The rules

| ID     | Detects                                                         | Severity | Fix      |
| ------ | --------------------------------------------------------------- | -------- | -------- |
| `L001` | `CREATE INDEX` without `CONCURRENTLY`                           | by size  | no       |
| `L002` | `CONCURRENTLY` inside a transactional step                      | error    | no       |
| `L003` | `ADD COLUMN` whose default rewrites the table                   | by size  | yes      |
| `L004` | `ALTER COLUMN TYPE` causing a rewrite                           | by size  | scaffold |
| `L005` | `SET NOT NULL` without a proving validated `CHECK`              | by size  | yes      |
| `L006` | `ADD FOREIGN KEY` without `NOT VALID`                           | by size  | yes      |
| `L007` | `ADD CHECK` without `NOT VALID`                                 | by size  | yes      |
| `L008` | `ADD PRIMARY KEY` without `USING INDEX`                         | by size  | yes      |
| `L009` | Inline `UNIQUE` in `ADD COLUMN`                                 | warn     | no       |
| `L010` | `VACUUM FULL` or `CLUSTER` in a migration                       | error    | no       |
| `L011` | `REFRESH MATERIALIZED VIEW` without `CONCURRENTLY`              | warn     | no       |
| `L020` | Several `ACCESS EXCLUSIVE` statements in one transaction        | warn     | no       |
| `L021` | Row work sharing a transaction with other DDL                   | warn     | no       |
| `L022` | An index built before the backfill that fills it                | info     | no       |
| `L023` | A foreign key added before the backfill that populates it       | warn     | no       |
| `L024` | An enum value used in the transaction that added it             | error    | no       |
| `L025` | A blocking step with `no_lock_timeout`                          | error    | no       |
| `L030` | A column or table dropped out from under the application        | warn     | no       |
| `L031` | A table or column renamed                                       | error    | no       |
| `L032` | A table created and never granted to the application role       | warn     | no       |
| `L033` | `TRUNCATE` in a migration                                       | error    | no       |
| `L040` | An `UPDATE` or `DELETE` over a whole table in one transaction   | by size  | no       |
| `L041` | A `DELETE` over a large table, for the bloat it leaves          | by size  | no       |
| `L042` | A concurrent index build reconciled by a hand-written predicate | info     | yes      |

"By size" means the rule is a warning offline and is graded by the table in connected mode: an error at a million rows or a gigabyte, an observation below ten thousand rows and eight megabytes, and a warning in between. Those four thresholds move from the [policy file](#the-policy-file).

`L020` to `L025` read the whole step rather than one statement, because a transactional step holds its locks until the last statement commits. Two `ALTER TABLE`s in one step hold `ACCESS EXCLUSIVE` for the sum of both, which is not visible in either statement on its own.

`L041` needs the catalog and stays silent without it, since "large" is not something the offline pass knows.

Rules never fire against a table the same migration creates earlier in the file. That table is empty when the statement runs, so a `CREATE TABLE` followed by a `CREATE INDEX` is silent, which is the most common migration shape there is.

### Fixing

`mig lint --fix` rewrites the flagged statements as safe steps. It shows the diff and asks before writing anything:

```sh
mig lint --dir migrations --fix
```

```
--- 20240817120000_add_email.sql
- -- +mig step: add_email
- ALTER TABLE users ADD COLUMN email text NOT NULL DEFAULT gen_random_uuid()::text;
+ -- +mig step: add_users_email_nullable
+ -- the column arrives nullable and empty: catalog only, no rewrite
+ ALTER TABLE users ADD COLUMN email text;
+
+ -- +mig step: set_users_email_default
+ -- future rows get the default; existing rows are left to the backfill
+ ALTER TABLE users ALTER COLUMN email SET DEFAULT gen_random_uuid()::text;
+
+ -- +mig step: backfill_users_email
+ -- +mig backfill: table=users key=id batch=5000
+ -- +mig satisfied: sql(SELECT NOT EXISTS (SELECT 1 FROM users WHERE email IS NULL))
+ -- batched so each transaction stays short; key=id is assumed, point it at the primary key if that is not right
+ UPDATE users SET email = gen_random_uuid()::text WHERE id > :cursor_lo AND id <= :cursor_hi AND email IS NULL;
+
+ -- +mig step: users_email_not_null_check
+ -- NOT VALID: the check exists without scanning anything
+ ALTER TABLE users ADD CONSTRAINT users_email_nn CHECK (email IS NOT NULL) NOT VALID;
+
+ -- +mig step: validate_users_email_not_null
+ -- +mig notx
+ -- the scan happens here, without blocking reads or writes
+ ALTER TABLE users VALIDATE CONSTRAINT users_email_nn;
+
+ -- +mig step: set_users_email_not_null
+ -- catalog only from Postgres 12: the validated check is the proof
+ ALTER TABLE users ALTER COLUMN email SET NOT NULL;
+
+ -- +mig step: drop_users_email_check
+ -- the column carries NOT NULL itself now; the scaffolding leaves
+ ALTER TABLE users DROP CONSTRAINT users_email_nn;
apply 1 fix(es) to 1 file(s)? [y/N]:
```

The output above is abridged: generated steps that cannot be inferred carry a `satisfied:` predicate so they can be re-run after a kill; steps whose condition mig can derive from the SQL omit it. `--fix --yes` skips the question, for a branch a CI job pushes to. Add `--dsn` and the generated backfill pages by the table's real primary key instead of assuming `id`.

Every generated step carries a comment saying why it exists, because a fix nobody reads is a fix nobody can review.

Where a rewrite cannot be completed safely on its own, the fix is a plan rather than a migration. `ALTER COLUMN TYPE` is one: swapping a column's type needs the application to write to both columns across a deploy, which no linter can generate. The fix is inserted above the statement, fully commented out, with a `TODO` naming the application change, and the original statement is left alone.

Executable fixes are checked in this repository's CI against a clone of the same database: the fixed migration must produce a schema fingerprint identical to the unsafe statement and must lint clean afterwards. A fix that produced a different schema would be worse than no fix at all. `mig lint --fix` itself only shows the diff and rewrites the file; it does not run that check.

### Silencing a rule

When you have looked at a finding and disagree with it, silence that rule with a reason:

```sql
-- +mig step: rebuild_lookup
-- +mig lint:ignore L001 reason="12 rows, rebuilt nightly, verified 2026-07-29"
CREATE INDEX idx_lookup_code ON lookup (code);
```

The reason is mandatory. A directive without one is itself an error (reported as rule `L000`), because a suppression nobody had to justify is one nobody can audit later.

The directive covers the step it sits in, or the whole file when it stands above the first `step:` line. It is an annotation rather than a plain comment, which means adding one to a migration that has already been applied does not change the step's checksum and will not be reported as drift.

List them all, with their age and whether they still silence anything:

```sh
mig lint --dir migrations --report-suppressions
```

```
FILE                      LINE  RULE  AGE   STATE   REASON
20240817120000_widen.sql  2     L001  714d  used    12 rows, rebuilt nightly, verified 2026-07-29
20240817120000_widen.sql  6     L033  714d  unused  left over from a rewrite that never landed
```

`unused` means the directive silenced nothing on this run: the statement it was written for is gone, or the rule no longer fires on it. Age is the migration's own, taken from the timestamp in its file name, so a migration adopted from a tool that numbered its versions sequentially shows `-` instead.

### The policy file

A team's decisions about the catalog belong in the repository rather than in every command line. `mig lint` reads `.miglint.yaml` from the working directory if it is there, and `--policy` names another path.

```yaml
# The Postgres major to lint against, when --target-version is not given.
target_version: 15

# The sizes a size-dependent hazard changes grade at. Rows, and bytes.
thresholds:
  big_rows: 5000000
  big_bytes: 10737418240
  small_rows: 1000
  small_bytes: 1048576

# Per-rule severity: off, info, warn or error.
rules:
  L004: error
  L022: off

# The same, for one migration directory.
overrides:
  - path: services/legacy/migrations
    rules:
      L003: off
```

An unknown setting is an error rather than a line quietly skipped, so a misspelling cannot leave you believing a rule is configured when it is not. Overrides are keyed by the directory being linted, which is what makes them useful in a repository holding several migration directories. A version given on the command line beats the policy, and `--dsn` beats both.

### In CI

Findings belong where the review happens rather than in a log tab nobody opens, so there are two formats for putting them there.

`--format sarif` writes a SARIF 2.1.0 log, which GitHub code scanning turns into annotations on the changed lines of the pull request:

```yaml
- run: mig lint --dir migrations --format sarif > mig-lint.sarif
- uses: github/codeql-action/upload-sarif@v3
  with:
    sarif_file: mig-lint.sarif
```

`--format github` renders one pull-request comment: a table of file, rule, severity, what happens, the lock, and the estimate, for a reviewer deciding whether to approve.

`--format json` is there for anything else. The exit code is 1 when any finding is an error, and 0 otherwise, so a job fails on errors while still printing warnings.

### Measuring it for real

A linter tells you what a statement does. It cannot tell you what that statement will cost on your database, under your traffic, because the cost is not a property of the statement.

`ALTER TABLE users ADD COLUMN note text` takes `ACCESS EXCLUSIVE` and holds it for microseconds. On an idle table nobody notices. Behind a thirty-second reporting query it waits thirty seconds for the lock, and because Postgres grants locks in the order they were requested, every query that arrives while it waits queues behind it, including the ones that would never have conflicted with the migration at all. The table is unavailable for thirty seconds because of a statement that needed the lock for a microsecond. None of that shows up in the SQL.

`mig lint verify` measures it. It applies the migrations to a database of its own making, under traffic you describe, and reports what that did to the traffic.

```sh
mig lint verify --dsn "$SCRATCH_SERVER" \
                --dir migrations \
                --workload workload.yaml \
                --budget p99=50ms,max_block=2s
```

```
BASELINE   p50 0.8ms     p99 2.1ms     max 14ms      (41203 queries)
DURING     p50 0.9ms     p99 4310ms    max 38.2s     (12841 queries)   <- FAIL

Migration took 38.4s
Blocked readings: 1284 of 41203
Longest block:    38.2s on users
Wait attribution: Lock:relation 99.2%, IO:DataFileRead 0.5%
FAIL p99 reached 4310ms, budget 50ms
```

Here `--dsn` names a **server**, not a database to migrate. A throwaway database is created on it, used, and dropped, so nothing anybody owns is touched. Point it at a CI service container or a scratch Postgres. `--keep` leaves the database behind to look at what the migration left.

The workload file describes the traffic:

```yaml
setup:
  - CREATE TABLE accounts (id bigint PRIMARY KEY, name text NOT NULL)
  - INSERT INTO accounts SELECT g, 'u' || g FROM generate_series(1, 20000) g
  - ANALYZE accounts

keys: 20000
baseline: 30s
settle: 30s

queries:
  - name: point_read
    sql: SELECT name FROM accounts WHERE id = $1
    key: true
    rate: 200
  - name: point_write
    sql: UPDATE accounts SET name = name WHERE id = $1
    key: true
    rate: 50

slow_read:
  sql: SELECT count(*) FROM accounts WHERE id <= 3 AND pg_sleep(0.3) IS NULL
  every: 500ms
```

| Key         | Required               | Meaning                                                                                                      |
| ----------- | ---------------------- | ------------------------------------------------------------------------------------------------------------ |
| `setup`     | yes                    | Builds the schema and rows the migration runs against.                                                       |
| `keys`      | when a query binds one | The key space `$1` is drawn from.                                                                            |
| `queries`   | yes                    | The fast traffic. Each needs `name` and `sql`. `key` binds `$1`, and `rate` is per second, defaulting to 50. |
| `slow_read` | yes                    | A long-running read. `every` defaults to 2s.                                                                 |
| `baseline`  | no                     | How long to measure before the migration. Defaults to 30s.                                                   |
| `settle`    | no                     | How long to keep measuring after it. Defaults to 30s.                                                        |

`slow_read` is required, and a workload without one is refused. It is the instrument that reproduces the queue described above: with nothing holding a lock, the migration takes its own immediately, nothing piles up behind it, and the harness reports all clear on exactly the migrations that would have taken the site down.

Measurement happens from both sides, because either one alone can mislead. Client-side latency gives p50, p99, p999 and the maximum, before and during. Server-side sampling of `pg_stat_activity` says what the time was spent waiting on, which turns "queries got slow" into "queries waited on `Lock:relation`".

`--budget` takes any of `p50`, `p99`, `p999` and `max_block`, as `name=duration` pairs. A term left out is not checked. Exceeding any term exits non-zero, so this is a gate, not only a report. `max_block` matters more than it looks: a block of a third of a second inside a window measured in seconds hurts only a few percent of queries, so it can sit under a p99 budget while still being the outage you meant to catch.

## The command line

| Command           | What it does                                                                                   |
| ----------------- | ---------------------------------------------------------------------------------------------- |
| `mig up`          | Applies every pending step, in order, under one lease.                                         |
| `mig plan`        | Prints each step and the condition it will be judged by. Opens no connection.                  |
| `mig verify`      | Reports whether the database shows every migration. Takes no lease and writes nothing.         |
| `mig status`      | Prints what the ledger records: state, attempts, checkpoint and last error.                    |
| `mig import`      | Adopts a goose or golang-migrate history. Fails immediately if another runner holds the lease. |
| `mig fingerprint` | Prints a canonical digest of the schema.                                                       |
| `mig lint`        | Reports the locks each statement takes; with `--fix`, rewrites the ones that hurt.             |
| `mig lint verify` | Applies the migrations under a workload and measures what they cost.                           |

Every setting is a flag. Three of them also read an environment variable, which supplies the default when the flag is absent. An explicit flag always wins.

| Variable        | Flag          | Used by                                                   |
| --------------- | ------------- | --------------------------------------------------------- |
| `MIG_DSN`       | `--dsn`       | `up`, `verify`, `status`, `import`, `fingerprint`, `lint` |
| `MIG_DIR`       | `--dir`       | `up`, `plan`, `verify`, `import`, `lint`                  |
| `MIG_LEASE_TTL` | `--lease-ttl` | `up`, `import`                                            |

```sh
export MIG_DSN="postgres://user:pass@localhost:5432/app?sslmode=disable"
export MIG_DIR=db/migrations

mig up                       # uses both variables
mig up --dir db/hotfix       # the flag overrides MIG_DIR
```

Connect directly to Postgres, or through a session-pooling port. Transaction pooling (the usual PgBouncer default) discards session settings and advisory locks between statements, and mig refuses to run against it.

### Common flags

| Flag            | Default      | Commands                                          | Meaning                                                                                                                                                                         |
| --------------- | ------------ | ------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--dsn`         | `MIG_DSN`    | `up`, `verify`, `status`, `import`, `fingerprint` | Postgres connection string. Required. The command fails before connecting if it is empty. See also Lint flags for optional `--dsn` on `lint`.                                   |
| `--dir`         | `migrations` | `up`, `plan`, `verify`, `import`, `lint`          | Directory holding the migration files.                                                                                                                                          |
| `--lease-ttl`   | `30s`        | `up`, `import`                                    | How long the lease stays valid without a heartbeat. An unusable `MIG_LEASE_TTL` is ignored and the default used; an invalid `--lease-ttl` flag fails the command at parse time. |
| `--on-locked`   | `wait`       | `up`                                              | `wait` blocks until the lease is free. `fail` exits with code 4 straight away. `import` always fails immediately if the lease is held.                                          |
| `--allow-drift` | `false`      | `up`                                              | Continue when a migration was edited after it was applied. Without it, a checksum mismatch on a succeeded step stops the run.                                                   |
| `--verbose`     | `false`      | `up`                                              | Structured JSON logs on stderr instead of the progress display, carrying the same records.                                                                                      |
| `--from`        | none         | `import`                                          | History to adopt: `goose` or `golang-migrate`. Required.                                                                                                                        |
| `--json`        | `false`      | `status`                                          | Emit the report as JSON instead of a table.                                                                                                                                     |
| `--describe`    | `false`      | `fingerprint`                                     | Print the rows behind the digest instead of the digest.                                                                                                                         |

A few session timeouts are fixed rather than configurable. A step waits at most 3 seconds for a lock, since a migration that blocks behind a long transaction should give way rather than queue behind it and hold up every writer after it. Use `-- +mig no_lock_timeout` on a DDL step that needs to wait (not on backfill batches). Transactional steps also carry a 30-second `statement_timeout` and a 30-second idle-in-transaction timeout; `notx` steps drop the statement timeout so a concurrent index build is not killed mid-flight. A backfill without a `batch=` setting starts at 1000 rows and adapts from there.

### Lint flags

`mig lint` takes `--dir` and `--dsn` as above, and:

| Flag                    | Default         | Meaning                                                                                                                        |
| ----------------------- | --------------- | ------------------------------------------------------------------------------------------------------------------------------ |
| `--dsn`                 | `MIG_DSN`       | Read table sizes from this database and grade the findings by them. Optional: without it the linter connects to nothing.       |
| `--format`              | `human`         | `human`, `json`, `sarif` or `github`.                                                                                          |
| `--target-version`      | `18`            | The Postgres major the migrations are written for. Ignored with `--dsn`, which uses the server's own.                          |
| `--policy`              | `.miglint.yaml` | Policy file assigning severities, thresholds and a target version. Optional at the default path, required to exist when named. |
| `--fix`                 | `false`         | Rewrite flagged statements as safe steps, after showing the diff. Renders a diff, so it takes no `--format`.                   |
| `--yes`                 | `false`         | Apply fixes without asking, for CI.                                                                                            |
| `--report-suppressions` | `false`         | List every `lint:ignore` directive with its age and whether it still silences anything. Renders with `--format human`.         |

`mig lint verify` takes `--dir` as above, and:

| Flag         | Default   | Meaning                                                                                                                                     |
| ------------ | --------- | ------------------------------------------------------------------------------------------------------------------------------------------- |
| `--dsn`      | `MIG_DSN` | A server to build the throwaway database on, not a database to migrate. Required.                                                           |
| `--workload` | none      | Workload file describing the traffic to run. Required.                                                                                      |
| `--budget`   | none      | What the run may cost, as `p99=50ms,max_block=2s`. Terms: `p50`, `p99`, `p999`, `max_block`. Without it the run reports and always exits 0. |
| `--keep`     | `false`   | Leave the throwaway database behind, to look at what the migration left.                                                                    |

### Exit codes

A scheduler needs to tell another runner holding the lease apart from a failed migration, so the codes are part of the interface:

| Code | Meaning                                                                                                                               |
| ---- | ------------------------------------------------------------------------------------------------------------------------------------- |
| `0`  | Converged. For `lint`, no error-severity finding. For `lint verify`, inside the budget.                                               |
| `1`  | Failed. For `lint`, at least one error-severity finding. For `lint verify`, the budget was exceeded or the migration would not apply. |
| `3`  | Work outstanding. `verify` only.                                                                                                      |
| `4`  | Another runner holds the lease.                                                                                                       |

`mig up` writes its machine-readable summary to stdout and the progress display to stderr, so a pipeline can parse one without filtering out the other. `--verbose` swaps the display for structured JSON logs.

## As a library

Everything the binary does is available in [pkg/mig](pkg/mig), so a program can migrate without running the binary, and a service can embed its migrations instead of shipping a directory next to it:

```go
import (
    "embed"
    "io/fs"

    "github.com/tochemey/mig/pkg/mig"
)

//go:embed migrations
var embedded embed.FS

func migrate(ctx context.Context, db *sql.DB) error {
    migrations, err := fs.Sub(embedded, "migrations")
    if err != nil {
        return err
    }

    summary, err := mig.Up(ctx, db, migrations, mig.Options{})
    if err != nil {
        return err
    }

    log.Printf("migrated: applied=%d skipped=%d", summary.Applied, summary.Skipped)

    return nil
}
```

| Function                                       | What it does                                                       |
| ---------------------------------------------- | ------------------------------------------------------------------ |
| `Up(ctx, db, fsys, opts)`                      | Applies every outstanding step under the lease.                    |
| `Verify(ctx, db, fsys)`                        | Returns an error wrapping `ErrPending` if anything is outstanding. |
| `Pending(ctx, db, fsys)`                       | The same result as a list, for a caller that reports it itself.    |
| `Plan(fsys)`                                   | What a run would check, without a database.                        |
| `Status(ctx, db)`                              | What the ledger records for every step.                            |
| `Import(ctx, db, fsys, source, opts)`          | Adopts another tool's history.                                     |
| `Fingerprint(ctx, db)` and `Describe(ctx, db)` | The schema digest, and the rows behind it.                         |

The linter is there too, so a project can run it from a Go test rather than a shell:

| Function                                                             | What it does                                                                   |
| -------------------------------------------------------------------- | ------------------------------------------------------------------------------ |
| `Lint(fsys, targetVersion, policy)`                                  | Lints offline. The policy may be nil.                                          |
| `LintConnected(ctx, db, fsys, policy)`                               | The same with the catalog in reach: sizes, size-scaled severity and estimates. |
| `LoadPolicy(path, dir)`                                              | Reads a `.miglint.yaml` and resolves its overrides for one directory.          |
| `Chaos(ctx, control, work, fsys, workload, budget)`                  | Runs the harness against a database the caller supplies.                       |
| `ParseWorkload(data)`, `ParseBudget(text)`, `RenderChaos(w, report)` | The workload file, the budget string, and the report.                          |

`LintReport` carries `Findings`, `Sources`, `Suppressions` and `Uncalibrated`, and `Errors()` counts the findings that should fail a build.

Sentinel errors: `ErrPending`, `ErrLocked`, `ErrFenced`, `ErrChecksumDrift` and `ErrUnknownSource`.

`Options` works as its zero value. `TTL` defaults to `DefaultTTL`, `OnLocked` to `Wait`, and `Work` to the pool passed in. For a long backfill, set `Work` to a second `*sql.DB` against the same database so batch traffic cannot starve the lease heartbeat.

## Deploying

Run `mig up` as a job. Call `Verify` when the service starts. Point `--dsn` at a direct Postgres connection or a session-pooling port; transaction pooling is refused.

```sh
mig up --dsn "$DATABASE_URL" --dir migrations
```

```go
// migrations is an fs.FS, such as the embedded directory shown above.
if err := mig.Verify(ctx, db, migrations); err != nil {
    log.Fatalf("migrations pending; refusing to start: %v", err)
}
```

Applying migrations on service boot has two problems. Every replica races to do the same work and none of them is elected to do it, and a migration that takes minutes delays the readiness probe. `Verify` takes no lease and writes nothing, so every replica can call it at once.

## Adopting an existing history

For a project already on [goose](https://github.com/pressly/goose) or [golang-migrate](https://github.com/golang-migrate/migrate). The files stay where they are and keep their names and their contents. The only thing that changes is which tool runs them.

The shape is the same for both: point mig at the existing directory, import the history once per database, and use `mig up` from then on. Existing files run unchanged, and new migrations can use steps, backfills and inferred conditions. Import fails if the source tool's history table is absent (`goose_db_version` or `schema_migrations`).

Each line of the import report says what happens to one migration:

- `adopted`: the old tool recorded it as applied, so it is marked succeeded in the ledger and will not run again.
- `recheck`: the old tool has no record of it. The next run checks it against the catalog and applies whatever is missing, which is safe whether or not it actually ran.
- `no file`: the history names a version with no file in the directory. It is reported rather than refused, since old files get deleted once every environment is past them.

### From goose

goose keeps one row per apply and per rollback in `goose_db_version`, so the applied set is known exactly, and a migration that was rolled back is correctly left outstanding.

```sh
mig import --from goose --dsn "$DATABASE_URL" --dir migrations
```

```
adopted 2 migration(s) from goose
  adopted:  1_create_users
  adopted:  2_add_email
  recheck:  3_add_phone
```

The next `mig up` skips the two adopted migrations and applies `3_add_phone`.

### From golang-migrate

golang-migrate keeps a single high-water mark in `schema_migrations`: everything at or below it counts as applied. Its `.up.sql` and `.down.sql` pairs load as they stand, with the `.up` suffix trimmed from the identifier and the `.down.sql` files skipped.

Its `dirty` flag marks a run that died mid-migration, and golang-migrate refuses to move until someone clears the flag by hand. mig reports the flag and leaves the version it names out of the adoption, so the catalog settles it instead:

```sh
mig import --from golang-migrate --dsn "$DATABASE_URL" --dir migrations
```

```
adopted 1 migration(s) from golang-migrate
  adopted:  1_create_users
  recheck:  2_add_email
  dirty:    2 left to reconcile against the catalog
```

Here version 2 was being applied when the run died. The next `mig up` checks what actually landed and applies only what is missing, with nobody clearing anything by hand.

## Contributing

Fork, branch, and open a pull request. The project follows [Semantic Versioning](https://semver.org) and [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/). [CONTRIBUTING.md](CONTRIBUTING.md) covers the development setup, the commit format, the code conventions and what the tests expect.
