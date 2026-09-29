#!/usr/bin/env bash
#
# Count code lines for one cleat-vs-DBOS pair, cleat side and DBOS side, with
# the exact same `cloc` invocation on both. cleat#2597.
#
# WHY THIS EXISTS
#
# A number quoted in a README rots the moment either side's source changes,
# and CLAUDE.md's own rule applies directly: "any number you write down
# carries a date and the command that re-derives it." Two sessions counting
# "the same way, slightly differently" -- one adding a --by-file flag, one
# not excluding node_modules, one counting comments and one not -- would
# produce two numbers that look like a discrepancy in the finding rather
# than a discrepancy in the tool invocation. Pinning both invocations in one
# script closes that off.
#
# USAGE
#
#   scripts/dbos-pair-loc.sh order-lifecycle
#   scripts/dbos-pair-loc.sh integration-hub
#
# Adding a pair means adding one case below, not a new script: the whole
# point is one counting rule for every pair this harness ever grows.
#
# integration-hub's file lists are NOT role-symmetric, and that asymmetry is
# itself part of the finding -- see the pair's own README
# (examples/integration-hub-dbos-port/README.md, "Why the totals below are
# not the whole comparison") before reading a smaller DBOS total as an
# efficiency win. Its cleat side includes engine/wasi_policy.go and
# engine/wasi_policy_wasmtime.go -- the enforcement code that makes the
# sandbox refusal real -- because that enforcement exists; the DBOS side has
# no equivalent to include, because nothing enforces anything there.
#
# EXIT STATUS
#
#   0  counted; both totals printed
#   2  UNMEASURED -- cloc is not installed, the named pair is unknown, or
#      the pair's files are missing. Never printed as if it were a count of
#      zero.
set -euo pipefail

if ! command -v cloc >/dev/null 2>&1; then
  echo "UNMEASURED: cloc is not installed" >&2
  echo "UNMEASURED: this is a failure of the check, not a finding about either side's size" >&2
  exit 2
fi

pair="${1:-}"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

case "$pair" in
  order-lifecycle)
    cleat_files=(
      "$repo_root/examples/order-lifecycle/order.go"
      "$repo_root/examples/order-lifecycle/backend/main.go"
      "$repo_root/examples/order-lifecycle/order_test.go"
    )
    dbos_files=(
      "$repo_root/examples/order-lifecycle-dbos-port/src/workflow.ts"
      "$repo_root/examples/order-lifecycle-dbos-port/src/server.ts"
      "$repo_root/examples/order-lifecycle-dbos-port/src/order.test.ts"
    )
    ;;
  integration-hub)
    cleat_files=(
      "$repo_root/examples/integration-hub/tenant-steps/normalize-order/main.go"
      "$repo_root/examples/integration-hub/tenant-steps/malicious-read-host-file/main.go"
      "$repo_root/engine/wasi_policy.go"
      "$repo_root/engine/wasi_policy_wasmtime.go"
    )
    dbos_files=(
      "$repo_root/examples/integration-hub-dbos-port/src/workflow.ts"
      "$repo_root/examples/integration-hub-dbos-port/src/wedge.test.ts"
    )
    ;;
  *)
    echo "usage: $0 order-lifecycle|integration-hub" >&2
    exit 2
    ;;
esac

for f in "${cleat_files[@]}" "${dbos_files[@]}"; do
  if [ ! -f "$f" ]; then
    echo "UNMEASURED: expected file is missing: $f" >&2
    exit 2
  fi
done

echo "== cleat side =="
cloc --quiet "${cleat_files[@]}"

echo
echo "== DBOS side =="
cloc --quiet "${dbos_files[@]}"
