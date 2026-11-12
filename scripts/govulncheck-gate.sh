#!/usr/bin/env bash
#
# Decide whether one module's govulncheck result fails the Vulnerability Check,
# with exactly ONE advisory waived, for a bounded time.
#
#   govulncheck-gate.sh <module-dir>       run govulncheck in the module, decide
#   govulncheck-gate.sh --classify RC FILE TODAY
#                                          decide from a captured run (used by
#                                          --self-test; no tool is run)
#   govulncheck-gate.sh --self-test
#
# Exit status, three ways on purpose:
#   0  passed -- nothing found, or only the waived advisory inside its window
#   1  a finding -- another advisory, or the waiver has expired
#   2  could not establish what govulncheck reported (the check failed, not
#      necessarily the tree)
# CI treats 1 and 2 alike; the difference is where to send the reader.
#
# THE WAIVER. cleat#3701, owner decision D69. A required Vulnerability Check
# went red on GO-2026-6859 (net/http, fixed in go1.27.4) and the fix cannot land:
# go1.27.4's cgo now rejects wasmtime-go's bundled libwasmtime.a (wasmtime-go
# issue 299), so cleat does not build under it. The advisory looks reachable in
# cleat-worker -- http.MaxBytesReader is called in server.go and
# plugin_body_limit.go -- so this is a deliberate, bounded acceptance and not a
# claim that the tree is unaffected.
#
# REMOVE IT when go1.27.5 ships or wasmtime-go fixes issue 299, in the same
# change that bumps the toolchain. Until then it must not outlive its reason:
# the waiver is valid THROUGH WAIVER_EXPIRES inclusive, and the first day it
# fails again is the day after. Extending it is a new owner decision, not an
# edit to this date.
# THE TOOL IS PINNED in the Vulnerability Check step of .github/workflows/ci.yml
# (GOVULNCHECK_VERSION, cleat#3706), because this script parses its text output:
# a format change in a new release must not turn the required check red on every
# PR at once. The step prints a warning annotation when a newer version exists.
# Bumping it is deliberate: read the release notes, run --self-test, and run a
# real scan under the new version and compare the ids it lists.
WAIVED_ID="GO-2026-6859"
WAIVER_EXPIRES="2026-12-11"
WAIVER_ISSUE="https://github.com/cleat-team/cleat/issues/3701"

set -uo pipefail

# classify RC FILE TODAY -- the decision, with no I/O beyond reading FILE.
#
# Reads the text output rather than the JSON stream, because text mode's own
# exit status is the authority on "reachable" (module- and package-level
# findings appear in JSON and do not fail a run). The text is then checked
# against itself: the ids listed must equal the count govulncheck states, so a
# format change that hides a finding yields 2 rather than a pass.
classify() {
  local rc="$1" file="$2" today="$3" ids n_ids stated id bad=""
  case "$rc" in
    0) echo "govulncheck: no reachable vulnerabilities"; return 0 ;;
    3) ;;
    *) echo "::error title=govulncheck::UNMEASURED: govulncheck exited $rc (want 0 or 3); this is a failure of the check, not a finding about the tree" >&2; return 2 ;;
  esac
  if ! [[ "$today" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ && "$WAIVER_EXPIRES" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]]; then
    echo "::error title=govulncheck::UNMEASURED: cannot read a date (today='$today' expires='$WAIVER_EXPIRES')" >&2
    return 2
  fi
  ids="$(sed -n 's/^Vulnerability #[0-9]\{1,\}: \(GO-[0-9]\{4\}-[0-9]\{1,\}\).*$/\1/p' "$file")"
  n_ids="$(printf '%s\n' "$ids" | grep -c .)"
  stated="$(sed -n 's/^Your code is affected by \([0-9]\{1,\}\) vulnerabilit.*$/\1/p' "$file" | head -1)"
  if [ "$n_ids" -eq 0 ] || [ -z "$stated" ] || [ "$n_ids" != "$stated" ]; then
    echo "::error title=govulncheck::UNMEASURED: exit 3 but listed $n_ids vulnerability ids against a stated count of '${stated:-none}'; this is a failure of the check, not a finding about the tree" >&2
    return 2
  fi
  while IFS= read -r id; do
    [ "$id" = "$WAIVED_ID" ] || bad="$bad $id"
  done <<< "$ids"
  if [ -n "$bad" ]; then
    echo "::error title=govulncheck::reachable vulnerabilities other than the waived $WAIVED_ID:$bad" >&2
    return 1
  fi
  # Both are validated YYYY-MM-DD, so string order is date order.
  if [[ "$today" > "$WAIVER_EXPIRES" ]]; then
    echo "::error title=govulncheck::the waiver for $WAIVED_ID expired after $WAIVER_EXPIRES (today $today); see $WAIVER_ISSUE" >&2
    return 1
  fi
  echo "::warning title=govulncheck::$WAIVED_ID is WAIVED through $WAIVER_EXPIRES ($WAIVER_ISSUE); the advisory looks reachable and the waiver is not a claim that it is not"
  return 0
}

run_module() {
  local dir="$1" out rc
  out="$(mktemp)"
  ( cd "$dir" && "${GOVULNCHECK:-govulncheck}" ./... ) >"$out" 2>&1
  rc=$?
  cat "$out"
  classify "$rc" "$out" "$(date -u +%F)"
  rc=$?
  rm -f "$out"
  return $rc
}

self_test() {
  local d fails=0 rc
  d="$(mktemp -d)"
  trap 'rm -rf "$d"' RETURN

  block() { # block ID -- one reachable-finding block in govulncheck's text form
    printf 'Vulnerability #%s: %s\n    some title\n  More info: https://pkg.go.dev/vuln/%s\n  Standard library\n    Found in: net/http@go1.27.3\n    Fixed in: net/http@go1.27.4\n    Example traces found:\n      #1: a.go:1:1: p.F calls http.Get\n\n' "$1" "$2" "$2"
  }
  summary() { printf 'Your code is affected by %s vulnerabilit%s from the Go standard library.\nexit status 3\n' "$1" "$([ "$1" = 1 ] && echo y || echo ies)"; }

  { echo '=== Symbol Results ==='; echo; block 1 GO-2026-6859; summary 1; } >"$d/only"
  { echo '=== Symbol Results ==='; echo; block 1 GO-2026-6859; block 2 GO-2026-5856; summary 2; } >"$d/second"
  { echo '=== Symbol Results ==='; echo; block 1 GO-2026-5856; summary 1; } >"$d/other"
  { echo '=== Symbol Results ==='; echo; block 1 GO-2026-6859; summary 2; } >"$d/miscount"
  { echo 'something unrecognised'; summary 1; } >"$d/noids"
  echo 'No vulnerabilities found.' >"$d/clean"

  # expect NAME WANT-STATUS WANT-TEXT RC FILE TODAY -- asserts the status AND a
  # fragment of the message, so a case that exits right for the wrong reason
  # (an unrelated check supplying the status) is reported rather than passed.
  expect() {
    local name="$1" want="$2" text="$3" rc_in="$4" file="$5" today="$6" got err
    err="$(classify "$rc_in" "$file" "$today" 2>&1)"
    got=$?
    if [ "$got" = "$want" ] && grep -qF -- "$text" <<<"$err"; then
      printf '  ok    %-44s rc=%s\n' "$name" "$got"
    else
      printf '  FAIL  %-44s rc=%s want=%s text-%s: %s\n' "$name" "$got" "$want" \
        "$(grep -qF -- "$text" <<<"$err" && echo found || echo MISSING)" "$err"
      fails=$((fails + 1))
    fi
  }

  expect "clean run"                               0 "no reachable"                 0 "$d/clean"  2026-11-12
  expect "only the waived id, inside the window"   0 "is WAIVED through 2026-12-11" 3 "$d/only"   2026-11-12
  expect "waived id on the last valid day"         0 "is WAIVED through 2026-12-11" 3 "$d/only"   2026-12-11
  expect "waived id the day after expiry"          1 "expired after 2026-12-11"     3 "$d/only"   2026-12-12
  expect "waived id long after expiry"             1 "expired after 2026-12-11"     3 "$d/only"   2027-06-01
  expect "a second advisory beside the waived one" 1 "other than the waived GO-2026-6859: GO-2026-5856" 3 "$d/second" 2026-11-12
  expect "only a different advisory"               1 "other than the waived GO-2026-6859: GO-2026-5856" 3 "$d/other"  2026-11-12
  expect "count disagrees with the ids listed"     2 "UNMEASURED"                   3 "$d/miscount" 2026-11-12
  expect "exit 3 with no ids parsed"               2 "UNMEASURED"                   3 "$d/noids"  2026-11-12
  expect "govulncheck failed outright (rc 1)"      2 "UNMEASURED"                   1 "$d/only"   2026-11-12
  expect "unreadable today"                        2 "cannot read a date"           3 "$d/only"   "yesterday"

  # The waiver's own constants are part of the contract.
  if [ "$WAIVED_ID" = "GO-2026-6859" ] && [ "$WAIVER_EXPIRES" = "2026-12-11" ]; then
    echo "  ok    waiver constants are as decided in D69"
  else
    echo "  FAIL  waiver constants changed: $WAIVED_ID $WAIVER_EXPIRES"; fails=$((fails + 1))
  fi

  if [ "$fails" -ne 0 ]; then echo "govulncheck-gate self-test: $fails failed" >&2; return 1; fi
  echo "govulncheck-gate self-test: all passed"
}

case "${1:-}" in
  --self-test) self_test; exit $? ;;
  --classify)
    [ $# -eq 4 ] || { echo "usage: $0 --classify RC FILE TODAY" >&2; exit 2; }
    classify "$2" "$3" "$4"; exit $? ;;
  ""|-*) echo "usage: $0 <module-dir> | --classify RC FILE TODAY | --self-test" >&2; exit 2 ;;
  *) run_module "$1"; exit $? ;;
esac
