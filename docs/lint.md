# The lock linter

This document records how `mig lint` works and the invariants any change to it must preserve. The [README](../README.md) explains how to use it; this document explains why it is shaped the way it is, and what a maintainer has to know before touching a rule, the lock model, or the fix engine. Read [design.md](design.md) first: the linter's output is expressed in the executor's step format, and that coupling is the point.

## Contents

- [Premise](#premise)
- [Scope](#scope)
- [Architecture](#architecture)
- [Operating modes](#operating-modes)
- [The lock model](#the-lock-model)
  - [Lock modes](#lock-modes)
  - [Duration classes](#duration-classes)
  - [Implicit relations](#implicit-relations)
  - [Version sensitivity](#version-sensitivity)
- [Severity](#severity)
  - [Size thresholds](#size-thresholds)
  - [Duration estimates](#duration-estimates)
  - [The empty-table suppression](#the-empty-table-suppression)
- [The rule catalog](#the-rule-catalog)
- [Autofix](#autofix)
- [The policy file](#the-policy-file)
- [Suppressions](#suppressions)
- [Output formats](#output-formats)
- [The chaos harness](#the-chaos-harness)
- [Adding a rule](#adding-a-rule)
- [Testing](#testing)
- [Review checklist](#review-checklist)

## Premise

Other linters analyse statements and emit warnings. This one differs in three ways, and those three are the whole justification for it existing.

**It reports lock mode, duration class and blast radius, not a warning string.** "This takes `ACCESS EXCLUSIVE` on a 340 GB table for an estimated 45 minutes and blocks all reads" is a fact somebody can act on. "Consider using CONCURRENTLY" is not.

**Its fixes are executable steps, not prose.** A flagged `ADD COLUMN ... DEFAULT volatile()` becomes an expand-contract migration in mig's own step format, and the executor is the oracle that proves the rewrite lands on the same schema.

**It can prove itself.** `mig lint verify` applies the migration to a throwaway database under synthetic load and measures the p99 latency and the blocked-query time it actually caused. Static analysis predicts; this measures.

One rule governs everything below, because it is what the tool's usefulness rests on:

> A linter that cries wolf gets disabled, and then it protects nothing.

That is why severity scales with table size, why no size-dependent hazard is reported against a table the same migration creates, and why a suppression must give a reason instead of being forbidden.

## Scope

The linter deliberately does not do these:

- General SQL style: naming, formatting, `SELECT *`. Out of scope permanently.
- Schema design opinions: normalisation, index strategy.
- Dialects other than Postgres.
- Anything requiring it to understand application code. Where a hazard is only a hazard because of what the application does, the rule says so and stops.

## Architecture

```
internal/lint/            the engine: walks the plan, runs the catalog, sorts and filters
internal/lint/lockmodel/  statement -> {relations, lock modes, duration class, notx}
internal/lint/rules/      the rule catalog, one file per rule
internal/lint/stats/      live catalog statistics and the calibration probe
internal/lint/fix/        fix builders, emitting mig steps as parse trees
internal/lint/policy/     the policy file and the suppression directives
internal/lint/report/     human, JSON, SARIF, markdown and audit renderers
internal/lint/verify/     the chaos harness: workload, latency, wait sampling, budget
test/lockmatrix/          the lock model held against a live server
test/flipmatrix/          version-conditional rules held against every supported major
test/estimate/            duration estimates held against a real rewrite
test/autofix/             fixes held against the executor's fingerprint oracle
test/chaos/               the harness held against a known-bad and a known-good migration
```

Three dependency rules hold the design together, and a change that breaks one is wrong:

**No regex against SQL, ever.** Every inspection goes through the `pg_query_go` parse tree, the same v6 parser and the same real Postgres grammar the executor loads migrations with. Text matching produces false positives on comments, string literals and dollar-quoted bodies, and cannot see through `ALTER TABLE ... , ...` multi-action statements. Where a rule has to search a whole tree, it walks it through protoreflect and names the node type through the generated Go type, so a change in the grammar's shape is a compile error rather than a rule that quietly stops matching.

**The linter reuses `internal/plan`, it does not fork it.** A migration is linted exactly as it will be executed: same loader, same step splitting, same annotations. The two recognisers the linter needs on raw lines, `plan.StepOf` and `plan.LintIgnoreOf`, are exported from the loader rather than written a second time.

**Every rule degrades gracefully offline.** Offline is the pre-commit hook and the default. A rule that needs the catalog stays silent without it rather than guessing.

## Operating modes

| Mode                 | Input                     | What it adds                                                                                                                                                                      |
|----------------------|---------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Offline, the default | SQL files only            | Statement classification, lock modes, duration classes, structural and cross-statement hazards. Every size-dependent hazard stays a warning, because nothing here knows the size. |
| Connected, `--dsn`   | SQL plus the live catalog | Table sizes and row estimates, size-scaled severity, duration estimates, the real primary key for generated backfills, and the server's own major version.                        |

Connected mode reads four things:

- `GREATEST(reltuples, 0)` and `pg_total_relation_size(oid)` per relation the plan names. `reltuples` is -1 on a table nobody has analysed, which is reported as a size on disk with no row count rather than as zero rows.
- The primary key columns from `pg_constraint`, used to page a generated backfill by the real key instead of an assumed `id`.
- `SHOW server_version_num`, which becomes the target version and overrides `--target-version`.
- A calibration probe, described under [Duration estimates](#duration-estimates).

Nothing in the schema is changed: the catalog reads are reads, and the calibration probe's one write is rolled back. It is still real work on a real server, so point connected mode at a production-shaped replica or a restored snapshot rather than at production.

## The lock model

`internal/lint/lockmodel` is the core asset; everything else is presentation. For each statement it produces:

```go
type LockEffect struct {
    Relation RelationRef   // schema-qualified
    Mode     LockMode      // ACCESS EXCLUSIVE, SHARE ROW EXCLUSIVE, ...
    Duration DurationClass // Instant | Scan | Rewrite | IndexBuild
    Implicit bool          // acquired indirectly
    Reason   string        // "table rewrite: column type change"
}
```

### Lock modes

| Mode                     | Blocks reads | Blocks writes | Typical source                                                            |
|--------------------------|--------------|---------------|---------------------------------------------------------------------------|
| `ACCESS SHARE`           | no           | no            | `SELECT`                                                                  |
| `ROW SHARE`              | no           | no            | `SELECT FOR UPDATE`                                                       |
| `ROW EXCLUSIVE`          | no           | no            | `INSERT`, `UPDATE`, `DELETE`                                              |
| `SHARE UPDATE EXCLUSIVE` | no           | no            | `CREATE INDEX CONCURRENTLY`, `VALIDATE CONSTRAINT`, `VACUUM`              |
| `SHARE`                  | no           | yes           | `CREATE INDEX` without `CONCURRENTLY`                                     |
| `SHARE ROW EXCLUSIVE`    | no           | yes           | `ADD FOREIGN KEY`, on both tables                                         |
| `EXCLUSIVE`              | mostly       | yes           | `REFRESH MATERIALIZED VIEW CONCURRENTLY`                                  |
| `ACCESS EXCLUSIVE`       | yes          | yes           | most `ALTER TABLE`, `DROP`, `TRUNCATE`, plain `REFRESH MATERIALIZED VIEW` |

### Duration classes

The mode alone is not the hazard. Mode multiplied by duration is.

- **Instant**: catalog work only. `ADD COLUMN` with no default, `SET DEFAULT`, `RENAME`. `ACCESS EXCLUSIVE` held for microseconds.
- **Scan**: one pass over the rows. `VALIDATE CONSTRAINT`, `SET NOT NULL` with no proving check.
- **Rewrite**: a full copy of the table, plus the disk for it. Type changes, `ADD COLUMN` with a volatile default, `VACUUM FULL`, `CLUSTER`.
- **IndexBuild**: O(rows log rows). A concurrent build takes two passes and waits out open transactions.

`ACCESS EXCLUSIVE` with Instant is routine. `ACCESS EXCLUSIVE` with Rewrite is an outage. A linter that calls both an "ALTER TABLE warning" gets muted inside a week.

### Implicit relations

These are resolved, because they are wrong in exactly the cases that matter most:

- `ADD FOREIGN KEY` locks the referenced table as well.
- `DROP TABLE` locks the tables whose foreign keys point at it.
- DDL on a table locks dependent views and materialized views.
- `ALTER COLUMN TYPE` rebuilds every index on the column and revalidates dependent constraints.
- Adding an inline `UNIQUE` or `PRIMARY KEY` builds an index under the table's existing `ACCESS EXCLUSIVE`.

`ALTER COLUMN TYPE` is predicted as a rewrite in every case. Ruling out the cases that do not rewrite, typmod widening and binary-coercible changes, needs the column's current type, and the lock model takes no catalog input: it reads a parse tree and a target version, and nothing else.

### Version sensitivity

The target version comes from `--target-version`, from the policy file, or in connected mode from the server. Behaviours that flip:

| Change                                                                   | Version |
|--------------------------------------------------------------------------|---------|
| `ADD COLUMN` with a non-volatile `DEFAULT` no longer rewrites            | 11      |
| `SET NOT NULL` can skip its scan given a valid `CHECK (col IS NOT NULL)` | 12      |
| `REINDEX CONCURRENTLY` available                                         | 12      |
| `DETACH PARTITION CONCURRENTLY` available                                | 14      |

The matrix is validated against real containers rather than transcribed from documentation into Go constants. `test/lockmatrix` runs each statement against a seeded table and reads back what actually happened: locks from `pg_locks`, where a waiting request is observed the same way as a granted one, so a `notx` statement is caught mid-acquisition behind a guard lock; rewrites from `pg_class.relfilenode` changing; scan-versus-instant from the server's own `client_min_messages=debug1` account of which relations it visited; and the transaction refusal by running the statement inside a transaction block and expecting SQLSTATE 25001. `test/flipmatrix` runs the version-conditional rules against every supported major and asserts the rule flips where the server does.

The container corrected the model twice before it shipped, which is the argument for having it:

- `REFRESH MATERIALIZED VIEW CONCURRENTLY` runs inside a transaction block. The model predicted a refusal; the server accepted it.
- `ADD CONSTRAINT ... PRIMARY KEY USING INDEX` locks the adopted index with `SHARE UPDATE EXCLUSIVE`, not `ACCESS EXCLUSIVE`.

Hardcoded version knowledge rots silently and turns the linter into a liability. Treat the matrix as test output, not as a table to edit.

The matrix covers what a container can observe. Two version-conditional behaviours are outside that, and a maintainer reading the matrix should know which: `L005`'s target-12 gate, and the enum-value transaction refusal before 12. Neither leaves a trace in `pg_locks` or in `relfilenode`, and both are sourced from the Postgres documentation instead.

## Severity

Three levels: `info` is worth knowing, `warn` is worth reading, `error` fails the run. `mig lint` exits non-zero only when a finding is an error.

Hazards divide in two:

- **Size-independent** hazards are errors always, because they are wrong at any scale: `CONCURRENTLY` inside a transaction, an enum value used in the transaction that added it, `TRUNCATE`, a rename, a blocking step with the lock timeout turned off.
- **Size-dependent** hazards are graded by the table. Offline they are warnings, because nothing there knows whether the table holds twelve rows or forty million.

### Size thresholds

Connected, the catalog decides:

| Table                                                  | Grade   |
|--------------------------------------------------------|---------|
| At or above 1,000,000 rows, or at or above 1 GB        | `error` |
| Below 10,000 rows and below 8 MB                       | `info`  |
| Anything between, or a table the catalog does not have | `warn`  |

The upper pair is the design's: a million rows or a gigabyte is where a rewrite stops being a deploy and starts being an outage. The lower pair exists so the lookup tables every schema has do not train the reader to ignore warnings. All four move from the [policy file](#the-policy-file).

The thresholds reach the rules through `rules.Context.Thresholds`. A field left at zero falls back to the package's own default, so a policy naming one threshold does not silently reset the other three. No rule reads the underlying constants directly, and a change that adds one is a change that lets a policy be ignored.

### Duration estimates

Connected findings carry an estimate:

```
L004  users  changing a column type rewrites the table
      ACCESS EXCLUSIVE on users (41.2M rows, 340.0 GB); blocks reads and writes
      estimated 38m to 55m, from this server's measured throughput; a cold or busy table is slower
```

The throughput comes from a calibration probe run once per connected run, on one pinned connection, inside a transaction that is rolled back:

```sql
BEGIN;
CREATE TABLE mig_lint_calibration (id bigint, pad text);
INSERT INTO mig_lint_calibration SELECT ... 200000 rows ...;
SELECT pg_total_relation_size('mig_lint_calibration');
ALTER TABLE mig_lint_calibration ADD COLUMN probe float8 DEFAULT random();  -- timed: rewrite
CREATE INDEX mig_lint_calibration_id ON mig_lint_calibration (id);          -- timed: index build
ROLLBACK;
```

Three things about it are load-bearing, and each was a measured error before it was a decision:

- It builds a **real table, rolled back**, not a temporary one. A temporary table is unlogged and local, and made the estimates three times too optimistic.
- It measures **both bytes per second and rows per second**, and `Estimate` takes whichever cost binds. Bytes alone was still 2.8 times optimistic on narrow rows, because a table of many small rows costs far more than its size suggests. With both, the error fell to about 1.3 times, inside the design's requirement of 2.
- An estimate under a second is **not reported**. "Estimated 0s to 0s" teaches a reader to skip the rest of them.

A probe that fails does not fail the run. A read-only standby refuses the write, and the report says so on its own line: the findings and the sizes are all still there, only the estimates are missing. `LintReport.Uncalibrated` carries the server's reason verbatim.

`test/estimate` is the acceptance: a 10-million-row table, a real rewrite, and the prediction held to within a factor of two of the measurement. It runs behind `MIG_ESTIMATE=1` in its own CI step, because it is a timing measurement and competing with every other package's containers makes it measure the load instead of the server.

### The empty-table suppression

No hazard whose cost is O(rows) is reported against a table the same migration creates before the statement runs, because that table is empty at that point. This is the credibility rule applied: create-table-then-index in one file is the single most common migration shape there is, and reporting it would train every reader to ignore the tool.

Only creations **before** the statement count, down to the position within the step. A creation afterwards proves nothing: either the statement fails, or the migration is recreating a table that holds rows right now.

## The rule catalog

Rule IDs are stable and never reused. `internal/lint/rules/rules.go` holds the ID constants and the one description of each, which is what the SARIF renderer hands a code-scanning UI and what tells a policy or a suppression naming a real rule from one naming a typo.

Severity below is the offline grade. Where it reads "sized", the rule is graded by the table under [Size thresholds](#size-thresholds) and is a warning offline.

### Rewrites and long locks

| ID     | Detects                                            | Severity | Fix      |
|--------|----------------------------------------------------|----------|----------|
| `L001` | `CREATE INDEX` without `CONCURRENTLY`              | sized    | no       |
| `L002` | `CONCURRENTLY` inside a transactional step         | error    | no       |
| `L003` | `ADD COLUMN` whose default rewrites the table      | sized    | yes      |
| `L004` | `ALTER COLUMN TYPE` causing a rewrite              | sized    | scaffold |
| `L005` | `SET NOT NULL` without a proving validated `CHECK` | sized    | yes      |
| `L006` | `ADD FOREIGN KEY` without `NOT VALID`              | sized    | yes      |
| `L007` | `ADD CHECK` without `NOT VALID`                    | sized    | yes      |
| `L008` | `ADD PRIMARY KEY` without `USING INDEX`            | sized    | yes      |
| `L009` | Inline `UNIQUE` in `ADD COLUMN`                    | warn     | no       |
| `L010` | `VACUUM FULL` or `CLUSTER` in a migration          | error    | no       |
| `L011` | `REFRESH MATERIALIZED VIEW` without `CONCURRENTLY` | warn     | no       |

`L003` classifies each `ADD COLUMN` action in isolation through `lockmodel.AnalyzeAddColumn`. Blaming `ADD COLUMN` for any rewrite in the statement reported the innocent column in `ADD COLUMN a int, ALTER COLUMN b TYPE bigint`.

`L005` accepts the safe pattern only when the proving `CHECK` is on the same table, matched by bare name. Two same-named tables in different schemas in one migration are beyond what the offline pass can settle.

### Transaction and ordering hazards

These read the whole step, and in some cases the whole migration. A transactional step holds its locks until the last statement commits, so the hazard is often not in any one statement but in the company it keeps.

| ID     | Detects                                                   | Severity | Fix |
|--------|-----------------------------------------------------------|----------|-----|
| `L020` | Several `ACCESS EXCLUSIVE` statements in one transaction  | warn     | no  |
| `L021` | Row work sharing a transaction with other DDL             | warn     | no  |
| `L022` | An index built before the backfill that fills it          | info     | no  |
| `L023` | A foreign key added before the backfill that populates it | warn     | no  |
| `L024` | An enum value used in the transaction that added it       | error    | no  |
| `L025` | A blocking step with the lock timeout turned off          | error    | no  |

`L020` requires two statements **and** two relations. `CLUSTER users USING users_pkey` names one table twice, and reporting it as two locks held for the sum was a false positive caught by another rule's golden fixture.

`L021` counts only statements whose own locks stop traffic, at `SHARE` or above. Counting a `SELECT` as company reported "alongside 1 other statement" for a read whose `ACCESS SHARE` conflicts with almost nothing.

`L024` finds a label added earlier in the same step and used later, by walking the parse tree for the literal rather than matching text.

### Application-coupling hazards

| ID     | Detects                                                   | Severity | Fix                             |
|--------|-----------------------------------------------------------|----------|---------------------------------|
| `L030` | A column or table dropped out from under the application  | warn     | no, the remedy is a deploy gate |
| `L031` | A table or column renamed, which no deploy order survives | error    | no                              |
| `L032` | A table created and never granted to the application role | warn     | no                              |
| `L033` | `TRUNCATE` in a migration                                 | error    | no                              |

`L032` earns its place: a table created by the migration role is invisible to a restricted application role, and the failure surfaces in production rather than in CI. The rule reads the grants **after** the creation, because `GRANT ON ALL TABLES IN SCHEMA` covers the tables that exist when it runs, so one written above the `CREATE` does not reach it. A table the same migration drops before it ends needs no grant at all.

### Data-volume hazards

| ID     | Detects                                                         | Severity              | Fix |
|--------|-----------------------------------------------------------------|-----------------------|-----|
| `L040` | An `UPDATE` or `DELETE` over a whole table in one transaction   | sized                 | no  |
| `L041` | A `DELETE` over a large table, for the bloat it leaves          | sized, connected only | no  |
| `L042` | A concurrent index build reconciled by a hand-written predicate | info                  | yes |

`L040` fires on an `UPDATE` or `DELETE` with no `WHERE` clause at all, and not on every write lacking a key-range predicate. Nothing offline can tell how many rows a predicate matches, and warning about every predicated write is how a linter teaches its reader to skip warnings. `L041` covers the large-table half, and stays silent offline because "large" is not something the offline pass knows.

`L042` exists because `plan.Step.Check` prefers the author's `satisfied:` over the inferred predicate, and the inferred one is the only place the invalid-index case is handled. An interrupted `CREATE INDEX CONCURRENTLY` leaves an index that exists and is invalid, which the planner ignores and every write to the table still maintains. A hand-written predicate asking only whether the index is there reports that wreckage as done, and the next run skips the rebuild. The rule stays silent when the predicate names `indisvalid`, and its fix is to drop the annotation and let inference do the work.

### L000

`L000` is not a schema hazard. It reports a lint directive the linter cannot honour: one naming no rule, one naming a rule this build does not have, or one giving no reason. It is an error, it is added after the suppressions have been applied so a broken suppression cannot silence the complaint about itself, and it can be neither graded by a policy nor named in a directive. Both attempts are refused with a message rather than accepted and ignored.

## Autofix

`mig lint --fix` shows a diff and applies it once confirmed. `--fix --yes` skips the question, for a CI branch. `--fix` renders a diff and takes no `--format`, and does not report suppressions; both combinations are refused rather than silently ignored.

Fixes are built as parse trees and deparsed by `pg_query_go`, so identifier quoting is the server's own and nothing is assembled by string concatenation. Every builder leaves the caller's tree untouched, which the fix tests assert directly.

| Builder                | Emits                                                                                                           |
|------------------------|-----------------------------------------------------------------------------------------------------------------|
| `AddColumnWithDefault` | Nullable column, `SET DEFAULT`, batched backfill, `NOT VALID` check, `VALIDATE`, `SET NOT NULL`, drop the check |
| `NotNullPattern`       | `NOT VALID` check, `VALIDATE`, `SET NOT NULL`, drop the check                                                   |
| `ForeignKeyTwoStep`    | `ADD CONSTRAINT ... NOT VALID`, then `VALIDATE CONSTRAINT` in a `notx` step                                     |
| `CheckTwoStep`         | The same two steps for a check constraint                                                                       |
| `PrimaryKeyViaIndex`   | `CREATE UNIQUE INDEX CONCURRENTLY` in a `notx` step, then `ADD CONSTRAINT ... PRIMARY KEY USING INDEX`          |
| `WithoutSatisfied`     | The same step with the `satisfied:` annotation removed                                                          |
| `TypeChangeScaffold`   | The expand-contract plan, fully commented out, with a `TODO` naming the application change                      |

Rules the engine holds every fix to:

- **Never rewrite silently.** The diff is printed and confirmed before anything is written.
- **Every generated step carries a comment saying why it exists.** The moment the output looks like machine noise, people stop reading it and the reviewability advantage is gone.
- **Refuse partial fixes.** Where a safe rewrite cannot be completed without a change the linter cannot make, the fix is a scaffold: fully commented out, inserted above the statement rather than replacing it, and marked `FixScaffold` so nothing treats it as executable.
- **A fix replaces its whole step.** A statement sharing a step with others keeps its finding and loses its fix, because splitting the step is the author's call. One replacement per statement: a second finding on the same span keeps its say in the report, not in the file.
- **A suppressed finding carries no fix**, because it is not in the report at all. Silencing a rule and having its rewrite applied anyway would be the worst of both.

The acceptance is the executor, not an eyeball. `test/autofix` runs the unsafe statement and its fix against two fresh clones of the same seeded database and requires an identical schema fingerprint, and requires that the fix lints clean, so the linter cannot recommend statements it would then flag. An autofix producing a different schema than the statement it replaced is worse than no autofix.

### Which rules carry a fix

A fix is the replacement of one statement's step, so what a rule can be given follows from the shape of the rule:

- **A rule firing on a single statement can carry an executable fix.** `L003`, `L005`, `L006`, `L007`, `L008` and `L042` do.
- **A rule whose remedy the linter cannot complete carries a scaffold.** `L004` does: swapping a column's type safely needs the application to dual-write across a deploy boundary, so the fix is the plan, commented out, with a `TODO` naming that change.
- **A rule firing on the shape of a whole step carries no fix.** The cross-statement rules, `L020` to `L025`, fire on steps holding several statements by definition, and their remedies are splitting a step and reordering two steps: edits to a step list rather than to a statement.
- **A rule whose remedy needs facts the offline pass does not have carries no fix.** `L031` needs the old column's type to declare the new one, and `L032` needs the name of the application role. Both rules say what to do instead.
- **A rule with no safe rewrite carries none.** `VACUUM FULL`, `CLUSTER` and `TRUNCATE` in a migration are refusals, not shapes to improve.

Applying a fix replaces the step from its `step:` line down, so a `lint:ignore` directive for a different rule inside that step goes with it. The diff shows the removal, and the rule the directive silenced fires again on the next run, so it is visible rather than silent.

## The policy file

`.miglint.yaml`, found beside the command unless `--policy` names another path. It is optional: without it the catalog's own defaults apply. A file named explicitly and not found is an error, because a run that silently ignores the policy it was pointed at reports the wrong severities.

```yaml
target_version: 15

thresholds:
  big_rows: 5000000
  big_bytes: 10737418240
  small_rows: 1000
  small_bytes: 1048576

rules:
  L004: error
  L022: off

overrides:
  - path: services/legacy/migrations
    rules:
      L003: off
      L004: warn
```

| Key              | Effect                                                                                                                                                                       |
|------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `target_version` | The Postgres major to lint against, when the command line does not name one.                                                                                                 |
| `thresholds`     | Moves the four size thresholds. Plain row counts and plain bytes: a number that means what it says beats a suffix the reader has to trust. A key left out keeps its default. |
| `rules`          | Assigns a severity per rule: `off`, `info`, `warn` or `error`. A rule set to `off` produces no findings at all.                                                              |
| `overrides`      | Re-assigns severities for one migration directory. `path` is required.                                                                                                       |

Decisions worth knowing before changing this:

- **An unknown field is an error**, for the same reason an unknown annotation is: a misspelled setting the author believes is in force is worse than no setting at all.
- **Every override is validated, and only the matching ones are applied.** A misspelled rule under a directory this run is not linting is still a mistake, and finding it now beats finding it on the run that needed it.
- **Overrides are keyed by the directory being linted**, not by the file a finding sits in. A migration source has no subdirectories, so the only useful reading is the monorepo one: one policy at the repository root, and a different severity for `services/legacy/migrations`. An override matches when the linted directory is the path or sits inside it, and the last matching override wins.
- **The policy is applied after grading**, so `L004: error` gives up the size scaling for that rule. That is what assigning a severity means.
- **Precedence for the target version** is the command line, then the policy, then the default. Connected mode ignores all three and uses the server's.

## Suppressions

```sql
-- +mig lint:ignore L004 reason="table has 12 rows, verified 2026-07-29"
```

The directive is an annotation, and that spelling is load-bearing: annotations are no part of a step's SQL, so silencing a rule on a migration that has already been applied leaves its checksum alone. A plain comment would change it, and the executor would report drift. The loader accepts the annotation and does nothing with it.

- **A directive addresses the step it sits in**, or the whole file when it stands above the first `step:` line. The loader skips it before opening a step, so a file-level directive does not open an empty step in front of the first real one.
- **The reason is mandatory.** A suppression nobody had to justify is one nobody can audit. A directive without one, or naming a rule this build does not have, is an `L000` error and silences nothing.
- **A directive written with tabs is the same directive.** The recogniser accepts any whitespace after `lint:ignore`, and the rule and the reason are read the same way.
- **Two directives covering one finding both count as used**, so neither is reported as dead weight it is not.

The linter reads directives out of the file text rather than out of the loaded plan, because an audit of suppressions is an audit of lines: it has to say where each one sits. `--report-suppressions` prints them:

```
FILE                      LINE  RULE  AGE   STATE   REASON
20240817120000_widen.sql  2     L001  714d  used    users has twelve rows, verified 2026-07-29
20240817120000_widen.sql  6     L033  714d  unused  left over from a rewrite that never landed
```

`unused` means the directive silenced nothing on this run: the statement it was written for is gone, or the rule no longer fires on it. `broken` marks one the linter cannot honour.

**Age is the migration's**, read from the timestamp in its file name. A directive added to an old migration long after the fact reads older than it is, and one in a history adopted from another tool, where the version is a sequence number rather than a timestamp, has no age at all and shows `-`. The accurate alternative is blaming the line in git, which buys precision with a dependency on a git checkout the linter otherwise does not need.

`--report-suppressions` renders with `--format human` and is refused with the others, since the audit is a table and the other formats are documents with their own shape.

## Output formats

| `--format`           | Use                                                                                                                                                            |
|----------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `human`, the default | A terminal. Position, severity, rule and message, the offending line with a caret under it, the lock detail, the estimate, and whether a rewrite is available. |
| `json`               | Scripting. One object with a `findings` array. No findings renders an empty list rather than null, so a consumer can iterate without a nil check.              |
| `sarif`              | GitHub code scanning. A SARIF 2.1.0 log.                                                                                                                       |
| `github`             | One pull-request comment, as a markdown table.                                                                                                                 |

Spans are byte offsets found by scanning the file forward statement by statement, so a repeated statement lands on its own occurrence. A statement whose text was rewritten after loading, which is what a backfill's cursor placeholders go through, gets a zero span and is reported without a line.

The SARIF renderer is handed the migration directory and joins it onto each finding's file name, because a finding names its file relative to that directory and an annotation anchored anywhere else lands on no diff at all. Each alert carries the message, the lock detail and the estimate, since those are what a reviewer decides on. `level` maps `info` to SARIF's `note`, `warn` to `warning` and `error` to `error`. A finding the engine could not place is reported against the file with no region rather than against line zero.

The markdown comment is a summary rather than a list of annotations, and a clean run renders "No lock hazards found." rather than nothing, because the comment replaces the previous one and silence would leave a stale verdict standing. Cell text is escaped: a pipe would end the column early and a newline the row.

## The chaos harness

### Why it exists

The rule catalog is a model of the server, and the lock matrix keeps that model honest about what a statement does. Neither can say what a statement will cost **here**, because the cost is not a property of the statement:

- **The lock is granted or it queues, and which one depends on the traffic.** `ALTER TABLE users ADD COLUMN c text` takes `ACCESS EXCLUSIVE` for microseconds and is invisible on an idle table. The same statement behind a thirty-second reporting query waits thirty seconds holding a lock request, and Postgres grants locks in order, so every query arriving in the meantime queues behind the DDL, including the ones that would never have conflicted with it. That is the outage mechanism, and it is nowhere in the SQL.
- **The lock timeout turns a slow migration into a failed one.** The executor gives way after three seconds rather than queueing, which is the right behaviour and also means a migration can be correct, well written, and still not apply under load. Only running it under load says which.
- **A prediction is a number nobody can check.** "Estimated 38 to 55 minutes" is a claim. Under a workload with a budget it becomes a pass or a fail, which is what a CI job can act on and what a review can trust.

So the harness closes the loop: the rules say what should hurt, and the harness measures whether it did. `test/chaos` runs a migration the rules flag under `L025` and requires the harness to catch it, and a concurrent index build the rules pass and requires the harness to pass it. When those two disagree, one of them is wrong and the disagreement is the finding.

The harness also reaches failures no other suite can. The kill matrices kill the process; this cancels a statement while the process lives, which is how it found the executor retrying a timed-out `CREATE INDEX CONCURRENTLY` without repairing the invalid index it left behind.

### What it does

`mig lint verify` applies the migrations to a database of its own making, under traffic, and measures what that did to the traffic.

```sh
mig lint verify --dsn "$SCRATCH_SERVER" \
                --dir migrations \
                --workload workload.yaml \
                --budget p99=50ms,max_block=2s
```

`--dsn` names a **server**, not a database to migrate. A throwaway database named `mig_verify_<random>` is created on it, used, and dropped, so nothing anybody owns is touched. `--keep` leaves it behind to look at what the migration left.

**It does not use testcontainers.** Linking a container runtime into the binary would roughly double a 26 MB CLI and make one command require a Docker socket. Naming a scratch server and building a throwaway database on it keeps the safety property, that nothing anybody owns is touched, without the dependency. The tests still provision by container through `test/harness`. Any change that puts testcontainers in the binary's dependency graph is wrong, and `go list -deps ./cmd/mig` is how to check.

### The workload file

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

| Key         | Required               | Meaning                                                                                                   |
|-------------|------------------------|-----------------------------------------------------------------------------------------------------------|
| `setup`     | yes                    | Builds the schema and rows the migration runs against.                                                    |
| `keys`      | when a query binds one | The key space `$1` is drawn from.                                                                         |
| `queries`   | yes                    | The fast traffic. Each needs `name` and `sql`; `key` binds `$1`, `rate` is per second and defaults to 50. |
| `slow_read` | yes                    | The long-running reader. `every` defaults to 2s.                                                          |
| `baseline`  | no                     | How long to measure before the migration. Defaults to 30s.                                                |
| `settle`    | no                     | How long to keep measuring after it. Defaults to 30s.                                                     |

**A workload without `slow_read` is refused.** The catastrophe a migration causes is rarely the DDL itself: it is the DDL queueing behind a long read and, because Postgres grants locks in order, blocking everything that arrives afterwards. Without a slow reader the lock-queue hazards never reproduce and the harness reports all clear on the migrations that matter most. This is a review-checklist item, and it is enforced in code.

The workload and the migration run on **separate connection pools**. Pool exhaustion masquerades as lock contention and produces misleading attribution.

### Measurement and the budget

Two measurements, from two sides:

- **Client-side latency**, per query class, as exact percentiles by nearest rank: p50, p99, p999 and the maximum, before and during.
- **Server-side wait attribution**, sampled from `pg_stat_activity` joined to `pg_locks` at roughly 50 ms intervals, which turns "queries got slow" into "queries waited on `Lock:relation`".

`--budget` takes `p50`, `p99`, `p999` and `max_block`, as `name=duration` pairs separated by commas. A term left out is not checked. Exceeding any term exits non-zero, which is what makes this a gate rather than a report.

```
BASELINE   p50 0.8ms     p99 2.1ms     max 14ms      (41203 queries)
DURING     p50 0.9ms     p99 4310ms    max 38.2s     (12841 queries)   <- FAIL

Migration took 38.4s
Blocked readings: 1284 of 41203
Longest block:    38.2s on users
Wait attribution: Lock:relation 99.2%, IO:DataFileRead 0.5%
FAIL p99 reached 4310ms, budget 50ms
```

Four properties of the harness, each of which a change has to preserve:

- **The long reader is measured but kept out of the percentiles.** It is the instrument: slow on purpose, and what makes the queue form. Averaged into the traffic it puts a budget's p99 on a query nobody waits for.
- **A failed apply is a verdict, not an error.** Under a workload that will not let go, the executor's lock timeout refuses to force the migration through. That is the right behaviour and it is also the finding: the report carries it, and the run fails on it.
- **Both sides are measured because either alone lies.** A migration blocking for a third of a second inside a window measured in seconds hurts a few percent of the queries, which leaves the client-side p99 under budget while the server-side attribution shows the block plainly. `max_block` is the term that catches it.
- **The harness is what found the executor's repair-before-retry defect**, and is the only suite that can find that class. `apply` retried a lock timeout by running the statement again with no repair in between, and a `CREATE INDEX CONCURRENTLY` cancelled by the timeout leaves an invalid index, so the retry failed with "already exists". The kill matrices cannot reach it: they kill the process, and this needs the statement cancelled while the process lives.

## Adding a rule

A rule is one file, one test file, and one fixture pair. No framework changes.

```go
type Rule interface {
    ID() string
    Check(ctx Context, stmt *pgquery.RawStmt) []Finding
}
```

`Context` carries the target version, the whole `plan.Migration`, the step index and statement index, the lock model's analysis of this statement, the whole parsed and analysed step, the size thresholds, and the catalog snapshot, which is nil offline.

1. Add the ID constant and its description in `rules.go`, and the rule to `All()`. IDs are stable and never reused.
2. Write `lNNN.go`. Build findings with `finding()` for a fixed severity or `sized()` for one graded by the table. Attach a fix with `withFix()`. Never inspect SQL as text.
3. Write `testdata/lNNN.sql` holding the hazard in its flagged form **next to the spellings that must stay silent**. That second half is what stops the rule from being a false-positive generator.
4. Run `go test ./internal/lint/rules -update` to write `testdata/lNNN.json`, and audit the diff by hand. A golden file accepted without reading it is not a test.
5. If the rule is version-conditional, add a case to `test/flipmatrix` so a container proves the flip.
6. If it has an executable fix, add a case to `test/autofix` so the executor proves the fix converges.

The engine stamps `RuleID`, `File`, `Step` and `Span`, so a rule fills in only severity, message, detail and fix.

## Testing

| Suite                          | Proves                                                                                                          |
|--------------------------------|-----------------------------------------------------------------------------------------------------------------|
| `internal/lint/...` unit tests | Every package at 100% statement coverage.                                                                       |
| `internal/lint/rules/testdata` | Golden fixtures: each rule's flagged and silent forms, as JSON, regenerated with `-update` and audited by hand. |
| `test/lockmatrix`              | The lock model against a live server: `pg_locks`, `relfilenode`, `debug1` visit messages, SQLSTATE 25001.       |
| `test/flipmatrix`              | Version-conditional rules against every supported major. Runs nightly across the matrix.                        |
| `test/estimate`                | Duration estimates within a factor of two on a 10-million-row table. Gated behind `MIG_ESTIMATE=1`.             |
| `test/autofix`                 | Fixes converge to a fingerprint-identical schema through the executor, and lint clean.                          |
| `test/chaos`                   | The harness catches a known-bad migration, passes a known-good one, and refuses a workload with no long reader. |

Tests assert on parse trees, catalog state and structured output, never on log text.

## Review checklist

Reject any change where:

- SQL is inspected with a regex or with string matching instead of through the parse tree.
- A version-conditional behaviour is hardcoded without a container test that verifies it.
- A rule's severity is size-independent when the underlying hazard is not.
- A rule reads a threshold constant directly instead of `Context.Thresholds`, which is how a policy gets ignored.
- A suppression is accepted without a reason string.
- An autofix mutates a file without showing a diff, or emits a partial rewrite instead of a scaffold.
- An executable autofix has no `test/autofix` case holding it to the executor's fingerprint.
- The chaos workload omits a long-running reader.
- The linter forks the parser or the loader instead of reusing `internal/plan`.
- A setting the caller passed is ignored rather than refused.
- Testcontainers appears in the dependency graph of `cmd/mig`.
