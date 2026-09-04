#!/usr/bin/env bash
#
# Print the next free IMPROVEMENT-PLAN.md section number in THIS sandbox's block.
#
# Usage:
#   scripts/next-section-number.sh              # infer the stream from the checkout
#   scripts/next-section-number.sh --stream WS-2
#   scripts/next-section-number.sh --all        # every stream's next free number
#
# This is the half of the block scheme that keeps a stream inside its own range.
# check-section-numbers.sh can only assert that a number sits in SOME block --
# it runs on a shallow checkout and cannot tell who added what (see its header).
# So the gate catches "3.115", and this helper is what stops "WS-2 takes 3.201".
# It is a helper, not a gate, and it is deliberately one command with no
# arguments, because the failure it prevents is someone counting by hand.
#
# IT READS origin/develop, NOT YOUR BRANCH. The number free in your working copy
# is not the number free in the repository -- that gap is the entire original
# defect. If origin/develop cannot be read the script says so and exits non-zero
# rather than falling back to the local file, because a fallback here would
# answer confidently with the stale number this exists to avoid.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT" || exit 1

# shellcheck source=scripts/section-blocks.sh
. "$REPO_ROOT/scripts/section-blocks.sh"

DOC="IMPROVEMENT-PLAN.md"

want_stream=""
show_all=0
while [ $# -gt 0 ]; do
  case "$1" in
    --stream) want_stream="${2:-}"; shift 2 ;;
    --all) show_all=1; shift ;;
    -h|--help) sed -n '2,25p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

plan="$(git show "origin/develop:$DOC" 2>/dev/null)"
if [ -z "$plan" ]; then
  echo "ERROR: cannot read $DOC from origin/develop." >&2
  echo "Run 'git fetch origin' first. This script will not fall back to the" >&2
  echo "working copy: the number free in your branch is not the number free in" >&2
  echo "the repository, and answering from the wrong one is the collision this" >&2
  echo "whole scheme exists to prevent." >&2
  exit 1
fi

used="$(printf '%s\n' "$plan" | grep -oE '^### 3\.[0-9]+ ' | sed 's/^### 3\.//; s/ $//' | sort -n)"
used_count="$(printf '%s\n' "$used" | grep -c . || true)"

# Same vacuity guard as the checker: a grep that matched nothing would make
# every number look free, and this script would confidently hand out 3.200 to
# all three streams at once.
if [ "$used_count" -lt 50 ]; then
  echo "ERROR: found only $used_count '### 3.N' headings on origin/develop," >&2
  echo "against 98 on 2026-09-04. The heading format has probably changed and" >&2
  echo "this script is reading nothing -- every number would look free. Fix the" >&2
  echo "pattern rather than lowering the floor." >&2
  exit 1
fi

next_in_block() {
  local stream="$1" spec range lo hi n
  for spec in "${SECTION_BLOCKS[@]}"; do
    [ "${spec%%:*}" = "$stream" ] || continue
    range="${spec#*:}"
    lo="${range%%-*}"
    hi="${range##*-}"
    for ((n = lo; n <= hi; n++)); do
      if ! printf '%s\n' "$used" | grep -qx "$n"; then
        echo "$n"
        return 0
      fi
    done
    echo "ERROR: block 3.$lo-$hi is full for $stream." >&2
    return 1
  done
  echo "ERROR: no block declared for '$stream'." >&2
  return 1
}

if [ "$show_all" -eq 1 ]; then
  echo "Next free section number per stream, against origin/develop:"
  echo
  for spec in "${SECTION_BLOCKS[@]}"; do
    stream="${spec%%:*}"
    n="$(next_in_block "$stream")" || exit 1
    printf '    %s  3.%s   (block 3.%s)\n' "$stream" "$n" "${spec#*:}"
  done
  exit 0
fi

if [ -z "$want_stream" ]; then
  want_stream="$(section_stream_for_checkout)"
fi

if [ -z "$want_stream" ]; then
  common="$(git rev-parse --path-format=absolute --git-common-dir 2>/dev/null)"
  echo "ERROR: cannot tell which stream this checkout belongs to." >&2
  echo "Its git common directory is:" >&2
  echo "    ${common:-<unknown>}" >&2
  echo "which matches no entry in scripts/section-blocks.sh. Either pass the" >&2
  echo "stream explicitly:" >&2
  echo "    scripts/next-section-number.sh --stream WS-2" >&2
  echo "or add this sandbox to SECTION_SANDBOXES if it is a new one." >&2
  exit 1
fi

n="$(next_in_block "$want_stream")" || exit 1
echo "3.$n"
