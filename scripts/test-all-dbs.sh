#!/usr/bin/env bash
#
# Run the database-backed tests against PostgreSQL, MySQL and SQL Server in ONE
# invocation, and say what happened rather than only whether it exited 0.
#
# This is `make test-all-dbs`. It replaces a target that ran
# `go test ./internal/host/...` three times: internal/host/ moved to engine/ in
# 3eeb74e, so the target failed with "no such file or directory" on every leg for
# months, and docs/project/release-process.md still told the release operator to
# run it (cleat#3457). Nothing ran it, so nothing noticed. `--self-test` is what
# keeps that from happening again: it resolves every package pattern below, so a
# renamed directory fails a lint step instead of a release.
#
# Usage:
#   scripts/test-all-dbs.sh               run the suite
#   scripts/test-all-dbs.sh --self-test   check the script itself; needs no database
#
# Environment (all three DSNs are REQUIRED, and there are deliberately no defaults):
#   CLEAT_TEST_POSTGRES (or CLEAT_TEST_DB)   PostgreSQL DSN
#   CLEAT_TEST_MYSQL                          MySQL DSN
#   CLEAT_TEST_MSSQL                          SQL Server DSN
#   PKGS      packages to test; default is the list below
#   TIMEOUT   per-package `go test -timeout`; default 30m
#   JSON      where to keep the `go test -json` report; default a temp file
#
# WHY NO DEFAULT DSNs. An unset DSN skips its dialect and the suite still prints
# `ok` (CLAUDE.md, "Is this result real?": 3462 passed and 876 skipped with none
# set). A default DSN is worse: a wrong one connects to nothing and reads as a
# working configuration. The DSNs are written down in WORKSTREAM.md.
#
# START FROM FRESH DATABASES. Any database a cleat release has migrated is refused
# by a 0.5.0 binary unless it is on the same chain, and a database a previous run
# left behind holds rows that fail a later run on MySQL and SQL Server (cleat#3474).
# Drop and recreate the three test databases before a run you intend to trust.
#
# WHAT IT DOES NOT RUN, and why: ./tests/exhaustion and ./tests/cluster need a
# running worker cluster (docker-compose.cluster.yml), not just a database. Run
# without one, the exhaustion test fails "no worker is serving task queue" and
# says itself that the run is "evidence about nothing". CI's cluster job owns
# them. Set PKGS to include them if a cluster is up.
#
# THE SKIP BUDGET IS NOT APPLIED. scripts/skip-ledger.d/ describes what each CI
# JOB may skip, and this run is not one of those jobs, so the budget would report
# errors about the environment rather than the tree. Read the skip names this
# prints instead: no remaining skip should be an unset or unreachable DSN.
#
# Exit status, and `2` is not `1`:
#   0   every test that ran passed, and something ran
#   1   a finding: a test failed, a package failed, a build failed, or timed out
#       Under `make`, any failure shows as make's own exit 2; the script's code is
#       on the `make: *** [test-all-dbs] Error N` line.
#   2   could not establish what was being measured (a DSN unset or not
#       connecting, a package pattern matching nothing, no test events at all).
#       This is a failure of the check, not a finding about the tree.
set -uo pipefail

SELF="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")"

# plugins/ is LAST, deliberately (cleat#3474): the notifications and webhookingest
# tests leave tenant_secrets rows behind on MySQL and SQL Server, sealed under a key
# only their own process held, and cmd/cleatctl's reseal test then fails on them
# ("unreadable") if it runs afterwards in the same database. CI runs those packages
# as separate jobs on separate databases, so it never meets this. That ordering
# makes a run on FRESH databases clean; it does not make a second run on the same
# databases clean, which is why the header says to start from fresh ones.
DEFAULT_PKGS="./engine/... ./migration/... ./migrations/... ./cmd/... \
./tests/integrity/... ./tests/crash/... ./tests/upgrade/... ./tests/scale/... \
./plugin/... ./pluginapi/... ./auth/... ./monitoring/... ./packaging/... ./plugins/..."

unmeasured() {
  echo "UNMEASURED: $*" >&2
  echo "This is a failure of the check, not a finding about the tree." >&2
  exit 2
}

# summarise <go test -json report>: print what failed and what skipped, and
# return 0 (clean), 1 (finding) or 2 (nothing ran). Read by --self-test too.
#
# The three failure counts are separate on purpose. A package that does not
# compile emits a package-level fail event with no "Test" field, so a count keyed
# on "Test" cannot see it, and a package that ran out of time looks identical
# until its own output is read (CLAUDE.md, "Is this result real?").
summarise() {
  python3 -I - "$1" <<'PY'
import json, re, sys

path = sys.argv[1]
passed = 0
test_fail, pkg_fail, skips = [], [], []
build_failed, timeouts = [], []
pkg_out = {}

for line in open(path, encoding="utf-8", errors="replace"):
    line = line.strip()
    if not line.startswith("{"):
        continue
    try:
        e = json.loads(line)
    except ValueError:
        continue
    act, test, pkg = e.get("Action"), e.get("Test"), e.get("Package", "")
    if act == "output":
        out = e.get("Output", "")
        if re.search(r"\[(build|setup) failed\]", out):
            build_failed.append(pkg or out.strip())
        if "panic: test timed out after" in out:
            timeouts.append(pkg or out.strip())
        continue
    if test:
        if act == "pass":
            passed += 1
        elif act == "fail":
            test_fail.append(f"{pkg} {test}")
        elif act == "skip":
            skips.append(f"{pkg} {test}")
    elif act == "fail":
        pkg_fail.append(pkg)

print(f"passed={passed} test-failures={len(test_fail)} "
      f"package-failures={len(pkg_fail)} build-failed={len(set(build_failed))} "
      f"timeouts={len(set(timeouts))} skipped={len(skips)}")

for t in test_fail:
    print(f"  FAIL  {t}")
for p in pkg_fail:
    why = ("did not build" if p in build_failed
           else "timed out" if p in timeouts
           else "failed with no test failure and neither a build error nor a timeout; read its output")
    print(f"  PACKAGE FAIL  {p}: {why}")

# Names, not the count. A name that mentions a dialect may mean a DSN did not
# take; several are structural (a MySQL subtest that is single-tenant by
# constraint), so this is for a human to read, not a verdict.
dialect = re.compile(r"postgres|mysql|mssql|sqlserver", re.I)
named = [s for s in skips if dialect.search(s)]
print(f"skips: {len(skips)} ({len(named)} name a dialect)")
for s in named:
    print(f"  skip names a dialect: {s}")

if passed + len(test_fail) == 0 and not pkg_fail:
    print("no test event of any kind was reported", file=sys.stderr)
    sys.exit(2)
sys.exit(1 if (test_fail or pkg_fail or build_failed or timeouts) else 0)
PY
}

# Every pattern must name at least one package. `go test` on a pattern that
# matches nothing exits 0 with "no packages to test" for some spellings, and a
# path that no longer exists is only an error from `go test`, not from a script
# that never ran it -- which is how the old target stayed in the runbook.
resolve_patterns() {
  local p n bad=""
  for p in $1; do
    n=$(go list "$p" 2>/dev/null | wc -l | tr -d ' ')
    [ "${n:-0}" -gt 0 ] || bad="$bad $p"
  done
  [ -z "$bad" ] || unmeasured "package pattern(s) match no package:$bad. Did a directory move? See scripts/test-all-dbs.sh DEFAULT_PKGS."
}

self_test() {
  local rc fails=0 here
  here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
  d=$(mktemp -d) || exit 2   # deliberately not local: the EXIT trap outlives this function
  trap 'rm -rf "${d:-}"' EXIT

  expect() { # name want-rc want-text
    local name=$1 want=$2 text=$3 out
    out=$(summarise "$d/$name.json" 2>&1); rc=$?
    if [ "$rc" != "$want" ] || { [ -n "$text" ] && ! grep -qF -- "$text" <<<"$out"; }; then
      echo "  FAIL $name: rc=$rc (want $want), wanted text: ${text:-none}"; awk '{print "        " $0}' <<<"$out"
      fails=$((fails + 1))
    else
      echo "  ok   $name: rc=$rc"
    fi
  }
  ev() { printf '{"Action":"%s","Package":"p/%s"%s}\n' "$1" "$2" "${3:+,$3}"; }

  { ev pass a '"Test":"TestOne"'; ev pass a; } > "$d/clean.json"
  expect clean 0 "passed=1 test-failures=0"

  { ev pass a '"Test":"TestOne"'; ev fail a '"Test":"TestTwo"'; ev fail a; } > "$d/testfail.json"
  expect testfail 1 "FAIL  p/a TestTwo"

  # The case a count keyed on "Test" cannot see: no test failed, a package did.
  { ev pass a '"Test":"TestOne"'
    ev output b '"Output":"FAIL\tp/b [build failed]\n"'; ev fail b; } > "$d/buildfail.json"
  expect buildfail 1 "PACKAGE FAIL  p/b: did not build"

  { ev pass a '"Test":"TestOne"'
    ev output b '"Output":"panic: test timed out after 30m0s\n"'; ev fail b; } > "$d/timeout.json"
  expect timeout 1 "PACKAGE FAIL  p/b: timed out"

  # A package-level failure with neither: still a finding, and says to read it.
  { ev pass a '"Test":"TestOne"'; ev fail b; } > "$d/otherpkg.json"
  expect otherpkg 1 "read its output"

  : > "$d/empty.json"
  expect empty 2 "no test event of any kind"

  { ev pass a '"Test":"TestOne"'; ev skip a '"Test":"TestNeedsMySQL"'; } > "$d/skipname.json"
  expect skipname 0 "skip names a dialect: p/a TestNeedsMySQL"

  # An unset DSN is UNMEASURED, never a pass: run the real script with none.
  out=$(env -i PATH="$PATH" HOME="${HOME:-/}" bash "$SELF" 2>&1); rc=$?
  if [ "$rc" = 2 ] && grep -q "UNMEASURED" <<<"$out"; then
    echo "  ok   no DSNs set: rc=2, says UNMEASURED"
  else
    echo "  FAIL no DSNs set: rc=$rc (want 2 and UNMEASURED)"; fails=$((fails + 1))
  fi

  # A pattern that matches nothing must be caught. The old target's path is the
  # known-positive: it is a real path that used to exist.
  out=$(cd "$here" && PKGS="./internal/host/..." CLEAT_TEST_POSTGRES=x CLEAT_TEST_MYSQL=x CLEAT_TEST_MSSQL=x \
        bash "$SELF" 2>&1); rc=$?
  if [ "$rc" = 2 ] && grep -q "match no package" <<<"$out"; then
    echo "  ok   dead package path: rc=2, names the pattern"
  else
    echo "  FAIL dead package path: rc=$rc (want 2 and 'match no package')"; awk '{print "        " $0}' <<<"$out"
    fails=$((fails + 1))
  fi

  # And every pattern this script runs by default has to resolve today.
  if (cd "$here" && resolve_patterns "$DEFAULT_PKGS") 2>/dev/null; then
    echo "  ok   every default package pattern resolves"
  else
    echo "  FAIL a default package pattern matches no package"; fails=$((fails + 1))
  fi

  [ "$fails" = 0 ] || { echo "self-test: $fails failure(s)"; exit 1; }
  echo "self-test: ok"
}

if [ "${1:-}" = "--self-test" ]; then
  self_test
  exit $?
fi

cd "$(dirname "${BASH_SOURCE[0]}")/.." || exit 2

PG="${CLEAT_TEST_POSTGRES:-${CLEAT_TEST_DB:-}}"
missing=""
[ -n "$PG" ] || missing="$missing CLEAT_TEST_POSTGRES"
[ -n "${CLEAT_TEST_MYSQL:-}" ] || missing="$missing CLEAT_TEST_MYSQL"
[ -n "${CLEAT_TEST_MSSQL:-}" ] || missing="$missing CLEAT_TEST_MSSQL"
[ -z "$missing" ] || unmeasured "not set:$missing. An unset DSN skips its dialect and the suite still prints ok. The DSNs are in WORKSTREAM.md."

export CLEAT_TEST_POSTGRES="$PG" CLEAT_TEST_DB="$PG"
export CLEAT_CRASH_DB="${CLEAT_CRASH_DB:-$PG}" \
       CLEAT_CRASH_MYSQL="${CLEAT_CRASH_MYSQL:-$CLEAT_TEST_MYSQL}" \
       CLEAT_CRASH_MSSQL="${CLEAT_CRASH_MSSQL:-$CLEAT_TEST_MSSQL}"

PKGS="${PKGS:-$DEFAULT_PKGS}"
TIMEOUT="${TIMEOUT:-30m}"
resolve_patterns "$PKGS"

# A DSN that is set but does not connect looks exactly like one that works until
# a test fails on connect. This probe Fatals on a set-but-unreachable DSN rather
# than skipping, and selects four tests: the parent and its three dialect subtests.
probe=$(go test ./engine/ -run TestPluginMigrations_AllDialects -count=1 -v 2>&1); prc=$?
runs=$(grep -c '^=== RUN' <<<"$probe")
if [ "$prc" != 0 ] || [ "${runs:-0}" -lt 4 ]; then
  echo "$probe" | tail -8 >&2
  unmeasured "the connectivity probe (TestPluginMigrations_AllDialects) exited $prc and selected ${runs:-0} test(s), want 4. A DSN is set but does not connect, or the probe selects nothing."
fi
echo "probe: TestPluginMigrations_AllDialects ran ${runs} tests across the three dialects"

JSON="${JSON:-$(mktemp "${TMPDIR:-/tmp}/test-all-dbs.XXXXXX")}"
echo "running: go test -count=1 -p 1 -timeout=$TIMEOUT -json $PKGS"
echo "report:  $JSON"
# -p 1 is both rules at once: engine/testutil's cleanup is an unqualified DELETE
# that packages would run against each other, and parallel packages exhaust the
# PostgreSQL connection limit. Two reasons for one flag (CLAUDE.md).
# shellcheck disable=SC2086 # PKGS is a deliberate word list
go test -count=1 -p 1 -timeout="$TIMEOUT" -json $PKGS > "$JSON" 2>&1
summarise "$JSON"
exit $?
