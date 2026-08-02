#!/usr/bin/env bash
#
# Detect code that only tests call.
#
# The failure mode this repo keeps hitting is not broken code -- it is code
# that is written, tested, passing, and wired to nothing. Some examples found
# in one scan:
#
#   engine/flush.go       (*Engine).flushCallIntent
#       The write half of crash recovery. The read half is live and correct,
#       so the detector looks for a sentinel that nothing ever writes. In a
#       real crash there is nothing to find. (IMPROVEMENT-PLAN.md 1.4)
#
#   engine/mssql_retry.go mssqlRetry, and engine/mssql_errors.go's whole
#       error-classification family. SQL Server transient-error retry, with
#       ~12 passing test cases covering deadlock retry, backoff, context
#       cancellation and retry exhaustion -- and no production caller. Every
#       SQL Server deadlock surfaces as a hard error.
#
# A test suite cannot catch this: the tests pass precisely because they are
# the only callers. staticcheck's U1000 can, if you tell it to ignore test
# files -- then anything used only from _test.go reads as unused.
#
# U1000 does not report exported identifiers in library packages, so a public
# API with no internal caller is not flagged. That is the desired behaviour
# here.
#
# Usage:
#   scripts/check-test-only-code.sh              # fail on entries not in the baseline
#   scripts/check-test-only-code.sh --update     # rewrite the baseline
#
# The baseline exists because there is a backlog. New entries fail the build;
# clearing the existing ones is tracked in IMPROVEMENT-PLAN.md. Removing an
# entry from the baseline (by wiring the code up or deleting it) never fails.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

BASELINE="scripts/deadcode-baseline.txt"

# Pinned: an unpinned @latest would turn an upstream release into a
# spontaneous CI failure on an unrelated PR.
STATICCHECK="honnef.co/go/tools/cmd/staticcheck@2025.1.1"

# Key on "<package dir><TAB><symbol>" rather than the raw staticcheck line,
# so that moving a function within its file does not churn the baseline.
# Input:  engine/flush.go:186:18: func (*Engine).flushCallIntent is unused (U1000)
# Output: engine<TAB>func (*Engine).flushCallIntent
scan() {
  CGO_ENABLED=0 go run "$STATICCHECK" -checks=U1000 -tests=false ./... 2>&1 |
    grep '(U1000)$' |
    sed -E 's|^([^:]*)/[^/:]*\.go:[0-9]+:[0-9]+: (.*) is unused \(U1000\)$|\1\t\2|' |
    sort -u
}

if [ "${1:-}" = "--update" ]; then
  scan > "$BASELINE"
  echo "Wrote $(wc -l < "$BASELINE" | tr -d ' ') entries to $BASELINE"
  exit 0
fi

if [ ! -f "$BASELINE" ]; then
  echo "ERROR: $BASELINE is missing. Generate it with:" >&2
  echo "  scripts/check-test-only-code.sh --update" >&2
  exit 1
fi

current="$(scan)"

# Anything present now but absent from the baseline is new.
new="$(comm -13 "$BASELINE" <(printf '%s\n' "$current"))"

if [ -n "$new" ]; then
  echo "ERROR: new code that only tests reference:" >&2
  echo >&2
  printf '%s\n' "$new" | sed 's/^/  /' >&2
  echo >&2
  echo "Either wire it into production, delete it, or -- if it is genuinely" >&2
  echo "meant to be called only from tests -- add it to $BASELINE with" >&2
  echo "  scripts/check-test-only-code.sh --update" >&2
  echo "and say why in the commit message." >&2
  exit 1
fi

echo "OK: no new test-only code ($(printf '%s\n' "$current" | grep -c . ) known entries in the baseline)."
