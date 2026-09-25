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
#   scripts/tier-gate.sh                    enforce (exit non-zero on failure or skip)
#   scripts/tier-gate.sh --measure          report only, never fail the build
#   scripts/tier-gate.sh --shard I/N        run only shard I of N (1-based; see below)
#   scripts/tier-gate.sh --verify-shard-coverage N
#                                            no DB needed: confirm the N-1 engine
#                                            buckets union to exactly the full
#                                            ./engine/... test list, no gaps or dupes
#   scripts/tier-gate.sh --list-shard I/N   print what shard I/N would run, then exit
#   scripts/tier-gate.sh --self-test        offline check of the sharding logic itself
#
# Sharding (cleat#2081). Shards 1..N-1 partition ./engine/...'s top-level tests by
# a stable hash of the test name; shard N runs every other tier-1 package plus the
# cleat module, unsharded. N must be >= 2. The hash is sha1, not Python's built-in
# hash() -- hash() is randomised per PROCESS via PYTHONHASHSEED, and this script
# shells out to a fresh python3 on every call, so a hash()-based partition would
# put the same test in a different bucket on every invocation. That would make
# "the shards union to the full list" true by luck on any one run and false as a
# property of the partition -- see --self-test, which asserts two independent
# invocations agree.
#
# tiers.yaml is read with a YAML parser, not with awk (cleat#2085). Until that
# fix each tier1.* list had its own hand-rolled awk, and four of them stopped at
# the first line inside their block that was not a `- ` entry -- which is what a
# comment line is. `packages` therefore found 8 of the declared 12, so
# ./monitoring/..., ./tests/integrity/..., ./tests/upgrade/... and
# ./tests/crash/... ran in NO tier-1 gate, and the two counts that existed to
# catch a short read were the same pattern twice and confirmed it instead.
#
# The parse happens AFTER --self-test and --verify-shard-coverage, because both run
# in the `tier1-coverage` job, which has "No DB, no Python, no wasm-tools" by design
# and exists to fail a broken partition in seconds. Both modes exit before the parse,
# so neither acquires a PyYAML dependency from this change.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TIERS="$REPO_ROOT/tiers.yaml"
MEASURE=0
SHARD_SPEC=""
VERIFY_N=""
LIST_SHARD_SPEC=""
SELF_TEST=0
while [ $# -gt 0 ]; do
  case "$1" in
    --measure) MEASURE=1; shift ;;
    --shard) SHARD_SPEC="${2:?--shard wants I/N}"; shift 2 ;;
    --verify-shard-coverage) VERIFY_N="${2:?--verify-shard-coverage wants N}"; shift 2 ;;
    --list-shard) LIST_SHARD_SPEC="${2:?--list-shard wants I/N}"; shift 2 ;;
    --self-test) SELF_TEST=1; shift ;;
    *) echo "tier-gate: unknown argument: $1" >&2; exit 2 ;;
  esac
done

fail() { echo "tier-gate: FAIL: $*" >&2; FAILED=1; }
note() { echo "tier-gate: $*"; }
FAILED=0

# --- sharding helpers ----------------------------------------------------------
# Top-level Test funcs only. -list does not expand subtests (a dialect subtest
# like TestPluginMigrations_AllDialects/postgres never appears here), so this
# partitions exactly the names go test -run can select at this granularity.
#
# sort -u, not sort: TestDialectConstants is a top-level Test func in two
# DIFFERENT packages under ./engine/... (confirmed live,
# `go test ./engine/... -list '.*' | grep -E '^Test' | sort | uniq -d`), so the
# raw list carries the same name twice. A shared name always hashes to the same
# bucket regardless, so this cannot split it across shards either way -- but an
# undeduplicated count double-counts it, which is what --verify-shard-coverage
# measured before this line read plain `sort`: sum-of-buckets 3486, full list
# 3486, but only 3485 DISTINCT names in the union. Both readings were of a real
# duplicate name, not a partition bug; dedupe once here rather than at every
# caller.
engine_all_tests() {
  (cd "$REPO_ROOT" && go test ./engine/... -list '.*' 2>/dev/null) | grep -E '^Test' | LC_ALL=C sort -u
}

# engine_shard_names BUCKET NBUCKETS: reads test names on stdin (one per line),
# prints the ones whose sha1 falls in this bucket. sha1, not hash() -- see the
# usage comment above.
engine_shard_names() {
  python3 -c '
import sys, hashlib
bucket, n = int(sys.argv[1]), int(sys.argv[2])
for name in sorted(l.strip() for l in sys.stdin if l.strip()):
    if int(hashlib.sha1(name.encode()).hexdigest(), 16) % n == bucket:
        print(name)
' "$1" "$2"
}

# names_to_run_pattern: reads names on stdin, prints an anchored go test -run
# alternation. Empty input prints the LITERAL PATTERN '^$', which matches no
# test -- never the empty string, which go test reads as "run everything" and
# is exactly the false-green this format exists to avoid on an empty bucket.
# Test names are Go identifiers (go test ./engine/... -list '.*' | grep -vE
# '^Test[A-Za-z0-9_]*$' returns nothing but module 'ok' lines), so none can
# carry a regex metacharacter that would need escaping here.
names_to_run_pattern() {
  local names
  names=$(cat)
  if [ -z "$names" ]; then
    echo '^$'
  else
    echo "^($(printf '%s\n' "$names" | paste -sd '|' -))\$"
  fi
}

if [ "$SELF_TEST" = "1" ]; then
  st_fail() { echo "tier-gate --self-test: FAIL: $*" >&2; ST_FAILED=1; }
  ST_FAILED=0

  # Determinism across independent processes -- the property sha1 has and a
  # PYTHONHASHSEED-randomised hash() would not.
  FIXTURE=$(printf 'TestAlpha\nTestBeta\nTestGamma\nTestDelta\nTestEpsilon\n')
  A1=$(printf '%s' "$FIXTURE" | engine_shard_names 1 3)
  A2=$(printf '%s' "$FIXTURE" | engine_shard_names 1 3)
  [ "$A1" = "$A2" ] || st_fail "engine_shard_names disagreed with itself across two invocations on the same input: '$A1' vs '$A2'"

  # Coverage + disjointness over a synthetic 50-name corpus, 4 buckets.
  SYN=$(python3 -c "print('\n'.join('TestSynthetic%d' % i for i in range(50)))")
  UNION=""
  TOTAL=0
  for b in 0 1 2 3; do
    part=$(printf '%s' "$SYN" | engine_shard_names "$b" 4)
    n=$(printf '%s\n' "$part" | grep -c . || true)
    TOTAL=$((TOTAL + n))
    UNION="$UNION
$part"
  done
  UNIQ_COUNT=$(printf '%s\n' "$UNION" | grep . | sort -u | grep -c . || true)
  [ "$TOTAL" = "50" ] || st_fail "4 buckets covered $TOTAL of 50 synthetic names, not 50"
  [ "$UNIQ_COUNT" = "50" ] || st_fail "4 buckets produced $UNIQ_COUNT distinct names from 50 -- not disjoint"

  # Known positive: splitting one name two ways must leave one bucket empty,
  # and that empty bucket's pattern must be the literal '^$', never "".
  EMPTY_BUCKET=""
  for b in 0 1; do
    part=$(printf 'TestLonely' | engine_shard_names "$b" 2)
    [ -z "$part" ] && EMPTY_BUCKET="$b"
  done
  [ -n "$EMPTY_BUCKET" ] || st_fail "splitting one name into 2 buckets left neither empty -- cannot exercise the empty-bucket case"
  PAT=$(printf '' | names_to_run_pattern)
  [ "$PAT" = '^$' ] || st_fail "names_to_run_pattern on empty input produced '$PAT', not '^\$' -- an empty -run pattern matches EVERYTHING"

  PAT2=$(printf 'TestFoo\nTestBar\n' | names_to_run_pattern)
  [ "$PAT2" = '^(TestFoo|TestBar)$' ] || st_fail "names_to_run_pattern on [TestFoo TestBar] produced '$PAT2', expected '^(TestFoo|TestBar)\$'"

  if [ "$ST_FAILED" = "1" ]; then
    echo "tier-gate --self-test: FAILED" >&2
    exit 1
  fi
  echo "tier-gate --self-test: all checks passed"
  exit 0
fi

[ -f "$TIERS" ] || { echo "tier-gate: $TIERS not found" >&2; exit 2; }

if [ -n "$VERIFY_N" ]; then
  case "$VERIFY_N" in ''|*[!0-9]*) echo "tier-gate --verify-shard-coverage: N must be a positive integer, got '$VERIFY_N'" >&2; exit 2 ;; esac
  [ "$VERIFY_N" -ge 2 ] || { echo "tier-gate --verify-shard-coverage: N must be >= 2 (>=1 engine bucket plus the rest shard), got $VERIFY_N" >&2; exit 2; }
  EBUCKETS=$((VERIFY_N - 1))

  note "verifying ./engine/... shard coverage across $EBUCKETS bucket(s) (N=$VERIFY_N)"
  ALL=$(engine_all_tests)
  NALL=$(printf '%s\n' "$ALL" | grep -c . || true)
  if [ "$NALL" = "0" ]; then
    echo "tier-gate --verify-shard-coverage: go test ./engine/... -list produced no top-level tests -- could not measure, not a coverage finding" >&2
    exit 2
  fi
  note "  full unsharded list: $NALL top-level tests"

  UNION=""
  TOTAL=0
  b=0
  while [ "$b" -lt "$EBUCKETS" ]; do
    part=$(printf '%s\n' "$ALL" | engine_shard_names "$b" "$EBUCKETS")
    n=$(printf '%s\n' "$part" | grep -c . || true)
    note "  bucket $b/$EBUCKETS: $n tests"
    TOTAL=$((TOTAL + n))
    UNION="$UNION
$part"
    b=$((b + 1))
  done

  UNIQ_COUNT=$(printf '%s\n' "$UNION" | grep . | sort -u | grep -c . || true)
  MISSING=$(comm -23 <(printf '%s\n' "$ALL" | grep . | sort -u) <(printf '%s\n' "$UNION" | grep . | sort -u))
  EXTRA=$(comm -13 <(printf '%s\n' "$ALL" | grep . | sort -u) <(printf '%s\n' "$UNION" | grep . | sort -u))

  if [ "$TOTAL" != "$NALL" ] || [ "$UNIQ_COUNT" != "$NALL" ] || [ -n "$MISSING" ] || [ -n "$EXTRA" ]; then
    echo "tier-gate --verify-shard-coverage: FAIL -- union of $EBUCKETS bucket(s) does not equal the full list" >&2
    echo "  full=$NALL  sum-of-buckets=$TOTAL  distinct-in-union=$UNIQ_COUNT" >&2
    [ -n "$MISSING" ] && { echo "  MISSING from every bucket:" >&2; printf '%s\n' "$MISSING" | sed 's/^/    /' >&2; }
    [ -n "$EXTRA" ] && { echo "  in a bucket but not in the full list (stale? flaky -list?):" >&2; printf '%s\n' "$EXTRA" | sed 's/^/    /' >&2; }
    exit 1
  fi

  note "tier-gate --verify-shard-coverage: OK -- $EBUCKETS bucket(s) partition all $NALL tests exactly once"
  exit 0
fi

RUN_MODE="full"
SHARD_I_NUM=""
SHARD_N_NUM=""
SHARD_TO_PARSE="${SHARD_SPEC:-$LIST_SHARD_SPEC}"
if [ -n "$SHARD_TO_PARSE" ]; then
  case "$SHARD_TO_PARSE" in
    */*) ;;
    *) echo "tier-gate: --shard/--list-shard wants I/N, got '$SHARD_TO_PARSE'" >&2; exit 2 ;;
  esac
  SHARD_I_NUM=${SHARD_TO_PARSE%%/*}
  SHARD_N_NUM=${SHARD_TO_PARSE##*/}
  case "$SHARD_I_NUM" in ''|*[!0-9]*) echo "tier-gate: --shard I must be a positive integer, got '$SHARD_I_NUM'" >&2; exit 2 ;; esac
  case "$SHARD_N_NUM" in ''|*[!0-9]*) echo "tier-gate: --shard N must be a positive integer, got '$SHARD_N_NUM'" >&2; exit 2 ;; esac
  [ "$SHARD_N_NUM" -ge 2 ] || { echo "tier-gate: --shard N must be >= 2 (>=1 engine bucket plus the rest shard), got $SHARD_N_NUM" >&2; exit 2; }
  { [ "$SHARD_I_NUM" -ge 1 ] && [ "$SHARD_I_NUM" -le "$SHARD_N_NUM" ]; } || { echo "tier-gate: --shard I must be between 1 and N ($SHARD_N_NUM), got $SHARD_I_NUM" >&2; exit 2; }
  if [ "$SHARD_I_NUM" -lt "$SHARD_N_NUM" ]; then
    RUN_MODE="engine-shard"
  else
    RUN_MODE="rest-shard"
  fi
fi

if [ -n "$LIST_SHARD_SPEC" ]; then
  if [ "$RUN_MODE" = "engine-shard" ]; then
    EBUCKETS=$((SHARD_N_NUM - 1))
    BUCKET=$((SHARD_I_NUM - 1))
    engine_all_tests | engine_shard_names "$BUCKET" "$EBUCKETS" | names_to_run_pattern
  else
    echo "REST shard $SHARD_I_NUM/$SHARD_N_NUM: every tier1 package except ./engine/..., plus the cleat module"
  fi
  exit 0
fi

# --- 0. Every tier1.* read comes from ONE YAML parse --------------------------------
# See the header. Four separate awk parses of one file disagreed about the same key;
# this replaces all of them. It is also what scripts/check-required-contexts.py
# already did -- and that script read 12 tier1.packages while this one read 8, for
# as long as both existed. Two readings of one file that nobody compares are not a
# check; the disagreement is what found this (CLAUDE.md, "could this check have
# disagreed?").
#
# Fail closed, with its OWN exit status. 2 means "could not establish what was being
# measured" and is deliberately not 1 ("a finding about the tree"), because the two
# send the next person to different places: 1 says go and look at the code, 2 says
# the gate itself is broken. A gate that cannot read its own manifest must not
# report a green -- that is the failure this whole script exists to refuse.
TIERS_TSV=$(python3 - "$TIERS" <<'PY'
import sys

try:
    import yaml
except ImportError:
    sys.stderr.write(
        "tier-gate: PyYAML is not installed, so tiers.yaml cannot be parsed.\n"
        "  Install it (pip install pyyaml). A gate that cannot read its own\n"
        "  manifest must not report success.\n")
    sys.exit(2)

try:
    with open(sys.argv[1]) as fh:
        doc = yaml.safe_load(fh)
except Exception as exc:                 # any parse failure is the same answer here
    sys.stderr.write("tier-gate: %s is not valid YAML: %s\n" % (sys.argv[1], exc))
    sys.exit(2)

tier1 = doc.get("tier1") if isinstance(doc, dict) else None
if not isinstance(tier1, dict):
    sys.stderr.write("tier-gate: %s has no `tier1:` mapping\n" % sys.argv[1])
    sys.exit(2)

# `key<TAB>value`, one line per entry. An ABSENT key prints nothing rather than
# erroring -- skip_allowlist and exclude_tests are legitimately empty in some trees,
# and an empty list is a legitimate value for any of these. A key of the WRONG SHAPE
# is an error: that is a manifest this gate does not understand, not an empty one.
for key in ("dialects", "languages", "packages", "modules",
            "skip_allowlist", "exclude_tests"):
    value = tier1.get(key)
    if value is None:
        continue
    if not isinstance(value, list):
        sys.stderr.write("tier-gate: tier1.%s is not a list (got %s)\n"
                         % (key, type(value).__name__))
        sys.exit(2)
    for item in value:
        if key == "modules":
            if not isinstance(item, dict) or not isinstance(item.get("dir"), str):
                sys.stderr.write(
                    "tier-gate: tier1.modules entries must be `- dir: <path>`\n")
                sys.exit(2)
            item = item["dir"]
        if not isinstance(item, str):
            sys.stderr.write("tier-gate: tier1.%s has a non-string entry\n" % key)
            sys.exit(2)
        sys.stdout.write("%s\t%s\n" % (key, item))
PY
) || exit 2

# tier1_list KEY: the entries for one key, one per line. tier1_list_ws: the same,
# space-separated, for the inline lists the code below walks with `for d in $V`.
tier1_list()    { printf '%s\n' "$TIERS_TSV" | awk -F'\t' -v k="$1" '$1==k{print $2}'; }
tier1_list_ws() { tier1_list "$1" | paste -sd ' ' -; }

[ -n "$SHARD_SPEC" ] && note "shard $SHARD_I_NUM/$SHARD_N_NUM ($RUN_MODE)"

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
DIALECTS=$(tier1_list_ws dialects)
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
# the ones written down: wrong database name, wrong passwords on MySQL and MSSQL. The run produced
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
       works to every other check in this script. Read the DSNs from WORKSTREAM.md, under
       'Sandboxes, databases, and shared files', rather than reconstructing them. Note
       that the credentials differ per port -- a probe that varies only the port is not
       a test of the credentials.

$(echo "$PROBE" | grep -E '^\s+.*(ping|unreachable|Access denied|login error|does not exist)' | head -3)"
  fi
done

# --- 2a2. Packages that need a database the dialect DSNs do not name ----------------
# tests/crash is in tier1.packages and does not use any CLEAT_TEST_* DSN. It reads
# CLEAT_CRASH_DB and, when that is unset, defaults to port 5433 -- deliberately not
# 5432, because WORKSTREAM.md assigns this suite its own instance so its
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
# An engine shard never runs ./tests/crash/... (it isn't part of the engine module and
# never can be), so this precondition does not apply there -- checked here rather than
# left to be discovered as an unused service, so a future engine-shard-only CI job does
# not carry a requirement it cannot trigger.
#
# This guard never fired until cleat#2085. It tested membership by running the SAME
# truncated packages extraction the rest of the script used, and ./tests/crash/... is
# one of the four entries that extraction dropped -- so the check for "is the crash
# suite in tier 1" was itself answered by the bug it was written to be robust against,
# and the whole of 2a2 above was dead. Checked on the real list now:
# `tier1_list packages | grep -qxF ./tests/crash/...` -> matches.
if [ "$RUN_MODE" != "engine-shard" ] && tier1_list packages | grep -qxF './tests/crash/...'; then
  if [ -z "${CLEAT_CRASH_DB:-}" ]; then
    fail "CLEAT_CRASH_DB is unset -- ./tests/crash/... is a tier-1 package and does not use
       the CLEAT_TEST_* DSNs. It defaults to port 5433 (WORKSTREAM.md gives this
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
LANGS=$(tier1_list_ws languages)
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
         docker run --rm -v \"\$PWD\":/src -w /src -e CGO_ENABLED=1 \\
           cleat-py-toolchain go test ./engine/ -run 'TestPython'
       CHECK THE MOUNT FIRST -- a runtime that cannot bind-mount this path does
       not fail, it mounts an EMPTY directory, and the run then dies with
       'go.mod file not found', which reads as a checkout problem:
         docker run --rm -v \"\$PWD\":/src cleat-py-toolchain test -f /src/go.mod
       This used to prescribe --context desktop-linux, which was right on a
       machine that also ran colima (2026-08-06) and could not work on this one
       (2026-09-16: Docker Desktop is not running, so the flag fails to connect
       while a plain docker run mounts the tree). The check is the instruction;
       a context name is a property of one machine at one time. cleat#1694."
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
PKGS=$(tier1_list packages)
[ -n "$PKGS" ] || { echo "tier-gate: could not read tier1.packages from $TIERS" >&2; exit 2; }

NPKG=$(echo "$PKGS" | grep -c .)

# NDECL is a deliberately DIFFERENT reading of the same key: a line-range scan that
# never parses YAML at all and never exits early. Its only job is to disagree.
#
# It replaces two awk counters that shared the extractor's own pattern, so a short
# read confirmed itself -- the check that existed to catch an 8-of-12 extraction was
# the same bug written twice, and reported 8 == 8. A check that cannot disagree with
# the thing it checks is a claim, not a check (CLAUDE.md, "Is this result real?").
#
# Two things this scan deliberately does NOT understand, both on purpose:
#   * `- ` entries with trailing YAML comments (`- foo   # note`) are counted whole;
#     the YAML parse strips the comment. That would differ, so the difference is
#     reported rather than resolved. No entry carries one today; if one ever does,
#     this is where a reviewer is told.
#   * it ends the block at the next 2-space key, which is the manifest's shape. A
#     construct it does not model is a disagreement, not a silent truncation -- the
#     failure mode the old pattern had and this exists to refuse.
NDECL=$(awk '
  /^tier1:[[:space:]]*$/              {t=1; next}
  t && /^[^[:space:]#]/               {exit}       # next top-level key ends the block
  t && /^  packages:[[:space:]]*$/    {p=1; next}
  p && /^  [A-Za-z_]/                 {exit}       # next sibling key ends the block
  p && /^    - /                      {n++}        # an entry, whatever follows it
  END                                 {print n+0}
' "$TIERS")

if [ "$NPKG" != "$NDECL" ]; then
  echo "tier-gate: the YAML parse found $NPKG tier1.package(s), an independent line-range scan found $NDECL." >&2
  echo "  The two disagree, so one of them is wrong and this gate will not run until that is resolved:" >&2
  echo "  a short read here means packages silently run in NO tier-1 gate." >&2
  exit 2
fi
note "tier1.packages: $NPKG entries (YAML parse and independent line-range scan agree)"

# --- Which packages does THIS invocation actually run? -------------------------
# Unsharded: all of them, as always. An engine shard: only ./engine/..., further
# narrowed by -run below. The rest shard: everything else.
#
# The rest shard grew its four packages when cleat#2085 was fixed: it is the shard
# that runs ./monitoring/..., ./tests/integrity/..., ./tests/upgrade/... and
# ./tests/crash/..., none of which any shard ran before.
SHARD_PATTERN=""
RUN_MOD_DIRS=1
case "$RUN_MODE" in
  engine-shard)
    if ! printf '%s\n' "$PKGS" | grep -Fxq './engine/...'; then
      echo "tier-gate: --shard requested an engine shard but ./engine/... is not in tier1.packages" >&2
      exit 2
    fi
    EBUCKETS=$((SHARD_N_NUM - 1))
    BUCKET=$((SHARD_I_NUM - 1))
    SHARD_PATTERN=$(engine_all_tests | engine_shard_names "$BUCKET" "$EBUCKETS" | names_to_run_pattern)
    ACTIVE_PKGS="./engine/..."
    RUN_MOD_DIRS=0
    note "  engine shard $SHARD_I_NUM/$SHARD_N_NUM: -run '$SHARD_PATTERN'"
    ;;
  rest-shard)
    ACTIVE_PKGS=$(printf '%s\n' "$PKGS" | grep -Fxv './engine/...')
    note "  rest shard $SHARD_I_NUM/$SHARD_N_NUM: $(echo "$ACTIVE_PKGS" | tr '\n' ' ')"
    ;;
  *)
    ACTIVE_PKGS="$PKGS"
    ;;
esac

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
EXCL=$(tier1_list exclude_tests)

SKIP_RE=""
if [ -n "$EXCL" ]; then
  # One pattern per line, read with `while read` rather than `for pat in $EXCL`.
  # The old word-splitting form was fine only because no pattern contains a space;
  # a quoted regex with one would have been split into two patterns silently, and
  # a split pattern matches nothing -- the stale-entry failure this block exists to
  # detect, reached from the other side.
  while IFS= read -r pat; do
    [ -n "$pat" ] || continue
    if [ -z "$SKIP_RE" ]; then SKIP_RE="$pat"; else SKIP_RE="$SKIP_RE|$pat"; fi
  done <<EOF
$EXCL
EOF
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
  while IFS= read -r pat; do
    [ -n "$pat" ] || continue
    # SC2086: $PKGS is a deliberately unquoted package list.
    # SC2015: `A && B || C` is not if-then-else, but here C is `|| true` on
    # the pipeline, guarding grep -c's exit 1 on no match -- which is the
    # case this loop exists to detect, so it must not abort.
    # shellcheck disable=SC2086,SC2015
    n=$(cd "$REPO_ROOT" && go test -list "$pat" $PKGS 2>/dev/null | grep -cE '^Test' || true)
    [ "$n" = "0" ] && fail "tier1.exclude_tests pattern '$pat' matches no test -- stale entry"
  done <<EOF
$EXCL
EOF
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

note "running root module: $(echo "$ACTIVE_PKGS" | tr '\n' ' ')"
# SKIP_ARGS is empty when tier1.exclude_tests is empty, so the unfiltered run is the
# default and the filter has to be asked for in the manifest.
SKIP_ARGS=""
[ -n "$SKIP_RE" ] && SKIP_ARGS="-skip $SKIP_RE"
RUN_ARGS=""
[ -n "$SHARD_PATTERN" ] && RUN_ARGS="-run $SHARD_PATTERN"
# shellcheck disable=SC2086
(cd "$REPO_ROOT" && go test -count=1 -p 1 -timeout "$GO_TEST_TIMEOUT" -v $RUN_ARGS $SKIP_ARGS $ACTIVE_PKGS) >> "$LOG" 2>&1
TEST_RC=$?

# Separate Go modules must be tested from inside their own directory; `go test
# ./cleat/...` from the root fails with "main module does not contain package".
# Run once only -- skipped by the engine shards, which own no module directory.
if [ "$RUN_MOD_DIRS" = "1" ]; then
  MODDIRS=$(tier1_list modules)
  for md in $MODDIRS; do
    [ -f "$REPO_ROOT/$md/go.mod" ] || { fail "tiers.yaml names module '$md' but $md/go.mod does not exist"; continue; }
    note "running module: $md"
    # shellcheck disable=SC2086
    (cd "$REPO_ROOT/$md" && go test -count=1 -p 1 -timeout "$GO_TEST_TIMEOUT" -v $SKIP_ARGS ./...) >> "$LOG" 2>&1
    rc=$?
    [ "$rc" = "0" ] || TEST_RC=$rc
  done
fi

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
ALLOW=$(tier1_list skip_allowlist)
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
note "tier 1 green: $PASS passed, 0 failed, 0 skipped, on all of: $DIALECTS${SHARD_SPEC:+ (shard $SHARD_SPEC)}"
