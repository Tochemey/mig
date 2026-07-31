<h1 align="center">mig</h1>

<p align="center"><em>The Postgres migration tool you can kill and re-run.</em></p>

<p align="center">
  <a href="https://github.com/Tochemey/mig/actions/workflows/build.yml"><img alt="build" src="https://img.shields.io/github/actions/workflow/status/Tochemey/mig/build.yml?branch=main&label=build"></a>
  <a href="https://codecov.io/gh/Tochemey/mig"><img alt="codecov" src="https://img.shields.io/codecov/c/github/Tochemey/mig/main"></a>
  <a href="LICENSE"><img alt="license" src="https://img.shields.io/github/license/Tochemey/mig"></a>
</p>

Kill a migration part way through, by SIGKILL, a pod eviction or a lost node, and the fix is to run it again. Before each step, mig asks Postgres whether the work is already there instead of trusting its own record of what ran, so the next run picks up from what the database actually holds and finishes the job.

mig is a command line tool for running migrations as a job, and a Go library, [pkg/mig](pkg/mig), for services that embed their migrations and verify them at startup. Both are one engine: every command is a thin wrapper over the library, so the two cannot drift apart.

## Contents

- [Premise](#premise)
- [Install](#install)
- [Quick start](#quick-start)
- [How it recovers](#how-it-recovers)
- [Migration format](#migration-format)
  - [Step kinds](#step-kinds)
  - [Inferred conditions](#inferred-conditions)
  - [Backfills](#backfills)
- [The command line](#the-command-line)
  - [Configuration](#configuration)
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

Before a step runs, mig asks the catalog what the actual state is and acts on the answer. The invalid index above is found, dropped and rebuilt, whatever state the database starts in, and the ledger is never permitted to authorize that decision. One invariant governs the whole design:

> Every step must converge to `succeeded` from an arbitrary, unknown starting state, using only the catalog and its own checkpoint.

## Install

The command line tool:

```sh
go install github.com/tochemey/mig/cmd/mig@latest
```

The library:

```sh
go get github.com/tochemey/mig
```

The container image, published on every release tag for linux amd64 and arm64.
The parser is compiled in, so running it needs no Go toolchain or C compiler:

```sh
docker run --rm \
  -v "$PWD/migrations:/migrations" \
  -e MIG_DSN="postgres://user:pass@db:5432/app?sslmode=disable" \
  ghcr.io/tochemey/mig:latest up --dir /migrations
```

Requirements for building from source:

- Go 1.26 or later, with cgo enabled. mig parses SQL with the real Postgres grammar through [pg_query_go](https://github.com/pganalyze/pg_query_go) instead of regular expressions, so quoted identifiers, embedded comments, partial indexes and multi-line statements are read the way the server reads them.
- Postgres. The test matrices run against 17 and 18 on every change.

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

Run it again, or kill the first run part way through and then run it again. Either way the next run does only what is missing, and a run with nothing to do displays nothing:

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

Every step goes through the same sequence, whatever happened before it:

1. Ask the catalog whether the work is already present. If it is, skip the step. The ledger is not consulted.
2. If the work is absent, read the ledger to find out whether an earlier attempt left the database part way through. An interrupted concurrent build leaves an invalid index, which is dropped before the rebuild.
3. Record the attempt, then run the statement. The attempt count is written first, so a crash during the statement leaves a trace.
4. Check the condition again. A step that reports success without changing the catalog fails instead of being recorded as done.

Two things make this safe when more than one runner starts:

- **A fenced lease.** One runner applies at a time and holds a lease carrying a monotonic fence token. Every ledger write re-asserts that token in the same transaction. A runner that was frozen past its expiry, by a GC pause or a stalled VM, finds its writes rejected when it wakes instead of overwriting its successor.
- **Transactional steps commit their ledger row with their DDL.** There is no window between the two, so nothing needs reconciling. This is the one case where the ledger decides whether a step is done.

## Migration format

Migrations are hand-written SQL files named `<version>_<name>.sql` and applied in version order. The version is the leading run of digits: a timestamp by convention, or a sequence number in a history adopted from another tool.

No annotation is mandatory. A plain `.sql` file with none at all is a valid migration: the whole file runs as one transactional step named `step_1`. Annotations come in when a step departs from that default — to split a file into steps, to leave the transaction, or to batch a data change.

An annotation is a comment line starting with `-- +mig `, so a migration file is still valid SQL. It applies to the step it appears in: `step:` starts the next step, and any other annotation before the first `step:` starts an implicit one. An annotation mig does not recognise is an error when the file loads, not a comment to skip, so a typo cannot silently drop an instruction.

| Annotation                                                    | Required                                                                                                                 | Effect                                                                                                      |
|---------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------|
| `-- +mig step: <name>`                                        | Never. A file with no `step:` lines is one step named `step_1`.                                                          | Starts a step. The SQL that follows belongs to it.                                                          |
| `-- +mig notx`                                                | On every step holding a statement Postgres refuses inside a transaction: `CREATE INDEX CONCURRENTLY`, `VALIDATE CONSTRAINT` and similar. Never inferred from the SQL. | Runs the step outside a transaction.                                                                        |
| `-- +mig backfill: table=T key=K [batch=N] [max_lag_bytes=N]` | For any batched data change. `table` and `key` are required; the bracketed settings are optional.                        | A resumable, batched data change of exactly one statement.                                                  |
| `-- +mig satisfied: sql(<expr>)`                              | On every backfill, and on a `notx` step whose condition cannot be inferred.                                              | Supply the done-condition yourself when it cannot be inferred. The expression must return a single boolean. |
| `-- +mig no_lock_timeout`                                     | Never.                                                                                                                   | Drop the default `lock_timeout` for this step.                                                              |

`notx` is the one requirement not caught before run time: mig never changes a step's kind from its SQL, so a `CREATE INDEX CONCURRENTLY` in a step without `notx` fails when Postgres refuses to run it inside the step's transaction. Everything else is checked as the files load, before anything is applied: a backfill missing `table=`, `key=` or `satisfied:`, a `satisfied:` in any form other than `sql(<expr>)`, a `notx` step with no condition, a step with no SQL, and a backfill holding more than one statement are all rejected by `mig plan` and at the start of `mig up`.

### Step kinds

Every step runs as one of three kinds. The names appear in `mig plan`, in the summary's `kind` field and in `mig status`:

| Kind       | How a step gets it          | What it means                                                                                                                                                                |
|------------|-----------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `ddl_tx`   | The default.                | Runs inside a transaction, and its ledger row commits in that same transaction. An interrupted attempt rolls back whole, so there is never anything to reconcile.            |
| `ddl_notx` | The `notx` annotation.      | Runs outside a transaction, for the statements Postgres refuses inside one. Judged against the catalog, and repaired first when an earlier attempt left partial work behind. |
| `backfill` | The `backfill:` annotation. | A batched data change. Each batch commits together with its cursor, so an interrupted run resumes from the last committed batch.                                             |

### Inferred conditions

You rarely write a `satisfied:` annotation. mig derives the condition from the statement:

| Statement                         | Judged done when                          |
|-----------------------------------|-------------------------------------------|
| `CREATE INDEX`                    | The index exists and is valid and ready.  |
| `DROP INDEX`                      | The index is gone.                        |
| `ALTER TABLE ADD COLUMN`          | The column exists.                        |
| `ALTER TABLE DROP COLUMN`         | The table exists and the column does not. |
| `ALTER TABLE ADD CONSTRAINT`      | The constraint exists.                    |
| `ALTER TABLE VALIDATE CONSTRAINT` | The constraint exists and is validated.   |
| `CREATE TABLE`                    | The relation exists.                      |
| `ALTER TYPE ADD VALUE`            | The label is present.                     |

A step counts as done only when every statement in it does. A non-transactional step whose condition cannot be inferred is rejected by `mig plan` and at the start of a run, before anything is applied.

### Backfills

A backfill names its table and key and marks the key range each batch covers:

```sql
-- +mig step: fill_email
-- +mig backfill: table=users key=id batch=5000
-- +mig satisfied: sql(SELECT NOT EXISTS (SELECT 1 FROM users WHERE email IS NULL))
UPDATE users SET email = legacy_email
 WHERE id > :cursor_lo AND id <= :cursor_hi AND email IS NULL;
```

mig substitutes `:cursor_lo` and `:cursor_hi` with each batch's key range. The low bound is exclusive and the high bound inclusive, so write the predicate as `key > :cursor_lo AND key <= :cursor_hi` and consecutive batches cannot overlap.

Each batch commits with the cursor that covers it, in one transaction. The rows and the cursor cannot land separately.

Pagination is by key range only. `OFFSET` rescans every row it skips, so its cost grows with the square of the table, and it misses rows when concurrent writes shift what it counts past.

Batch size adapts. It halves when replication lag passes `max_lag_bytes` or a batch runs longer than the target, and grows by a quarter when neither happens, staying between 100 and 50,000 rows. Batches run on a separate connection pool so a backfill cannot starve the heartbeat that holds the lease.

A backfill requires `satisfied:`. Its cursor lives in the ledger, and the ledger does not decide whether work remains.

## The command line

| Command           | What it does                                                                           |
|-------------------|----------------------------------------------------------------------------------------|
| `mig up`          | Applies every pending step, in order, under one lease.                                 |
| `mig plan`        | Prints each step and the condition it will be judged by. Opens no connection.          |
| `mig verify`      | Reports whether the database shows every migration. Takes no lease and writes nothing. |
| `mig status`      | Prints what the ledger records: state, attempts, checkpoint and last error.            |
| `mig import`      | Adopts a goose or golang-migrate history.                                              |
| `mig fingerprint` | Prints a canonical digest of the schema.                                               |

Run `mig <command> --help` for flags.

### Configuration

Every setting is a flag. Three of them also read an environment variable, which supplies the default when the flag is absent. An explicit flag always wins.

| Flag            | Environment     | Default      | Commands                                          | Meaning                                                                                                                                                                                                    |
|-----------------|-----------------|--------------|---------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `--dsn`         | `MIG_DSN`       | none         | `up`, `verify`, `status`, `import`, `fingerprint` | Postgres connection string. Required. The command fails before connecting if it is empty.                                                                                                                  |
| `--dir`         | `MIG_DIR`       | `migrations` | `up`, `plan`, `verify`, `import`                  | Directory holding the migration files.                                                                                                                                                                     |
| `--lease-ttl`   | `MIG_LEASE_TTL` | `30s`        | `up`, `import`                                    | How long the lease stays valid without a heartbeat. A value that does not parse as a Go duration is ignored and the default is used, so a typo cannot shorten a lease into one that expires mid-migration. |
| `--on-locked`   | none            | `wait`       | `up`                                              | `wait` blocks until the lease is free. `fail` exits with code 4 straight away.                                                                                                                             |
| `--allow-drift` | none            | `false`      | `up`                                              | Continue when a migration was edited after it was applied. Without it, a checksum mismatch on a succeeded step stops the run.                                                                              |
| `--verbose`     | none            | `false`      | `up`                                              | Structured JSON logs on stderr instead of the progress display, carrying the same records.                                                                                                                 |
| `--from`        | none            | none         | `import`                                          | Source history to adopt: `goose` or `golang-migrate`. Required.                                                                                                                                            |
| `--json`        | none            | `false`      | `status`                                          | Emit the report as JSON instead of a table.                                                                                                                                                                |
| `--describe`    | none            | `false`      | `fingerprint`                                     | Print the rows behind the digest instead of the digest.                                                                                                                                                    |

Example:

```sh
export MIG_DSN="postgres://user:pass@localhost:5432/app?sslmode=disable"
export MIG_DIR=db/migrations

mig up                       # uses both variables
mig up --dir db/hotfix       # the flag overrides MIG_DIR
```

Two values are fixed rather than configurable. A step waits at most 3 seconds for a lock, since a migration that blocks behind a long transaction should give way rather than queue behind it and hold up every writer after it. Use `-- +mig no_lock_timeout` on the step that needs to wait. A backfill without a `batch=` setting starts at 1000 rows and adapts from there.

### Exit codes

A scheduler needs to tell another runner holding the lease apart from a failed migration, so the codes are part of the interface:

| Code | Meaning                          |
|------|----------------------------------|
| `0`  | Converged.                       |
| `1`  | Failed.                          |
| `3`  | Work outstanding. `verify` only. |
| `4`  | Another runner holds the lease.  |

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
|------------------------------------------------|--------------------------------------------------------------------|
| `Up(ctx, db, fsys, opts)`                      | Applies every outstanding step under the lease.                    |
| `Verify(ctx, db, fsys)`                        | Returns an error wrapping `ErrPending` if anything is outstanding. |
| `Pending(ctx, db, fsys)`                       | The same result as a list, for a caller that reports it itself.    |
| `Plan(fsys)`                                   | What a run would check, without a database.                        |
| `Status(ctx, db)`                              | What the ledger records for every step.                            |
| `Import(ctx, db, fsys, source, opts)`          | Adopts another tool's history.                                     |
| `Fingerprint(ctx, db)` and `Describe(ctx, db)` | The schema digest, and the rows behind it.                         |

Sentinel errors: `ErrPending`, `ErrLocked`, `ErrFenced`, `ErrChecksumDrift` and `ErrUnknownSource`.

`Options` works as its zero value. `TTL` defaults to `DefaultTTL`, `OnLocked` to `Wait`, and `Work` to the pool passed in.

## Deploying

Run `mig up` as a job. Call `Verify` when the service starts.

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

The shape is the same for both: point mig at the existing directory, import the history once per database, and use `mig up` from then on. Existing files run unchanged, and new migrations can use steps, backfills and inferred conditions.

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
