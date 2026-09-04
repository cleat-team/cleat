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
#   go test ./engine/  with no CLEAT_TEST_* set  -> 3462 passed, 876 skipped, "ok"
#   go test ./engine/  with all three DSNs set   -> 4510 passed,   4 skipped, "ok"
#
# Both print ok. The first one tested no database at all. Measured 2026-09-03; the skipped
# column is the reliable half and is reproducible to the test, while the wall clock that
# used to be quoted here no longer separates the two cases (see CLAUDE.md, "Is this result
# real?"). Nothing in a green result can tell those two runs apart, which is why tier 1
# asserts the connection up front rather than inferring anything from one.
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

# --- 2a1. ...and every one of them must actually CONNECT ----------------------------
# A DSN that is SET but wrong is indistinguishable from one that works, by every signal
# this script had until now. Setting the variable is what stops a test skipping;
# connecting is a separate question, and neither the skip count nor the wall clock asks
# it -- those tests fail on connect instead of skipping.
#
# Measured 2026-09-03, reconstructing the three DSNs from memory instead of reading
# WS3-STATUS.md: wrong database name, wrong passwords on MySQL and MSSQL. The run produced
# a clean monotonic-looking result -- skips falling 876 -> 581 -> 4 across no-DSN /
# postgres-only / all-three, with the final 4 matching the table above EXACTLY. It was 1086
# connection failures. The matching 4 read as corroboration.
#
# The loop above would have passed all three. This is what it costs to find out otherwise:
# without the probe the gate discovers it 4779 tests and ~18 minutes later, as a wall of
# failures that mean "nobody could connect" and are indistinguishable from real ones in the
# summary line -- the same shape as the CLEAT_CRASH_DB precondition below, and the specific
# failure mode this whole script exists to prevent.
#
# TestTenantSelfAccess is the probe because it already does exactly this: forEachBackend
# calls backend.Setup(t) for every registered dialect, and Setup pings and Fatals rather
# than skipping. Measured: ~1s with all three reachable, and 0.9s to fail naming the
# dialect when one password is wrong.
#
# Asserting a PASS PER DIALECT rather than the exit code is the point. forEachBackend SKIPS
# a backend whose Enabled() is false, and a skipped subtest leaves `go test` printing ok --
# so an exit-code check here would be a green that measured nothing, in the script whose
# entire job is to refuse those.
note "checking that each tier-1 dialect accepts a connection"
PROBE=$(cd "$REPO_ROOT" && go test ./engine/ -run '^TestTenantSelfAccess$' -count=1 -v 2>&1)
for d in $DIALECTS; do
  if echo "$PROBE" | grep -q -- "--- PASS: TestTenantSelfAccess/$d"; then
    note "  $d: connected"
  else
    fail "$d: no connection was made. TestTenantSelfAccess/$d did not pass, so this dialect
       refused the connection, or was never registered, or skipped -- and a skip here leaves
       \`go test\` exiting 0, which is why this asserts a PASS per dialect rather than an exit
       code. If the DSN-is-set check above also failed for $d, that is the cause and this is
       the echo. Otherwise the variable is set and WRONG, which looks exactly like one that
       works to every other check in this script. Read the DSNs from WS3-STATUS.md (this
       checkout) or PARALLEL-WORKSTREAMS.md (defaults) rather than reconstructing them.

$(echo "$PROBE" | grep -E '^\s+.*(ping|unreachable|Access denied|login error|does not exist)' | head -3)"
  fi
done

# --- 2a2. Packages that need a database the dialect DSNs do not name ----------------
# tests/crash is in tier1.packages and does not use any CLEAT_TEST_* DSN. It reads
# CLEAT_CRASH_DB and, when that is unset, defaults to port 5433 -- deliberately not
# 5432, because PARALLEL-WORKSTREAMS.md assigns this suite its own instance so its
# crash-and-recover cycles cannot disturb another workstream's fixtures.
#
# Its five tests then Fatal rather than skip when that instance is unreachable, which
# is the right behaviour and is exactly why this check belongs here. Without it the
# gate does what it does for no other precondition: it runs the whole suite, spends
# several minutes, and reports
#
#   tier-gate: FAIL: tier 1 has 7 failing test(s)
#
# where five of the seven mean "nobody configured this" and two are real. Measured
# 2026-08-06 from inside the Python toolchain container: with CLEAT_CRASH_DB set,
# `go test ./tests/crash/...` is ok in 73.9s and the count drops to 2.
#
# A precondition the gate discovers 6605 tests in is not a precondition, it is a
# failure mode -- and the failures it produces are indistinguishable from real ones
# in the summary line, which is the specific thing this script exists to prevent.
#
# Guarded on the package actually being listed, so removing ./tests/crash/... from
# tiers.yaml removes this requirement with it rather than leaving a check for a
# suite that no longer runs.
if awk '/^tier1:/{t=1} t&&/^  packages:/{p=1;next} p&&/^    - /{print;next} p{exit}' "$TIERS" \
     | grep -q '\./tests/crash/'; then
  if [ -z "${CLEAT_CRASH_DB:-}" ]; then
    fail "CLEAT_CRASH_DB is unset -- ./tests/crash/... is a tier-1 package and does not use
       the CLEAT_TEST_* DSNs. It defaults to port 5433 (PARALLEL-WORKSTREAMS.md gives this
       suite its own instance) and its tests Fatal, not skip, when that is unreachable --
       so leaving it unset produces five failures that mean 'nobody asked' and are
       indistinguishable from real ones in this script's summary."
  else
    note "  crash suite: CLEAT_CRASH_DB is set"
  fi
fi

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

# --- 3b. Lower-tier tests living in tier-1 packages ---------------------------------
# D5. tier1.packages contains the Rust, Java and AssemblyScript integration tests and
# the parked decomposition test; tier 1 forbids skips, and no test fix removes a skip
# whose cause is an absent tier-2 toolchain. The decision was to filter rather than
# provision -- see the long note above tier1.exclude_tests in tiers.yaml.
#
# `go test -skip` is the right tool and the dangerous one: unlike t.Skip it emits no
# `--- SKIP` line, so an excluded test is invisible in the log rather than merely
# unrun. An exclusion list that silently swallowed a failing test would be exactly the
# "known failures" mechanism tier 1 forbids, wearing a different hat.
#
# Two things keep it honest, and both are below rather than in a comment:
#   * every pattern is resolved to concrete test names with `go test -list` and printed
#     on every run, so what was excluded is in the log next to what ran;
#   * a pattern matching nothing is a hard failure, so an entry cannot rot into place
#     after the test it names is renamed or deleted.
EXCL=$(awk '/^tier1:/{t=1} t&&/^  exclude_tests:/{p=1;next} p&&/^    - /{sub(/^    - /,"");gsub(/"/,"");print;next} p&&/^    #/{next} p&&/^$/{next} p{exit}' "$TIERS")

SKIP_RE=""
if [ -n "$EXCL" ]; then
  for pat in $EXCL; do
    if [ -z "$SKIP_RE" ]; then SKIP_RE="$pat"; else SKIP_RE="$SKIP_RE|$pat"; fi
  done
  note "excluding lower-tier tests: $SKIP_RE"

  # Resolve to names. -list does not run anything, so this is cheap and is the only
  # thing that makes the exclusion auditable rather than blind.
  # shellcheck disable=SC2086
  MATCHED=$(cd "$REPO_ROOT" && go test -list "$SKIP_RE" $PKGS 2>/dev/null | grep -E '^Test' | sort)
  NMATCH=$(printf '%s\n' "$MATCHED" | grep -c . || true)
  if [ "$NMATCH" = "0" ]; then
    fail "tier1.exclude_tests is non-empty but matches no test in tier1.packages.
       Every pattern is stale: the tests were renamed or removed and the list was not.
       Delete the entries rather than leaving a filter that hides nothing and says
       nothing."
  else
    note "  $NMATCH test(s) excluded, by name:"
    printf '%s\n' "$MATCHED" | while IFS= read -r n; do [ -n "$n" ] && note "    $n"; done
  fi

  # A pattern that matches nothing individually is just as stale as the whole list
  # being stale, and is far easier to miss.
  for pat in $EXCL; do
    # SC2086: $PKGS is a deliberately unquoted package list.
    # SC2015: `A && B || C` is not if-then-else, but here C is `|| true` on
    # the pipeline, guarding grep -c's exit 1 on no match -- which is the
    # case this loop exists to detect, so it must not abort.
    # shellcheck disable=SC2086,SC2015
    n=$(cd "$REPO_ROOT" && go test -list "$pat" $PKGS 2>/dev/null | grep -cE '^Test' || true)
    [ "$n" = "0" ] && fail "tier1.exclude_tests pattern '$pat' matches no test -- stale entry"
  done
fi

if [ "$FAILED" = "1" ] && [ "$MEASURE" = "0" ]; then
  echo "tier-gate: refusing to run -- the exclusion list above is not describing this tree." >&2
  exit 1
fi

# Keep the log. A gate that deletes its evidence on failure makes the CI operator
# re-run the whole suite to find out what broke.
LOG="${TIER_GATE_LOG:-$REPO_ROOT/tier-gate.log}"
: > "$LOG"

# -p 1 is required: engine/testutil's CleanupPostgresTestData is an unqualified
# DELETE FROM across eleven tables, so packages run concurrently against one database
# delete each other's fixtures mid-test.
# GO_TEST_TIMEOUT, because Go's default is 10 minutes and this gate outgrew it.
#
# Measured 2026-09-03 on a runner: `ran=6546 pass=6543 fail=0 skip=2` followed by
# `panic: test timed out after 10m0s` and `FAIL github.com/cleat-team/cleat/engine
# 600.038s`. Zero failing tests, and the job still went red -- which is the shape
# CLAUDE.md's "Is this result real?" section is about, read in the other
# direction: a red that is not a failure.
#
# This gate is the invocation that hits it because it is the one that sets all
# three DSNs. The ci.yml matrix runs the same packages against PostgreSQL only
# and finishes inside the default, which is why this surfaced here first rather
# than everywhere.
#
# 30m, not 45m: the workflow's own timeout-minutes is 45, and Go's timeout has to
# fire FIRST or the runner kills the job and prints no goroutine dump -- the
# difference between "which test was hanging" and "the job stopped". It also has
# to cover two sequential invocations, the root module and each tier-1 module.
GO_TEST_TIMEOUT="${GO_TEST_TIMEOUT:-30m}"

note "running root module: $(echo "$PKGS" | tr '\n' ' ')"
# SKIP_ARGS is empty when tier1.exclude_tests is empty, so the unfiltered run is the
# default and the filter has to be asked for in the manifest.
SKIP_ARGS=""
[ -n "$SKIP_RE" ] && SKIP_ARGS="-skip $SKIP_RE"
# shellcheck disable=SC2086
(cd "$REPO_ROOT" && go test -count=1 -p 1 -timeout "$GO_TEST_TIMEOUT" -v $SKIP_ARGS $PKGS) >> "$LOG" 2>&1
TEST_RC=$?

# Separate Go modules must be tested from inside their own directory; `go test
# ./cleat/...` from the root fails with "main module does not contain package".
MODDIRS=$(awk '/^tier1:/{t=1} t&&/^  modules:/{p=1;next} p&&/^    - dir: /{sub(/^    - dir: /,"");print;next} p&&/^      /{next} p{exit}' "$TIERS")
for md in $MODDIRS; do
  [ -f "$REPO_ROOT/$md/go.mod" ] || { fail "tiers.yaml names module '$md' but $md/go.mod does not exist"; continue; }
  note "running module: $md"
  # shellcheck disable=SC2086
  (cd "$REPO_ROOT/$md" && go test -count=1 -p 1 -timeout "$GO_TEST_TIMEOUT" -v $SKIP_ARGS ./...) >> "$LOG" 2>&1
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
