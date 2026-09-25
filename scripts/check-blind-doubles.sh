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
#   scripts/check-blind-doubles.sh             # fail on anything not baselined
#   scripts/check-blind-doubles.sh --update    # re-record the baseline
#   scripts/check-blind-doubles.sh --self-test # assert the scanner and the
#                                              # stale-entry check both work
set -euo pipefail

cd "$(dirname "$0")/.."
BASELINE="scripts/blind-doubles-baseline.txt"

# stale_entries prints the baseline lines the current scan does not produce.
# cleat#1746's shape; cleat#1762 is the same defect on the sibling guard.
#
# WHY THE CEILING ALONE IS NOT ENOUGH. The comparison below asks only
# "findings - baseline". A baseline entry the scanner stopped producing is
# invisible to it, and here that entry is a standing EXEMPTION: this baseline is
# explicitly not a to-do list, its entries are doubles reviewed and judged
# deliberate. Delete such a test, rename it, or change the rule that reported it,
# and the exemption stays -- covering whatever arrives at that key next.
#
# AND IT SUBSUMES A VACUITY CHECK THIS SCRIPT DID NOT HAVE. The stderr guard
# above catches a scanner that COMPLAINS. A scanner that exits 0, writes nothing
# to stderr and produces no findings at all is a clean pass today: `new` is empty
# and the run reports OK. With this comparison in place an empty scan makes
# every baseline entry stale and the guard fails loudly, which is the right
# answer for a scan that measured nothing.
#
# WHOLE-LINE, so a baseline key cannot be satisfied by a longer finding that
# merely contains it. grep -Fxv rather than comm: comm requires both inputs
# sorted in its own collation and emits garbage when they disagree.
#
# A SEPARATE FUNCTION so --self-test can drive it against fixtures.
stale_entries() {
  local findings="$1" baseline="${2:-$BASELINE}"
  grep -Fxv -f <(printf '%s\n' "$findings") "$baseline" | grep -v '^[[:space:]]*$' || true
}

# --self-test drives the REAL scanner against a fixture whose answer is known
# independently, and then drives stale_entries against fixtures of its own.
#
# WHY IT HAS TO BE THE REAL SCANNER. cleat#1743's point is that a guard which
# regenerates its baseline from its own scan cannot detect a systematic error in
# itself: --update reproduces the error faithfully and the totals balance either
# way. Nothing here asserted what findblinddoubles.go ATTRIBUTES, so a rule that
# silently stopped matching would have produced a shrinking baseline and a green
# run -- and this baseline is already small enough that nobody would notice.
#
# THE FIXTURE CARRIES BOTH DIRECTIONS IN ONE FILE. A double that names
# idempotencyKey and never reads it, in a test named for it, must be REPORTED; a
# double that reads its parameter must NOT be. A scanner that reports everything
# and one that reports nothing are both refused, and neither is caught by a
# fixture with only the positive case in it.
#
# git init, because findblinddoubles.go enumerates through `git ls-files` -- the
# CLAUDE.md preference over find/rglob, so that a scratch checkout under the
# tree cannot be mistaken for the repo. Outside a repository it exits 128, which
# is a failure this self-test would otherwise report as "reported nothing".
if [ "${1:-}" = "--self-test" ]; then
  ok=0
  fx="$(mktemp -d)"
  mkdir -p "$fx/pkg"
  cat > "$fx/pkg/double_test.go" <<'GOFIXTURE'
package pkg

import "testing"

type store struct {
	startFn func(idempotencyKey string) string
}

// The defect: names idempotencyKey, never reads it, and the test is named for
// it. This must be reported.
func TestStartWorkflow_WithIdempotencyKey(t *testing.T) {
	s := store{startFn: func(idempotencyKey string) string { return "wf" }}
	if s.startFn("k") != "wf" {
		t.Fatal("no")
	}
}

// The control: reads its parameter, so it can fail for the reason its name
// gives. This must NOT be reported.
func TestStartWorkflow_WithTenantID(t *testing.T) {
	s := store{startFn: func(tenantID string) string { return tenantID }}
	if s.startFn("k") != "k" {
		t.Fatal("no")
	}
}
GOFIXTURE
  git -C "$fx" init -q . >/dev/null 2>&1
  git -C "$fx" add -A >/dev/null 2>&1

  st_out="$(go run scripts/findblinddoubles.go "$fx" 2>&1 || true)"

  # ON THE TEXT, not on a count. A scanner reporting one finding of the wrong
  # kind satisfies "exactly one line" and tells you nothing.
  if ! grep -q 'TestStartWorkflow_WithIdempotencyKey.*idempotencyKey' <<< "$st_out"; then
    echo "SELF-TEST FAIL: the blind double was not reported, or not attributed to" >&2
    echo "  its test and parameter. Scanner output was:" >&2
    printf '%s\n' "$st_out" | sed 's/^/    /' >&2
    ok=1
  fi
  if grep -q 'WithTenantID' <<< "$st_out"; then
    echo "SELF-TEST FAIL: a double that READS its parameter was reported." >&2
    printf '%s\n' "$st_out" | sed 's/^/    /' >&2
    ok=1
  fi

  # stale_entries, against fixtures. A known-positive first: "it says nothing on
  # a healthy tree" is satisfied by a version that says nothing ever, which is
  # what this guard shipped with.
  st_base="$fx/baseline.txt"
  printf 'a_test.go\tTestLive\tlimit\nb_test.go\tTestGoneAway\tpayload\n' > "$st_base"
  st_got="$(stale_entries "$(printf 'a_test.go\tTestLive\tlimit\n')" "$st_base")"
  if ! grep -qF 'TestGoneAway' <<< "$st_got"; then
    echo "SELF-TEST FAIL: a baseline entry the scanner does not produce was not reported." >&2
    ok=1
  fi
  # The negative control: report a live entry and every run fails, which gets
  # the guard switched off.
  if grep -qF 'TestLive' <<< "$st_got"; then
    echo "SELF-TEST FAIL: a baseline entry the scanner DOES produce was reported stale." >&2
    ok=1
  fi
  # Substring safety, and BOTH the direction and the FIELD matter. Without -x a
  # baseline line is matched whenever any finding appears ANYWHERE in it, so a
  # SHORT finding vouches for a LONGER baseline entry -- the baseline therefore
  # holds the long line and the scan produces the short one.
  #
  # The containment has to be a real prefix of the WHOLE line, which is why this
  # varies the last field. Written first as TestPurgeVersion against
  # TestPurgeVersion_Cancelled -- two names that genuinely coexist in this
  # baseline -- it passed under a deliberately broken -F, because the third
  # field follows the test name and `...\tTestPurgeVersion\tversion` is not
  # contained in `...\tTestPurgeVersion_Cancelled\tversion`. Same-arity lines
  # cannot differ that way. A parameter pair can: `version` and `versionID`.
  printf 'a_test.go\tTestPurgeVersion\tversionID\n' > "$st_base"
  if ! stale_entries "$(printf 'a_test.go\tTestPurgeVersion\tversion\n')" "$st_base" |
      grep -qF 'versionID'; then
    echo "SELF-TEST FAIL: a baseline entry was satisfied by a finding it merely contains." >&2
    ok=1
  fi
  # The vacuity case, stated as its own assertion because it is the one this
  # script had no cover for: a scanner that produces NOTHING must not read as a
  # clean tree.
  if [ -z "$(stale_entries "" "$st_base")" ]; then
    echo "SELF-TEST FAIL: an empty scan left every baseline entry unreported." >&2
    ok=1
  fi

  rm -rf "$fx"
  if [ "$ok" -ne 0 ]; then
    exit 1
  fi
  echo "OK: self-test passed -- the scanner reports a blind double and spares a"
  echo "    reading one, and a baseline entry it no longer produces is reported."
  exit 0
fi

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

# And the other direction: an exemption covering nothing.
stale="$(stale_entries "$findings")"

if [ -n "$stale" ]; then
  echo "ERROR: $BASELINE lists entries the scanner no longer reports:" >&2
  echo >&2
  printf '%s\n' "$stale" | sed 's/^/  /' >&2
  echo >&2
  echo "Each is a standing exemption for a double that is no longer reported --" >&2
  echo "the test was renamed, deleted, or fixed. Nothing is wrong with the tree." >&2
  echo "Left in place the exemption outlives what it exempted and silently covers" >&2
  echo "whatever arrives at that key next." >&2
  echo >&2
  echo "If ALL of them are listed here, suspect the scanner rather than the tests:" >&2
  echo "an empty scan makes every entry stale, which is what this reports." >&2
  echo >&2
  echo "Refresh with:" >&2
  echo "  scripts/check-blind-doubles.sh --update" >&2
  echo "Rebase FIRST -- a regeneration from a stale base reinstates whatever the" >&2
  echo "other side removed, and merges cleanly doing it (WORKSTREAM.md R6a)." >&2
  exit 1
fi

echo "OK: no new blind doubles ($(grep -c . "$BASELINE" || true) known entries in the baseline, none stale)."
