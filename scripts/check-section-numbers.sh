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
# Usage: scripts/check-section-numbers.sh
#
# Re-derive by hand:
#   grep -oE '^### [0-9]+\.[0-9]+ ' IMPROVEMENT-PLAN.md | sort | uniq -d

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT" || exit 1

DOC="IMPROVEMENT-PLAN.md"

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

echo "OK: all $count section numbers in $DOC are unique."
