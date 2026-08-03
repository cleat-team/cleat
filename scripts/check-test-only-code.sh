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
#
# The analysis is pinned to GOOS=linux so the baseline is portable. Build
# constraints mean staticcheck sees a different set of files per platform, so a
# baseline generated on darwin does not match one generated on the CI runner
# and the difference surfaces as phantom "new" entries.
#
# The tool itself must still be built for the host: `GOOS=linux go run` would
# cross-compile staticcheck and then fail to execute it ("exec format error").
# So it is installed for the host first and only its analysis is retargeted.
#
# LC_ALL=C pins the collation, which otherwise differs between a developer's
# locale and the runner's.
TOOLDIR="$(mktemp -d)"
trap 'rm -rf "$TOOLDIR"' EXIT

scan() {
  if [ ! -x "$TOOLDIR/staticcheck" ]; then
    if ! GOBIN="$TOOLDIR" go install "$STATICCHECK" >&2; then
      echo "ERROR: could not install $STATICCHECK" >&2
      exit 1
    fi
  fi

  local out
  out="$(LC_ALL=C GOOS=linux CGO_ENABLED=0 "$TOOLDIR/staticcheck" \
    -checks=U1000 -tests=false ./... 2>&1)"

  local findings
  findings="$(printf '%s\n' "$out" |
    grep '(U1000)$' |
    sed -E 's|^([^:]*)/[^/:]*\.go:[0-9]+:[0-9]+: (.*) is unused \(U1000\)$|\1\t\2|' |
    LC_ALL=C sort -u)"

  # A scan that finds nothing is far more likely to be a broken scan than a
  # clean tree -- a cross-compile failure, a build error, a changed message
  # format. Treating that as "no findings" would leave the guard passing
  # vacuously, which is the exact failure mode it exists to catch. This repo
  # has a real backlog, so zero is never legitimate.
  if [ -z "$findings" ]; then
    echo "ERROR: staticcheck reported no U1000 findings at all." >&2
    echo "That almost certainly means the scan failed rather than that the" >&2
    echo "tree is clean. Raw output follows:" >&2
    printf '%s\n' "$out" | head -20 | sed 's/^/    /' >&2
    # NOT exit: scan runs inside a command substitution, so exit would only
    # leave the subshell and the caller would carry on with an empty result
    # and report OK -- a vacuous pass by the guard against vacuous passes.
    # Callers check for the sentinel instead.
    echo "$SCAN_FAILED"
    return
  fi

  printf '%s\n' "$findings"
}

# Emitted by scan() when it produced nothing, so callers can distinguish a
# failed scan from a clean tree across the command-substitution boundary.
SCAN_FAILED="__scan_failed__"

die_if_scan_failed() {
  if [ "$1" = "$SCAN_FAILED" ]; then
    exit 1
  fi
}

if [ "${1:-}" = "--update" ]; then
  fresh="$(scan)"
  die_if_scan_failed "$fresh"
  printf '%s\n' "$fresh" > "$BASELINE"
  echo "Wrote $(wc -l < "$BASELINE" | tr -d ' ') entries to $BASELINE"
  exit 0
fi

if [ ! -f "$BASELINE" ]; then
  echo "ERROR: $BASELINE is missing. Generate it with:" >&2
  echo "  scripts/check-test-only-code.sh --update" >&2
  exit 1
fi

current="$(scan)"
die_if_scan_failed "$current"

# Anything present now but absent from the baseline is new.
#
# grep -Fxv rather than comm: comm requires both inputs to be sorted in the
# *same* collation it uses, and silently emits "file 1 is not in sorted order"
# plus garbage results when they disagree -- which is exactly what happened
# between a darwin-generated baseline and the CI runner. A set-membership test
# has no ordering requirement at all.
new="$(printf '%s\n' "$current" | grep -Fxv -f "$BASELINE" || true)"

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
