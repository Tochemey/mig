#!/usr/bin/env bash
#
# Render a Go coverage profile as Markdown, for GITHUB_STEP_SUMMARY.
#
# Per-package figures are computed from the profile rather than from
# `go tool cover -func`, whose per-function percentages cannot be averaged:
# functions hold different numbers of statements. Blocks are keyed by position
# so that a profile merging the parent test binaries with their spawned
# children counts each block once, covered if either run reached it.

set -euo pipefail

profile="${1:-.cover/coverage.out}"
module="${2:-github.com/tochemey/mig}"

if [[ ! -s "$profile" ]]; then
  echo "## Test coverage"
  echo
  echo "No coverage profile at \`$profile\`."
  exit 0
fi

echo "## Test coverage"
echo

awk -v module="$module/" '
  NR == 1 { next }                                  # the "mode:" header

  {
    key = $1                                        # file:start,end
    stmts[key] = $2
    if ($3 + 0 > 0) hit[key] = 1

    colon = index($1, ":")
    file = substr($1, 1, colon - 1)

    slash = 0
    for (i = length(file); i > 0; i--) {
      if (substr(file, i, 1) == "/") { slash = i; break }
    }

    pkg[key] = substr(file, 1, slash - 1)
  }

  END {
    for (k in stmts) {
      p = pkg[k]
      total[p] += stmts[k]
      grand += stmts[k]

      if (k in hit) {
        covered[p] += stmts[k]
        grandCovered += stmts[k]
      }
    }

    if (grand == 0) {
      print "No statements found."
      exit
    }

    printf "**%.1f%%** of statements (%d of %d)\n\n", grandCovered * 100 / grand, grandCovered, grand
    print "| Package | Coverage | Statements |"
    print "| --- | --- | --- |"

    for (p in total) {
      name = p
      sub("^" module, "", name)
      printf "| %s | %.1f%% | %d |\n", name, covered[p] * 100 / total[p], total[p] | "sort"
    }

    close("sort")
  }
' "$profile"

below=$(go tool cover -func="$profile" | grep -v '100.0%$' | grep -v '^total:' || true)

if [[ -n "$below" ]]; then
  count=$(printf '%s\n' "$below" | wc -l | tr -d ' ')

  echo
  echo "<details><summary>Functions below 100% ($count)</summary>"
  echo
  echo '| Function | Coverage |'
  echo '| --- | --- |'

  printf '%s\n' "$below" | awk -v module="$module/" '
    {
      where = $1
      sub("^" module, "", where)
      printf "| `%s` %s | %s |\n", where, $2, $3
    }'

  echo
  echo "</details>"
fi
