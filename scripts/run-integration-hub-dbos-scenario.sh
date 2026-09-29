#!/usr/bin/env bash
#
# Run examples/integration-hub-dbos-port end to end against a real DBOS
# runtime and a real PostgreSQL -- the DBOS-side twin of
# scripts/run-integration-hub-tenant-sandbox-scenario.sh. cleat#2597.
#
# WHAT THIS ASSERTS, AND WHY IT IS NOT THE USUAL PASS/FAIL SHAPE
#
# examples/integration-hub-dbos-port/README.md states the finding this pair
# exists to measure: DBOS has no platform-enforced boundary against a step
# reaching the host filesystem, unlike cleat's WASI policy
# (engine/wasi_policy.go). src/wedge.test.ts runs a legitimate tenant step
# (the mandatory positive control -- without it, a failure on the
# adversarial step would be indistinguishable from a DBOS runtime that fails
# everything) and an adversarial one that reads /etc/hosts, and asserts the
# adversarial read SUCCEEDS, because nothing in DBOS stops it.
#
# THIS SCRIPT JUST TRANSLATES npm test's OWN EXIT CODE -- it does not
# reinterpret it, because src/wedge.test.ts already distinguishes the three
# cases that matter:
#
#   0  both assertions held: the positive control ran, and the adversarial
#      read succeeded exactly as documented.
#   1  A FINDING, not "the test failed" in the ordinary sense: the
#      adversarial read was refused or threw. That would FALSIFY the
#      documented claim -- today's DBOS does not behave the way this pair
#      says it does -- and needs investigating, never silently accepted.
#   2  UNMEASURED: the positive control did not hold, or the test harness
#      crashed before reaching a verdict. Says nothing about the boundary
#      this pair measures.
#
# Read that as `1` meaning "go re-check this pair's finding against current
# DBOS", not "go fix this test" -- see the port's own README, "Reading this
# pair's exit codes", because `1` conventionally reads as a broken test and
# a future reader will assume exactly that unless told otherwise.
#
# NOT A DIALECT MATRIX. The property under test belongs to the language
# runtime (does anything in the execution path restrict a step's host
# access), not to SQL -- the same reasoning as cleat's own sandbox scenario
# ("ONE ARM, NOT A DIALECT MATRIX").
set -uo pipefail
set +e

cd "$(git rev-parse --show-toplevel)" || exit 2

EXAMPLE_DIR="examples/integration-hub-dbos-port"

if ! command -v node >/dev/null 2>&1; then
  echo "UNMEASURED: node is not available, so nothing here can be run." >&2
  exit 2
fi
if [[ ! -f "$EXAMPLE_DIR/package.json" ]]; then
  echo "UNMEASURED: $EXAMPLE_DIR/package.json is missing." >&2
  exit 2
fi

DB_URL="${DBOS_SYSTEM_DATABASE_URL:-}"
if [[ -z "$DB_URL" ]]; then
  echo "UNMEASURED: DBOS_SYSTEM_DATABASE_URL is not set." >&2
  echo "This script needs a reachable Postgres; it does not start one itself." >&2
  exit 2
fi

cd "$EXAMPLE_DIR" || exit 2

echo "==> npm install"
if ! npm install --no-audit --no-fund >/tmp/ih-dbos-npm-install.log 2>&1; then
  echo "UNMEASURED: npm install failed" >&2
  tail -30 /tmp/ih-dbos-npm-install.log >&2
  exit 2
fi

echo "==> npm run build"
if ! npm run build >/tmp/ih-dbos-npm-build.log 2>&1; then
  echo "UNMEASURED: npm run build failed" >&2
  tail -30 /tmp/ih-dbos-npm-build.log >&2
  exit 2
fi

echo "==> npm test"
DBOS_SYSTEM_DATABASE_URL="$DB_URL" npm test
rc=$?

case "$rc" in
  0) echo; echo "the documented finding holds: the sandbox gap is confirmed" ;;
  1) echo >&2; echo "FINDING: today's DBOS did not behave as this pair documents -- see the port's README before treating this as a broken test" >&2 ;;
  2) echo >&2; echo "UNMEASURED: the harness itself did not reach a verdict" >&2 ;;
  *) echo >&2; echo "UNMEASURED: npm test exited $rc, which none of this pair's documented codes describe" >&2; rc=2 ;;
esac

exit "$rc"
