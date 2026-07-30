<h1 align="center">mig</h1>

<p align="center">
  <a href="https://github.com/Tochemey/mig/actions/workflows/build.yml"><img alt="build" src="https://img.shields.io/github/actions/workflow/status/Tochemey/mig/build.yml?branch=main&label=build"></a>
  <a href="https://codecov.io/gh/Tochemey/mig"><img alt="codecov" src="https://img.shields.io/codecov/c/github/Tochemey/mig/main"></a>
  <a href="LICENSE"><img alt="license" src="https://img.shields.io/github/license/Tochemey/mig"></a>
</p>

Postgres migrations you can kill and re-run.

Kill a migration part way through, with SIGKILL, a pod eviction or a lost node, then run it again. It converges on the same result.

Before running a step, mig asks Postgres whether the work is already present. It does not rely on its own record of what ran. The catalog is the source of truth and the ledger is advisory.

It is one engine in two forms: a command line tool, for running migrations as a job, and a Go library, [pkg/mig](pkg/mig), for services that embed their migrations and verify them at startup. Every command is a thin wrapper over the library, so the two cannot drift apart.

## Contents

- [Why](#why)
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
- [Limitations](#limitations)
- [Contributing](#contributing)

## Why

Most migration tools record what they did in a table of their own. That record can disagree with the database, and when it does the tool has no way to tell which of the two is correct.

`CREATE INDEX CONCURRENTLY` shows the problem. If the build is interrupted, Postgres leaves the index in place with `indisvalid = false` and will not resume it. A tool that trusts its own record either finds no entry and reruns the statement, which fails on the duplicate name, or finds a success entry and skips it, leaving an index the planner never uses.

Reading the catalog answers this without ambiguity: the index exists, it is not valid, so drop it and rebuild. mig makes that check before every step, from whatever state the database is in.

## Install

The command line tool:

```sh
go install github.com/tochemey/mig/cmd/mig@latest
```

The library:

```sh
go get github.com/tochemey/mig
```

Requirements for both:

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

Two words carry the design, and this README uses them throughout:

- The **catalog** is Postgres's own account of what exists: `pg_class`, `pg_index`, `pg_attribute` and the rest of the system tables. An index or a column is real exactly when the catalog says it is.
- The **ledger** is mig's own bookkeeping: a schema named `mig`, created on the first run, holding each step's attempts, status and checkpoint, plus the lease. It records what mig did, which is not the same as what is true.

When the two disagree, the catalog wins. The ledger advises: it says where to look and what may need repair, and only one narrow case trusts it outright.

This is what that looks like. Kill a run during a concurrent index build, then run it again:

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

Annotations are SQL comments, so a migration file is still valid SQL:

| Annotation                                                    | Effect                                                                                                      |
|---------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------|
| `-- +mig step: <name>`                                        | Starts a step. SQL before the first one becomes a step named `step_1`.                                      |
| `-- +mig notx`                                                | Run outside a transaction, for `CREATE INDEX CONCURRENTLY`, `VALIDATE CONSTRAINT` and similar.              |
| `-- +mig backfill: table=T key=K [batch=N] [max_lag_bytes=N]` | A resumable, batched data change.                                                                           |
| `-- +mig satisfied: sql(<expr>)`                              | Supply the done-condition yourself when it cannot be inferred. The expression must return a single boolean. |
| `-- +mig no_lock_timeout`                                     | Drop the default `lock_timeout` for this step.                                                              |

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

## Limitations

- **Down migrations are not run.** A rollback cannot reverse a destructive change, and running one against a schema that a failed `up` left half applied is guesswork. Re-running a converging `up` is the recovery path. A CI check that applies up, down and up again against a throwaway database and compares schema fingerprints is planned instead.
- **Transaction poolers are refused.** Session state on a pooled handle is meaningless and every step needs a pinned connection. mig detects PgBouncer in transaction mode and stops, instead of working around it and failing later in a way that is hard to diagnose.

## Contributing

Fork, branch, and open a pull request. The project follows [Semantic Versioning](https://semver.org) and [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/). [CONTRIBUTING.md](CONTRIBUTING.md) covers the development setup, the commit format, the code conventions and what the tests expect.
