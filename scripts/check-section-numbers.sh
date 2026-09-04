#!/usr/bin/env bash
#
# Assert that every `### N.M` section number in IMPROVEMENT-PLAN.md is unique.
#
# Three workstreams append sections to this file concurrently, each picking
# "the next free number" against a develop that has already moved. The
# allocation is invisible in the file you edited, so the collision only exists
# in the merge -- and a duplicate heading renders perfectly, so nothing catches
# it. Measured 2026-09-02, all in one day:
#
#   * 3.77 was allocated by WS-1 (#579) and by WS-3 (#580) simultaneously.
#   * 3.78 was allocated by WS-1 (#582) and by WS-2 (#583); fixed in #587 by
#     moving WS-2's to 3.80 -- onto the number WS-3 had by then moved to, so
#     #580 had to renumber a second time, 3.77 -> 3.80 -> 3.81.
#   * 2.70 was allocated twice; fixed in #588.
#   * 1.3 was carried by both the cancellation section and a "1.3 residual"
#     section 45 lines above it. Fixed alongside this script by making the
#     residual a `####` subsection of 1.3, which is what 2.15 and 2.28 already
#     do with theirs.
#
# The cost is not the duplicate itself but the cross-references that rot around
# it. 3.79 read "found by 3.77's vacuity guard" while 3.77 was an unrelated
# WS-1 item about per-tenant names -- a pointer into real prose about the wrong
# thing, which is worse than a dead link because it reads as correct.
#
# Only `###` is checked. `####` subsections legitimately repeat a parent's
# number (`#### 1.4 phase D`, `#### 1.4 phase E`) and are not section numbers.
#
# SINCE 2026-09-04 THIS SCRIPT ALSO ENFORCES PER-STREAM BLOCKS. Detecting a
# duplicate is a diagnosis after the fact: it fires in the merge, by which time
# both branches exist and one of them has to renumber -- 3.77 renumbered twice.
# Blocks remove the mechanism instead. Two streams drawing from disjoint ranges
# cannot pick the same number however stale their view of develop is.
#
# The allocation lives in scripts/section-blocks.sh, with the measurements that
# chose it. Run scripts/next-section-number.sh to be told yours.
#
# WHAT THIS CHECK CANNOT DO, stated because a guard that overstates its reach is
# worse than none. It runs on a shallow checkout (only one job in ci.yml sets
# fetch-depth: 0, and it is not this one), so it cannot diff against develop and
# therefore cannot see WHICH numbers a PR added, or who added them. It reads the
# merged file and asserts that every 3.x section sits in some declared block.
# That is what closes the race -- a stream can no longer take "the next free
# integer", which is the move that collided -- but a stream that reaches into
# ANOTHER stream's block is caught only by the duplicate check above, and only
# once both branches have landed. The local helper is what prevents that, and it
# is a helper, not a gate.
#
# THAT LIMITATION WAS REALISED TWICE WITHIN HOURS OF THIS RULE LANDING, reported
# by WS-2 and confirmed on develop: 3.201 and 3.202 are WS-2's work inside
# WS-1's block, and 3.300 is WS-3's inside WS-2's. Both authors had started
# before the rule existed and never re-ran the helper. This gate stayed green
# for all three, exactly as the paragraph above said it would.
#
# .githooks/pre-commit now refuses such a commit in the sandbox, which is the
# only place the stream is known. It runs only where
# `git config core.hooksPath .githooks` has been set, so it is a second line of
# defence rather than the rule itself.
#
# Usage: scripts/check-section-numbers.sh [--self-test]
#
# --self-test runs the block classifier over known-good and known-bad numbers
# and asserts each verdict. CLAUDE.md's standing rule is that a verification
# script needs its own negative control: a classifier that returned "fine" for
# everything would pass this file's real check silently forever.
#
# Re-derive by hand:
#   grep -oE '^### [0-9]+\.[0-9]+ ' IMPROVEMENT-PLAN.md | sort | uniq -d

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT" || exit 1

# shellcheck source=scripts/section-blocks.sh
. "$REPO_ROOT/scripts/section-blocks.sh"

DOC="IMPROVEMENT-PLAN.md"

# The negative control. Each case is a boundary that the real check depends on
# being right, including the two that are easy to get wrong: 115 (the first
# number a "next free integer" habit would reach for) and 199 (the end of the
# range WORKSTREAM.md's R2 originally handed to WS-1, which is allocated to
# nobody now).
if [ "${1:-}" = "--self-test" ]; then
  fails=0
  ran=0
  while read -r n want; do
    [ -n "$n" ] || continue
    ran=$((ran + 1))
    got="$(section_block_for "$n")"
    if [ "$got" != "$want" ]; then
      echo "SELF-TEST FAIL: 3.$n classified as '$got', want '$want'" >&2
      fails=$((fails + 1))
    fi
  done <<'CASES'
10 grandfathered
114 grandfathered
115 unallocated
199 unallocated
200 WS-1
299 WS-1
300 WS-2
399 WS-2
400 WS-3
499 WS-3
500 unallocated
CASES
  # A while-read loop that read nothing would report zero failures, which is
  # this file's own trap.
  if [ "$ran" -ne 11 ]; then
    echo "SELF-TEST FAIL: ran $ran cases, expected 11 -- the case table was not read." >&2
    exit 1
  fi
  if [ "$fails" -gt 0 ]; then
    echo "SELF-TEST FAILED: $fails of $ran cases wrong." >&2
    exit 1
  fi
  echo "OK: self-test passed, all $ran block boundaries classify correctly."
  exit 0
fi

if [ ! -f "$DOC" ]; then
  echo "ERROR: $DOC not found" >&2
  exit 1
fi

headings="$(grep -oE '^### [0-9]+\.[0-9]+ ' "$DOC" | sed 's/^### //; s/ $//')"
count="$(printf '%s\n' "$headings" | grep -c . || true)"

# Vacuity guard. A grep that matches nothing reports success, so a heading
# format change would silently turn this script into a no-op that prints OK
# forever -- the exact failure mode CLAUDE.md's "Is this result real?" section
# is about. There were 120 sections on 2026-09-02; 50 is a floor well below
# that which still fails loudly if the format moves.
if [ "$count" -lt 50 ]; then
  echo "ERROR: found only $count '### N.M' headings in $DOC." >&2
  echo "That is far below the ~120 that exist; the heading format has probably" >&2
  echo "changed and this guard is no longer looking at anything. Fix the" >&2
  echo "pattern in $0 rather than lowering the floor." >&2
  exit 1
fi

dupes="$(printf '%s\n' "$headings" | sort | uniq -d)"

if [ -n "$dupes" ]; then
  echo "ERROR: duplicate section numbers in $DOC:" >&2
  echo >&2
  while IFS= read -r num; do
    [ -n "$num" ] || continue
    grep -nE "^### ${num//./\\.} " "$DOC" | sed 's/^/    /' >&2
  done <<<"$dupes"
  echo >&2
  echo "Two sections cannot share a number: every '§$dupes' reference elsewhere in" >&2
  echo "the plan becomes ambiguous, and pointers into the wrong section read as" >&2
  echo "correct prose rather than as broken links." >&2
  echo >&2
  echo "Pick the next free number from develop, not from your own branch:" >&2
  echo "    git show origin/develop:$DOC | grep -oE '^### [0-9]+\\.[0-9]+ ' | sort -V | tail -5" >&2
  echo "then re-grep the old number across $DOC *and* engine/*.go, which carry" >&2
  echo "section numbers in comments." >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Block check. Every 3.x section must sit in a declared block or be one of the
# grandfathered numbers that predate the scheme.
# ---------------------------------------------------------------------------

outside=""
grandfathered=0
while IFS= read -r num; do
  [ -n "$num" ] || continue
  case "$num" in
    3.*) ;;
    *) continue ;;
  esac
  n="${num#3.}"
  verdict="$(section_block_for "$n")"
  case "$verdict" in
    grandfathered) grandfathered=$((grandfathered + 1)) ;;
    unallocated) outside="$outside$num\n" ;;
  esac
done <<<"$headings"

# Vacuity guard for THIS check, separate from the count floor above. The floor
# proves headings were found; this proves the 3.x series was found and
# classified. If the series were ever renamed, every number would fall through
# the `3.*` case and the loop would report a clean bill of health over nothing.
# There were 98 grandfathered sections on 2026-09-04; 50 is a floor below that
# which still fails loudly if the series moves.
if [ "$grandfathered" -lt 50 ]; then
  echo "ERROR: only $grandfathered section numbers in the 3.x series were" >&2
  echo "classified, against 98 on 2026-09-04. The series has probably been" >&2
  echo "renamed and the block check is looking at nothing. Fix the pattern in" >&2
  echo "$0 rather than lowering this floor." >&2
  exit 1
fi

if [ -n "$outside" ]; then
  echo "ERROR: section number(s) outside every allocated block in $DOC:" >&2
  echo >&2
  while IFS= read -r num; do
    [ -n "$num" ] || continue
    grep -nE "^### ${num//./\\.} " "$DOC" | sed 's/^/    /' >&2
  done <<<"$(printf '%b' "$outside")"
  echo >&2
  echo "Sections are allocated in per-stream blocks, so that two streams" >&2
  echo "appending at once cannot pick the same number:" >&2
  echo >&2
  section_blocks_summary >&2
  echo >&2
  echo "3.1 through 3.$GRANDFATHER_MAX predate the scheme and are left alone." >&2
  echo "A new section may NOT take 3.$((GRANDFATHER_MAX + 1)) to squeeze under that" >&2
  echo "line -- that is the 'next free integer' move the blocks exist to stop." >&2
  echo >&2
  echo "Ask for your number rather than picking one:" >&2
  echo "    scripts/next-section-number.sh" >&2
  echo >&2
  echo "See scripts/section-blocks.sh for why the blocks start at 200." >&2
  exit 1
fi

echo "OK: all $count section numbers in $DOC are unique."
echo "OK: every 3.x section is grandfathered ($grandfathered) or in an allocated block."
