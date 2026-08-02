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
#   examples, testdata, wasm-demo — fixtures and demos, not product code
#   benchmarks, tests            — driven by their own dedicated CI jobs
#   packages                     — AssemblyScript SDK. NOTE: its Go harness
#     (packages/cleat-as/test_runner/test_runner.go) is NOT run by any CI job;
#     the `assemblyscript` job runs `npm test`, which invokes as-pect only.
#     Exempt here because this guard checks matrix drift, not overall coverage.
#     Wiring that harness into CI is tracked in IMPROVEMENT-PLAN.md Phase 2.
EXEMPT="examples testdata wasm-demo benchmarks tests packages"

cd "$REPO_ROOT"

# Every top-level dir that contains at least one .go file.
# Portable: no `mapfile` (bash 4+) and no `find -printf` (GNU only), so this
# also runs on a stock macOS shell.
go_dirs="$(find . -mindepth 2 -name '*.go' -not -path './node_modules/*' \
             -not -path '*/node_modules/*' 2>/dev/null |
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
