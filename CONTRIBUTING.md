# Contributing

Contributions are welcome.

The project follows [Semantic Versioning](https://semver.org) and [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/).

To contribute:

- Fork the repository.
- Create a feature branch off `main`.
- Make your change, with tests.
- Submit a [pull request](https://help.github.com/articles/using-pull-requests).

## Prerequisites

| Requirement                                        | Why                                                                                                                |
|----------------------------------------------------|--------------------------------------------------------------------------------------------------------------------|
| Go 1.26.5 or later                                 | The version in `go.mod`.                                                                                           |
| cgo enabled                                        | `pg_query_go` links the Postgres parser.                                                                           |
| Docker                                             | The test suite starts a real Postgres through testcontainers, spawns the migrator as a child process and kills it. |
| [golangci-lint](https://golangci-lint.run) v2.12.2 | The version CI pins.                                                                                               |

## Before you submit

```sh
make lint    # golangci-lint, with the repo config
make test    # full suite with the race detector
make cover   # coverage, merged across parent and child processes
```

All three must pass. `make test` takes a few minutes on a first run because it pulls the Postgres image and seeds a template database. Later runs clone that template per test.

If Docker is not running, the suite skips the tests that need it and reports success. That is convenient locally and misleading in a pull request, so check that the tests actually ran rather than trusting a green result.

## Commit messages

Conventional Commits, in the imperative mood, describing what the change does rather than how the work went:

```
<type>: <summary>

[optional body]
```

Types used here:

| Type       | For                                   |
|------------|---------------------------------------|
| `feat`     | A new capability.                     |
| `fix`      | A bug fix.                            |
| `perf`     | A change made for speed or memory.    |
| `refactor` | A change with no effect on behaviour. |
| `test`     | Tests only.                           |
| `docs`     | Documentation only.                   |
| `build`    | Build files, dependencies, tooling.   |
| `ci`       | Workflows and CI configuration.       |
| `chore`    | Anything that fits nowhere else.      |

Examples from this repository:

```
feat: :sparkles: add mig up, annotated SQL migrations and index reconciliation
feat: :sparkles: backfill with atomic batch+checkpoint, throttle
```

A [gitmoji](https://gitmoji.dev) after the type is optional and used for features.

Two things to leave out of a message:

- Planning vocabulary. Milestone numbers, phase names and internal tracking references mean nothing to someone reading the log a year later.
- Narration of the work. Say what the commit changes, not that it was tricky, that a test was added afterwards, or that something was refactored twice on the way.

## Code conventions

Match the surrounding code. Beyond that:

**Every file carries the MIT header.** The `goheader` linter fails the build without it. Copy it from any existing file.

**Do not swallow errors.** If an error cannot be handled where it occurs, return it. A deferred `Close` whose error is discarded hides a real failure:

```go
// No.
defer func() { _ = conn.Close() }()

// Yes.
defer func() {
    if closeErr := conn.Close(); closeErr != nil {
        err = errors.Join(err, fmt.Errorf("release connection: %w", closeErr))
    }
}()
```

**Blank lines around multi-line blocks.** Put one before and after any `{ }` body that spans more than one line. Exceptions: a block that opens or closes its enclosing block needs none, and a single statement that sets up the block immediately below it can stay attached to it.

```go
cfg := funcConfig{}

for _, o := range opts {
    o(&cfg)
}

schema := cfg.schema
if schema == nil {
    schema = reflect(zero)
}
```

**One test file per source file.** `import.go` has `import_test.go`. Do not group several units into one test file, and do not leave a test file whose source file does not exist. Shared scaffolding such as `TestMain` and database helpers belongs in the package's central test file.

**Comments say why, not what.** The code already says what it does. A comment earns its place by recording the reason a thing is done the way it is, especially when the obvious alternative is wrong.

## Tests

Every code path needs a test, including the error paths. Coverage must not regress; `make cover` prints the total and `make cover-summary` breaks it down per package and per function.

Three kinds of test exist, and a change usually needs the first:

- **Package tests** run against a real Postgres in a container. Most of the suite.
- **Recovery tests** in `test/kill` run the migrator as a child process and send it SIGKILL at named points in `internal/crash`. Add a crash point when you add a step kind that can be interrupted part way through.
- **Fault injection** through `test/faultdb` fails a chosen statement inside the driver. Use it for the error branch of a database call, which no kill can reach because the process has to survive to report it.

A test that asserts recovery ends with the same bundle: the schema and data fingerprint match an uninterrupted run, no invalid indexes, no unvalidated constraints, no lease left held, and a further run applies and repairs nothing.

## Changelog

Add an entry to `CHANGELOG.md` under `## [Unreleased]`, in the `Added`, `Changed`, `Fixed` or `Removed` section. Write what changed and why it matters to someone using the tool, not which files moved.

## Reporting bugs

Open an issue with the Postgres version, the migration that reproduces it, and what the database looked like afterwards. `mig status --json` and `mig fingerprint --describe` are the two commands worth attaching.

For anything with a security impact, follow [SECURITY.md](SECURITY.md) instead of opening a public issue.

## Code of conduct

Participation is governed by [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).
