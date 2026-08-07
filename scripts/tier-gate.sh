#!/usr/bin/env bash
#
# Enforce tier 1 from tiers.yaml.
#
# Tier 1's contract is "must pass, and a skip is a failure". That second half is the
# point of this script. `go test` reports a skip as neither a pass nor a failure, and
# CI reads it as a pass -- so a suite can go green having executed nothing. This repo
# has already paid for that repeatedly (see scripts/check-skips.sh for four cases), and
# the sharpest version is still live:
#
#   go test ./engine/  with no CLEAT_TEST_* set  -> 2544 tests, 166 skipped, 16s, "ok"
#   go test ./engine/  with all three DSNs set   -> 3846 tests,   4 skipped, 60s, "ok"
#
# Both print ok. The first one tested no database at all. Nothing in the tree could tell
# those two runs apart, which is why tier 1 asserts the connection rather than inferring
# it from a green result.
#
# Usage:
#   scripts/tier-gate.sh            enforce (exit non-zero on failure or skip)
#   scripts/tier-gate.sh --measure  report only, never fail the build
#
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TIERS="$REPO_ROOT/tiers.yaml"
MEASURE=0
[ "${1:-}" = "--measure" ] && MEASURE=1

fail() { echo "tier-gate: FAIL: $*" >&2; FAILED=1; }
note() { echo "tier-gate: $*"; }
FAILED=0

[ -f "$TIERS" ] || { echo "tier-gate: $TIERS not found" >&2; exit 2; }

# --- 1. CGO must be on -------------------------------------------------------------
# CGO_ENABLED=0 does not skip a check: it removes NewWasmtimeBackend (//go:build cgo)
# from the binary and silently runs everything on wazero, which tier 1 does not name.
# A tier-1 result obtained that way is not evidence about tier 1.
if [ "${CGO_ENABLED:-1}" = "0" ]; then
  fail "CGO_ENABLED=0 -- this removes the wasmtime backend entirely. Tier 1 names wasmtime."
fi

# --- 2. Every tier-1 dialect must actually connect ----------------------------------
# Read the dialect list out of tiers.yaml rather than restating it here, so the file
# stays the source of truth.
DIALECTS=$(awk '/^tier1:/{t=1} t&&/^  dialects:/{gsub(/.*\[|\].*/,""); gsub(/,/," "); print; exit}' "$TIERS")
[ -n "$DIALECTS" ] || { echo "tier-gate: could not read tier1.dialects from $TIERS" >&2; exit 2; }
note "tier 1 dialects: $DIALECTS"

# No associative arrays: macOS ships bash 3.2 and `declare -A` is a syntax error there.
# This script has to run on a developer's Mac as well as on a Linux runner, because a
# gate that only works in CI is a gate developers route around.
for d in $DIALECTS; do
  # Postgres needs BOTH names. The tree carries two conventions for the same DSN:
  # CLEAT_TEST_DB (26 files, and what ci.yml sets) and CLEAT_TEST_POSTGRES (12 files,
  # and what the workstream docs tell a developer to export). Setting only the second
  # silently skips everything keyed on the first -- which is how
  # TestAuthMiddlewareRejectsInvalidKey, an auth-rejection test, skipped in this gate's
  # own first run. The comment above that test records the same thing happening in CI
  # after a rename. Requiring both here is a stopgap; converging on one name is the fix.
  case "$d" in
    postgres) v="CLEAT_TEST_POSTGRES CLEAT_TEST_DB" ;;
    mysql)    v=CLEAT_TEST_MYSQL ;;
    mssql)    v=CLEAT_TEST_MSSQL ;;
    *)        fail "no DSN variable known for dialect '$d'"; continue ;;
  esac
  for one in $v; do
    eval "dsn=\${$one:-}"
    if [ -z "$dsn" ]; then
      fail "$one is unset -- $d tests keyed on it would skip silently and still print ok"
    else
      note "  $d: $one is set"
    fi
  done
done

# --- 2b. Every tier-1 guest language must have its toolchain ------------------------
# Same principle as the dialects. A guest language whose compiler is absent does not
# fail: its tests skip, and the suite still prints ok. Python is the case that forced
# this -- engine/python_wasm_e2e_test.go and cmd/cleat's build round-trip both skip on a
# missing componentize-py, and there are TWO prerequisites checked independently
# (componentize-py and wasm-tools), so installing only the first leaves the engine tests
# skipping with a different message.
LANGS=$(awk '/^tier1:/{t=1} t&&/^  languages:/{gsub(/.*\[|\].*/,""); gsub(/,/," "); print; exit}' "$TIERS")
[ -n "$LANGS" ] || { echo "tier-gate: could not read tier1.languages from $TIERS" >&2; exit 2; }
note "tier 1 languages: $LANGS"

for l in $LANGS; do
  case "$l" in
    # Nothing to check: the Go toolchain is what runs this script's `go test`.
    go) note "  go: the test runner's own toolchain" ;;
    python)
      for tool in componentize-py wasm-tools; do
        if command -v "$tool" >/dev/null 2>&1; then
          note "  python: $tool found"
        else
          fail "python is tier 1 but $tool is not on PATH -- its tests would skip and this gate would still print ok.
       On macOS componentize-py cannot run natively (it dies on EXC_GUARD /
       GUARD_TYPE_MACH_PORT, a Darwin kernel guard). Use the Linux container:
         docker build -f scripts/docker/python-toolchain.Dockerfile -t cleat-py-toolchain .
         docker --context desktop-linux run --rm -v \"\$PWD\":/src -w /src -e CGO_ENABLED=1 \\
           cleat-py-toolchain go test ./engine/ -run 'TestPython'
       Docker Desktop, not colima. Colima cannot bind-mount these paths, and it
       does not fail: -v \"\$PWD\":/src mounts an empty directory and the run dies
       with 'go.mod file not found', which reads as a checkout problem. Mounting
       the repo root under colima is worse still -- it succeeds and shows a
       different tree. --context desktop-linux is the whole fix."
        fi
      done
      ;;
    *) fail "no toolchain precondition known for tier-1 language '$l' -- add one here rather than letting it skip" ;;
  esac
done

if [ "$FAILED" = "1" ] && [ "$MEASURE" = "0" ]; then
  echo "tier-gate: refusing to run -- the preconditions above make a green result meaningless." >&2
  exit 1
fi

# --- 3. Run tier-1 packages ---------------------------------------------------------
# `next` is load-bearing: sub() rewrites $0, so without it the stripped line no longer
# matches /^    - / and the following rule's exit fires after the first entry. That bug
# shipped once and made this gate test 1 of 11 packages while reporting healthily --
# which is precisely the class of failure this script exists to catch.
PKGS=$(awk '/^tier1:/{t=1} t&&/^  packages:/{p=1;next} p&&/^    - /{sub(/^    - /,"");print;next} p{exit}' "$TIERS")
[ -n "$PKGS" ] || { echo "tier-gate: could not read tier1.packages from $TIERS" >&2; exit 2; }

NPKG=$(echo "$PKGS" | grep -c .)
NDECL=$(awk '/^tier1:/{t=1} t&&/^  packages:/{p=1;next} p&&/^    - /{n++;next} p{exit} END{print n+0}' "$TIERS")
if [ "$NPKG" != "$NDECL" ]; then
  echo "tier-gate: extracted $NPKG package(s) but tiers.yaml declares $NDECL" >&2; exit 2
fi

# Keep the log. A gate that deletes its evidence on failure makes the CI operator
# re-run the whole suite to find out what broke.
LOG="${TIER_GATE_LOG:-$REPO_ROOT/tier-gate.log}"
: > "$LOG"

# -p 1 is required: engine/testutil's CleanupPostgresTestData is an unqualified
# DELETE FROM across eleven tables, so packages run concurrently against one database
# delete each other's fixtures mid-test.
note "running root module: $(echo "$PKGS" | tr '\n' ' ')"
# shellcheck disable=SC2086
(cd "$REPO_ROOT" && go test -count=1 -p 1 -v $PKGS) >> "$LOG" 2>&1
TEST_RC=$?

# Separate Go modules must be tested from inside their own directory; `go test
# ./cleat/...` from the root fails with "main module does not contain package".
MODDIRS=$(awk '/^tier1:/{t=1} t&&/^  modules:/{p=1;next} p&&/^    - dir: /{sub(/^    - dir: /,"");print;next} p&&/^      /{next} p{exit}' "$TIERS")
for md in $MODDIRS; do
  [ -f "$REPO_ROOT/$md/go.mod" ] || { fail "tiers.yaml names module '$md' but $md/go.mod does not exist"; continue; }
  note "running module: $md"
  (cd "$REPO_ROOT/$md" && go test -count=1 -p 1 -v ./...) >> "$LOG" 2>&1
  rc=$?
  [ "$rc" = "0" ] || TEST_RC=$rc
done

RAN=$(grep -c '^=== RUN'    "$LOG")
PASS=$(grep -c -- '--- PASS' "$LOG")
FAILN=$(grep -c -- '--- FAIL' "$LOG")
SKIP=$(grep -c -- '--- SKIP' "$LOG")

echo
note "ran=$RAN pass=$PASS fail=$FAILN skip=$SKIP   (full log: $LOG)"

if [ "$TEST_RC" != "0" ]; then
  if [ "$FAILN" != "0" ]; then
    fail "tier 1 has $FAILN failing test(s)"
    grep -B1 -A6 -- '--- FAIL' "$LOG" | head -60
  else
    # go test exited non-zero with no test-level failure: a build error, a module
    # resolution error, or a panic outside a test. Do not report this as "0 failing
    # tests" -- the first draft did, and it hid a whole module going untested.
    fail "go test exited $TEST_RC with no failing test -- build/module error, not a test failure"
    grep -E '^(FAIL|#|.*cannot find|.*does not contain|panic:)' "$LOG" | head -20
  fi
fi

# --- 4. A skip in tier 1 is a failure -----------------------------------------------
# The allowlist covers non-tests only (see tiers.yaml). Allowlisted skips are still
# printed, so the exception stays visible rather than becoming invisible policy.
ALLOW=$(awk '/^tier1:/{t=1} t&&/^  skip_allowlist:/{p=1;next} p&&/^    - /{sub(/^    - /,"");print;next} p&&/^ *#/{next} p{exit}' "$TIERS")
SKIP_ALLOWED=0
if [ -n "$ALLOW" ]; then
  for a in $ALLOW; do
    n=$(grep -c -- "--- SKIP: $a" "$LOG")
    [ "$n" = "0" ] || note "allowlisted skip (non-test): $a x$n"
    SKIP_ALLOWED=$((SKIP_ALLOWED + n))
  done
fi
SKIP=$((SKIP - SKIP_ALLOWED))

if [ "$SKIP" != "0" ]; then
  fail "tier 1 skipped $SKIP test(s). Tier 1's contract is that a skip is a failure --"
  echo "      either the precondition belongs in tier 1 and must be installed, or the" >&2
  echo "      test belongs in tier 2. Do not widen this gate to accommodate it." >&2
  grep -A2 -- '--- SKIP' "$LOG" | grep -E 'SKIP|\.go:' | head -40
fi

if [ "$MEASURE" = "1" ]; then
  note "--measure: reporting only, not failing the build"
  exit 0
fi

[ "$FAILED" = "0" ] || exit 1
note "tier 1 green: $PASS passed, 0 failed, 0 skipped, on all of: $DIALECTS"
