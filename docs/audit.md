# The false-positive audit

This document records the audit that gated the linter's rule catalog: three real-world migration corpora, every finding hand-graded, against an acceptance bar of a false-positive rate under 5%. The run is offline, default policy, all 24 rules. The [README](../README.md#linting) explains how to use the linter; [lint.md](lint.md) explains how it works and why the rules are shaped the way they are.

## Contents

- [Corpus](#corpus)
- [Criteria, written before classification](#criteria-written-before-classification)
- [Transaction fidelity](#transaction-fidelity)
- [Per-rule verdicts](#per-rule-verdicts)
- [Tally](#tally)
- [The fixes the audit demanded](#the-fixes-the-audit-demanded)
- [After the fixes](#after-the-fixes)

## Corpus

| Repo        | SHA                                      | Source                                                | Target PG | Files         | Excluded               |
|-------------|------------------------------------------|-------------------------------------------------------|-----------|---------------|------------------------|
| Mattermost  | d5d5b2ca2973ef92f3b3eaad182fe78816cf1481 | server/channels/db/migrations/postgres                | 13        | 206           | 3 comment-only         |
| Sourcegraph | c864f15af264f0f456a6d8a83290b5c940715349 | migrations/{frontend,codeintel,codeinsights}          | 12        | 380 + 23 + 28 | 3 + 1 + 1 comment-only |
| Temporal    | ce1e067e41facf31e588f65da3b5eacd324e8fa3 | schema/postgresql/v12/{temporal,visibility}/versioned | 12        | 25 + 15       | 0                      |

Conversion is mechanical: Mattermost loads as a golang-migrate directory unchanged; Sourcegraph's per-migration `up.sql` is flattened to `<dir>.sql`; Temporal's files are sequenced in manifest order. Comment-only no-op migrations are excluded because the mig format refuses an empty step; each exclusion is counted above.

## Criteria, written before classification

- **TP**: the statement can cause the hazard the rule names, with the lock and preconditions the rule describes. That the table is known small, that the team tolerated the downtime, or that the migration ran fine in production are not defenses; offline severity already prices size in.
- **FP**: the statement cannot cause the named hazard: the lock the rule assumes is not taken, a precondition the rule requires is absent, or the rule misread the tree.
- **Arguable**: the hazard is possible but the finding misfits the corpus's operating model in a way the linter could not know offline. Counted separately, never against the 5% and never toward it.

A finding is judged against the SQL its span points at, not against its message. One judgment covers a group only when the statements are the same shape.

## Transaction fidelity

The first run manufactured 94 L002 findings and dozens of L020/L021 ones by loading every file as a transactional step when the source runner never wrapped it in one. The corpus carries each runner's real semantics:

- golang-migrate sends a file as one Exec, an implicit transaction for a multi-statement file; Mattermost's 30 CONCURRENTLY files are all single statements, autocommit in reality, and load as notx.
- Sourcegraph's migrator wraps a migration unless metadata.yaml says createIndexConcurrently: true; those 63 load as notx.
- Temporal's schema tool splits every file and runs each statement as its own Exec (tools/common/schema/updatetask.go, execStmts); every Temporal step is notx.

After that, every remaining finding is the rule's own doing, not the harness's.

## Per-rule verdicts

Each judgment was made against the SQL the span points at. Scripts only did the grouping bookkeeping; every FP class and every borderline file was confirmed by hand, and the flips are recorded here.

### L001 `CREATE INDEX` without `CONCURRENTLY`: 153 TP, 0 FP

Every finding is a non-concurrent build on a table that pre-exists the migration. The same-file created-table suppression held throughout: files that create tables and index them drew findings only for the pre-existing tables they also touch. Nit, not an FP: on a step that is already notx the remedy tail "and mark the step notx" is stale.

### L003 rewriting `ADD COLUMN`: 66 TP, 0 FP

All are genuine rewrites: volatile defaults (random(), nextval(), gen_random_uuid()), SERIAL on an existing table, stored generated columns (Temporal's 35-column search-attribute ALTER accounts for the bulk, one finding per action by design).

### L004 `ALTER COLUMN TYPE`: 18 TP, 6 FP

The six FPs are metadata-only changes Postgres does not rewrite: varchar(100)->varchar(255) and varchar(26)->varchar(100) (Mattermost 000104), varchar(2000)->text (Mattermost 000122), varchar(15)->varchar(128) (Temporal 008), varchar(64)->varchar(255) twice (Temporal 014). In every case the prior type is visible in the same lineage the linter loaded. This is the known no-rewrite-transition gap, now measured.

### L005 `SET NOT NULL`: 27 TP. L006 FK: 8 TP. L007 CHECK: 8 TP. L008 PK: 7 TP. L009 inline UNIQUE: 1 TP

No FPs. Every constraint lands on a pre-existing table with no proving CHECK / NOT VALID split anywhere in the lineage.

### L020 several ACCESS EXCLUSIVE in one transaction: 35 TP, 39 FP

Two FP causes, both in how the relation set is counted:

1. **Relations created in the same step can block nobody.** All twelve Mattermost create_* files, all three Sourcegraph squashed bootstraps, 000112's drop-and-recreate, and 1669576792's TEMPORARY table (invisible to other sessions, a lock-model gap of its own).
2. **A table and its own index or view are one lock domain.** DROP INDEX locks the parent table; a view whose base is already locked blocks a subset of the same traffic (changesets + reconciler_changesets, lsif_uploads + its view, and friends).

Hand flips against the classifier: 1670365552, 1670881409, 1678994673 to TP (dropping a table or index takes ACCESS EXCLUSIVE on the pre-existing parent even when recreated in the same file); 000088 to TP (dropped legacy tables can exist on adopted databases); 1648628900 to FP (locked view over a locked base).

### L021 long work alongside other locks: 94 TP, 81 FP

FP causes: 79 findings whose long statement targets a same-file-created table (an index build or scan on empty storage is instant, so nothing is stretched), plus 1658174103 (scan feeds a brand-new table; no blockable lock overlaps it) and 1667220502 (the only other statement follows the scan, so no lock precedes it). Everything else is real, including the same-table cases: an instant ADD COLUMN whose ACCESS EXCLUSIVE is then held through a long index build is the rule's core case, and 1661502186 opens with LOCK repo IN EXCLUSIVE MODE before a full-table rebuild.

### L030 drops: 109 TP, 0 FP

Every drop is of a pre-existing table or column; the two suspects checked out (the _bak table pre-exists its dropper; 1648628900 really drops the old table rather than renaming).

### L031 renames: 3 TP, 1 FP

The FP: 1679058200 renames a column created, backfilled and swapped inside one transaction; the old name never existed outside it. The other three rename columns a dual-writer from an earlier migration can still reference.

### L032 created and never granted: 0 TP, 378 FP

Zero GRANT statements exist anywhere in any of the three lineages (677 files). No repo manages a restricted application role in its migrations, so the hazard the rule names cannot occur in any of them, and the rule warned on every CREATE TABLE all three repos ever wrote. The premise is detectable from the plan itself: a lineage that never grants is a lineage this rule cannot help.

### L033 TRUNCATE: 14 TP, 0 FP. L040 whole-table writes: 23 TP, 0 FP

All deliberate resets and backfills of pre-existing tables; the hazard (data loss, long row work in a transaction) is exactly what the rules name.

## Tally

| Corpus                   | Findings | TP      | FP      | FP rate   |
|--------------------------|----------|---------|---------|-----------|
| mattermost               | 206      | 70      | 136     | 66.0%     |
| sourcegraph-frontend     | 578      | 327     | 251     | 43.4%     |
| sourcegraph-codeintel    | 81       | 36      | 45      | 55.6%     |
| sourcegraph-codeinsights | 35       | 7       | 28      | 80.0%     |
| temporal-temporal        | 50       | 6       | 44      | 88.0%     |
| temporal-visibility      | 121      | 120     | 1       | 0.8%      |
| **overall**              | **1071** | **566** | **505** | **47.2%** |

FP by cause: L032 premise 378, same-step-created relations treated as blockable (L020 + L021 + L031) 106, table-and-dependent counted as two domains (L020) 21, no-rewrite ALTER TYPE (L004) 6, no lock actually stretched (L021) 2. Excluding L032 alone the rate is 127/693 = 18.3%. The acceptance (under 5%) was **not met** by the rule set as first written.

## The fixes the audit demanded

1. **Same-step-created relations are not blockable.** The lock model's relation set for L020/L021/L031 (and the temp-table case) must exclude relations the step itself creates. Kills 106 FPs across three rules and all three repos.
2. **Count lock domains, not relations, in L020.** An index or sequence belongs to its table; a view whose base is locked adds nothing. A drop of a pre-existing relation still counts even when the same step recreates it. Kills 21 FPs.
3. **L004 must know the no-rewrite transitions.** Track column types through the plan; varchar widening, varchar->text and friends are metadata-only. When the prior type is unknown, keep the warning but say the rewrite is conditional. Kills 6 FPs.
4. **L032 needs evidence of grant discipline.** Fire only when the lineage contains at least one GRANT (or a policy opts in). Kills 378 FPs, and keeps the rule for the deployments it was written for.
5. **L021 message nit**: drop the stale "mark the step notx" tail when the step already is, and consider statement order for the trailing-DDL case.

Predicted rate after fixes 1-4: 2/693 known residual (the two L021 order cases if fix 5's order logic is skipped), well under 5%.

## After the fixes

Rerun 2026-08-01, same corpus, same SHAs. All four fixes are implemented, plus two the implementation surfaced: the referenced side of a foreign key carries the validation's duration, which is no work at all when the referencing table is the step's own (killed a seventh FP shape, 1690401277), and any `DropStmt` was recorded as a relation event, so a dropped trigger sharing its table's name could have faked a recreation (caught by the history's own tests, never observed in the wild).

Mechanically: a new `internal/lint/history` package reads the whole plan once (creations and drops with positions, index and sequence parents, view bases through joins and CTEs, column types over time, grant discipline); L020 counts lock domains over blockable relations; L021 finds its long work and its hostages among blockable relations only; L004 consults the prior type for the in-place transitions; L031 skips columns this migration added; L032 requires grant evidence; L001 drops the stale notx remedy on notx steps.

| Corpus                   | Findings | Audit-graded TP | Residual FP   |
|--------------------------|----------|-----------------|---------------|
| mattermost               | 70       | 69              | 1             |
| sourcegraph-frontend     | 330      | 328             | 2             |
| sourcegraph-codeintel    | 36       | 36              | 0             |
| sourcegraph-codeinsights | 7        | 7               | 0             |
| temporal-temporal        | 6        | 6               | 0             |
| temporal-visibility      | 120      | 120             | 0             |
| **overall**              | **569**  | **566**         | **3 (0.53%)** |

**The acceptance is met.** Nothing new fires that did not fire before; the survivors match the audited TP sets exactly, after these re-grades forced by consistency:

- L031's three swap-pattern renames (1684248574) were mis-graded TP: the columns are added, backfilled and swapped in the same file, so the audit's own scaffolding logic makes them FP. Fixed by the same-file column check.
- 000088's L020 was graded TP while the emoji-file ghost drops were graded FP; one ghost policy now covers both (IF EXISTS drop of a relation the plan never created is reconciliation), so 000088's L020 re-grades FP and is suppressed. Its L030 deploy-order findings stand.
- Six L021 findings retargeted from fresh tables to the real pre-existing work beside them (000147, 1671463799, 1674669326, 1677166643, 1700645180 kept as TP; 1690401277 killed by the implicit-effect fix).

The three residuals, each accepted and documented rather than patched:

1. 1658174103: the scan feeds a brand-new table; the view churn around it is counted as a hostage though its locks land after the scan.
2. 1667220502: the only other statement follows the DELETE, so no lock actually precedes the long work. Both need statement-order reasoning in L021, deliberately skipped: order-blind is simpler and errs loud.
3. 000104's L021 calls the step's work "a table rewrite" though L004 now knows both varchar changes are in-place. The lock model's durations do not consult the history yet; that is the ALTER-TYPE catalog work already on the roadmap.

Quality gates at the rerun: whole suite green (32 packages), lint tree at 100% statement coverage including the new history package, vet and golangci-lint clean. New regression anchors: `testdata/blockable.sql` (fresh, ghost, temp and recreation shapes), `testdata/l032_nogrants.sql`, the notx L001 case, the L021 fresh-table and implicit-FK cases, the L031 scaffolding rename, and cross-file tests for L020's domain collapse and L004's transition table.
