#!/usr/bin/env bash
#
# Refuse NEW test doubles that cannot see the argument their test is named for.
#
# The defect, from cleat#1167. POST /api/dead-letters/{id}/reprocess never read
# Idempotency-Key, so an operator retry after a lost response re-drove work that
# had already failed partway. Two tests in that package asserted idempotency and
# both passed on the broken code, because the double they drove was:
#
#   ms.startNewRunFn = func(_ context.Context, runID, defName string, defVersion int,
#       input json.RawMessage, idempotencyKey, tenantID string, priority int) (string, bool, error) {
#       return "wf-existing", true, nil
#   }
#
# It names idempotencyKey, never reads it, and answers "already started" whatever
# it is handed. Such a test asserts that the handler RENDERS the already-started
# branch, never that anything can reach it. Replacing the working start handler's
# header read with "" left both tests green -- so "the header is read at all" was
# under test nowhere in cmd/cleat-worker, which is how the reprocess bug survived
# next door to a correct implementation.
#
# WHAT THIS GATE IS, AND IS NOT
#
# It is a ratchet, not a defect detector. Read scripts/findblinddoubles.go for
# the rule; what matters here is that a flagged double is a CANDIDATE for review,
# not a proven fault. Some are deliberate and correct:
# TestGetCompactionCandidates_LimitEnforcement drives a double that ignores
# `limit` and returns three candidates for a limit of two, precisely so the
# assertion can prove the sharded wrapper truncates. Ignoring the argument is the
# point of that test.
#
# So the baseline is NOT a to-do list and entries should not be "fixed" on sight.
# The gate exists because the cheapest moment to ask "can this double see the
# thing the test is named after?" is when the double is being written.
#
# THE CHECK THIS DOES NOT REPLACE
#
# Whole classes are invisible here. TestHandleDeadLetterReprocess_AlreadyExisted
# had exactly the #1167 defect and is not reported, because "AlreadyExisted"
# shares no identifier with "idempotencyKey". A name-based rule can only see
# doubles whose test was named after the parameter.
#
# The check that has no such blind spot is sabotage: neuter the read in the
# production code, run the suite, and see whether anything goes red. Four of the
# seven /api/workflows list filters turned out to be guarded by nothing that way
# (cleat#1248), and a test literally named TestTheTargetedFiltersReachTheStore
# covered exactly three. Use that when the question is "is this parameter
# guarded?". CONTRIBUTING.md carries the procedure.
#
# Usage:
#   scripts/check-blind-doubles.sh            # fail on anything not baselined
#   scripts/check-blind-doubles.sh --update   # re-record the baseline
set -euo pipefail

cd "$(dirname "$0")/.."
BASELINE="scripts/blind-doubles-baseline.txt"

TMPOUT="$(mktemp)"
TMPERR="$(mktemp)"
trap 'rm -f "$TMPOUT" "$TMPERR"' EXIT

if ! go run scripts/findblinddoubles.go . > "$TMPOUT" 2> "$TMPERR"; then
  echo "ERROR: findblinddoubles failed to run:" >&2
  cat "$TMPERR" >&2
  exit 1
fi

# A parse error must not read as "no findings" -- an empty result is the most
# persuasive output there is, and it is what a broken checker produces too.
if [ -s "$TMPERR" ]; then
  echo "ERROR: findblinddoubles reported problems; refusing to treat its output as complete:" >&2
  cat "$TMPERR" >&2
  exit 1
fi

# Drop the line number: a double keeps its identity when code above it moves.
findings="$(awk -F'\t' '{print $1"\t"$3"\t"$4}' "$TMPOUT" | LC_ALL=C sort -u)"

if [ "${1:-}" = "--update" ]; then
  printf '%s\n' "$findings" > "$BASELINE"
  echo "Wrote $(grep -c . "$BASELINE" || true) entries to $BASELINE"
  exit 0
fi

if [ ! -f "$BASELINE" ]; then
  echo "ERROR: $BASELINE is missing. Create it with --update." >&2
  exit 1
fi

new="$(printf '%s\n' "$findings" | grep -Fxv -f "$BASELINE" || true)"
new="$(printf '%s' "$new" | grep -v '^$' || true)"

if [ -n "$new" ]; then
  echo "ERROR: new test double(s) blind to the parameter their test is named for:" >&2
  echo >&2
  printf '%s\n' "$new" | sed 's/^/  /' >&2
  echo >&2
  echo "The double declares that parameter and never reads it, so the test cannot" >&2
  echo "fail for the reason its name gives. Either have the double RECORD what it" >&2
  echo "was handed and assert on it, or -- if ignoring the argument is the point of" >&2
  echo "the test, as in a truncation check -- add it with a reason in the commit" >&2
  echo "message via" >&2
  echo "  scripts/check-blind-doubles.sh --update" >&2
  exit 1
fi

echo "OK: no new blind doubles ($(grep -c . "$BASELINE" || true) known entries in the baseline)."
