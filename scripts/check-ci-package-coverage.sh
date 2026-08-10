#!/usr/bin/env bash
#
# Guard against CI test-matrix drift.
#
# The `test-go` matrix in .github/workflows/ci.yml enumerates package paths by
# hand. Enumerations rot: `engine/`, `wasm/`, `plugin/`, `auth/`, `migration/`,
# `monitoring/`, `pluginapi/` and `wasmrw/` were all absent from it for months,
# so the core of the product was never exercised by the main test lane.
#
# This script fails if a top-level directory containing Go packages is not
# matched by any path in the matrix. Run locally or in CI.
#
# Usage: scripts/check-ci-package-coverage.sh [path-to-ci.yml]
#
# The optional argument exists so the guard itself can be tested against a
# doctored workflow file.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CI_FILE="${1:-$REPO_ROOT/.github/workflows/ci.yml}"

# Directories that legitimately need no entry in the test-go matrix.
#   examples, testdata           — fixtures and demos, not product code
#   benchmarks, tests            — driven by their own dedicated CI jobs
#   packages                     — AssemblyScript SDK. NOTE: its Go harness
#     (packages/cleat-as/test_runner/test_runner.go) is NOT run by any CI job;
#     the `assemblyscript` job runs `npm test`, which invokes as-pect only.
#     Exempt here because this guard checks matrix drift, not overall coverage.
#     Wiring that harness into CI is tracked in IMPROVEMENT-PLAN.md Phase 2.
#   scripts                      — scripts/finddeadexports.go is a
#     //go:build ignore helper invoked by scripts/check-dead-exports.sh via
#     `go run`, not a package `go build ./...` or `go test ./...` ever
#     compiles. It has no tests to run and needs no matrix entry; it is
#     exercised by the "Dead-export code guard" lint step instead.
EXEMPT="examples testdata benchmarks tests packages scripts"

cd "$REPO_ROOT"

# Every top-level dir that contains at least one .go file.
# Portable: no `mapfile` (bash 4+) and no `find -printf` (GNU only), so this
# also runs on a stock macOS shell.
# `.claude/worktrees/` is excluded because it is gitignored (.gitignore:95)
# and holds full checkouts of this repo -- one per agent worktree. A bare
# `find .` walks into them and reports their contents as findings in this
# tree. CI never sees it (its checkout is clean), so this guard was only
# ever exercised where the bug could not appear, while anyone using the
# repo's own worktree convention hit it on every local run. The general
# rule, for the next `find .` added here: a gitignored directory holding a
# copy of the repo makes an unpruned walk report someone else's tree.
go_dirs="$(find . -mindepth 2 -name '*.go' -not -path './node_modules/*' \
             -not -path '*/node_modules/*' -not -path './.claude/*' 2>/dev/null |
  sed 's|^\./||' | cut -d/ -f1 | sort -u)"

# The set of directories covered by the matrix. Two forms count:
#   path: ./engine/...   -> engine
#   dir: cleat           -> cleat   (a separate Go module, tested from inside
#                                    it with `path: ./...`; see ci.yml)
matrix_dirs="$( {
  grep -oE 'path: .*' "$CI_FILE" |
    grep -oE '\./[A-Za-z0-9_-]+/\.\.\.' |
    sed 's|^\./||; s|/\.\.\.$||'
  grep -oE '^[[:space:]]*dir: [A-Za-z0-9_/-]+' "$CI_FILE" |
    sed 's|.*dir: ||'
} | sort -u)"

missing=""
count=0
for d in $go_dirs; do
  count=$((count + 1))
  case " $EXEMPT " in *" $d "*) continue ;; esac
  if ! printf '%s\n' "$matrix_dirs" | grep -qx "$d"; then
    missing="$missing $d"
  fi
done

if [ -n "$missing" ]; then
  echo "ERROR: these top-level Go package dirs are not covered by the test-go matrix" >&2
  echo "       in $CI_FILE:" >&2
  for d in $missing; do echo "  - $d" >&2; done
  echo >&2
  echo "Add them to the matrix, or add them to EXEMPT in $0 with a reason." >&2
  exit 1
fi

echo "OK: all $count top-level Go package dirs are covered or exempt."

# ---------------------------------------------------------------------------
# Second check: the `tests` exemption has to be true.
#
# `tests` is exempt above on the stated grounds that its suites are "driven by
# their own dedicated CI jobs". That was an assertion, not a check, and it was
# false for six of the seven suites: tests/cluster, tests/integrity,
# tests/upgrade, tests/soak, tests/scale and tests/cross-language were named by
# no workflow file at all. Only tests/plugin-harness was actually run.
#
# tests/integrity, tests/upgrade and tests/scale were wired into the test-go
# matrix on 2026-08-04, and tests/cluster into the cluster job the same day.
# All four were dropped from the list below. What it found on its first real run is the argument for
# clearing the rest: 22 of its 30 tests failed immediately against a migrated
# database (its fixture invented its own schema, without the foreign key
# production has), and behind that sat a real engine defect -- the event
# checksum chain was restarted on every write, so every workflow that suspends
# and resumes verified as corrupt. See IMPROVEMENT-PLAN 2.30.
#
# That is the same defect this whole guard exists to catch -- an enumeration
# that rotted -- hiding inside the guard's own exemption list. An exemption
# that says "covered elsewhere" is a claim about CI, so it gets verified like
# any other.
#
# Worth knowing when wiring these up: the Makefile's `test-cluster` target is
# also dead, running `./internal/host/...`, a path that has not existed since
# commit 3eeb74e moved internal/host/ to engine/.
#
# Suites known to be unwired are listed here rather than silently tolerated,
# so the set cannot grow without an edit to this file. Baselined rather than
# zeroed, the same as scripts/deadcode-baseline.txt -- clearing the backlog is
# tracked in IMPROVEMENT-PLAN.md Phase 2. Removing a name from this list when
# you wire its suite up must never fail the build; adding one requires saying
# why here.
# Empty: every suite under tests/ is now run by a workflow. Keep it that way.
# Adding a name here requires saying why, immediately below this line.
UNWIRED_SUITES=""

unreferenced=""
regressed=""
for suite_dir in tests/*/; do
  suite="$(basename "$suite_dir")"
  # Only suites that actually contain Go tests are in scope.
  if ! find "$suite_dir" -name '*_test.go' -print -quit 2>/dev/null | grep -q .; then
    continue
  fi
  if grep -rqF "tests/$suite" .github/workflows/ 2>/dev/null; then
    case " $UNWIRED_SUITES " in
      *" $suite "*)
        regressed="$regressed $suite"
        ;;
    esac
    continue
  fi
  case " $UNWIRED_SUITES " in
    *" $suite "*) continue ;;
  esac
  unreferenced="$unreferenced $suite"
done

if [ -n "$unreferenced" ]; then
  echo "ERROR: these test suites under tests/ contain Go tests but are named by" >&2
  echo "       no file under .github/workflows/, so nothing runs them:" >&2
  for s in $unreferenced; do echo "  - tests/$s" >&2; done
  echo >&2
  echo "A suite no job runs is not coverage. Wire it into a workflow, or add it" >&2
  echo "to UNWIRED_SUITES in $0 with a reason." >&2
  exit 1
fi

if [ -n "$regressed" ]; then
  echo "NOTE: now referenced by a workflow and can be dropped from"
  echo "      UNWIRED_SUITES in $0:"
  for s in $regressed; do echo "  - tests/$s"; done
fi

echo "OK: every tests/ suite is either run by a workflow or listed as unwired."
