#!/usr/bin/env bash
#
# The per-stream section-number allocation, in one place because two scripts
# and three humans read it.
#
# WHY BLOCKS. Three streams appended sections to IMPROVEMENT-PLAN.md by picking
# "the next free number" against a develop that had already moved. The
# allocation is invisible in the file you edited, so the collision exists only
# in the merge -- and a duplicate heading renders perfectly. Four landed on
# 2026-09-02 alone (1.3, 2.70, 3.77, 3.78). check-section-numbers.sh detects
# them; blocks make them impossible, because two streams drawing from disjoint
# ranges cannot pick the same number however stale their view of develop is.
#
# WHY NOT 3.1xx/3.2xx/3.3xx, which is what WORKSTREAM.md's R2 originally said.
# 3.1xx was already occupied when the rule was written -- 3.100 through 3.114
# existed, allocated sequentially by all three streams before any block scheme:
#
#   3.100  #634   3.107  #647   3.113  #684 (WS-1)   3.114  #687 (WS-2)
#
# Re-derive any of them with
#   git log -S'### 3.114 ' --format='%h %s' --reverse -- IMPROVEMENT-PLAN.md | head -1
#
# So handing 3.1xx to WS-1 would have retroactively assigned three streams' work
# to one stream and left the rule unenforceable on its first day. The blocks
# below start above the high-water mark instead.
#
# Measured 2026-09-04: 98 sections in the 3.x series, spanning 3.10 to 3.114.
#   grep -oE '^### 3\.[0-9]+ ' IMPROVEMENT-PLAN.md | sed 's/^### 3\.//; s/ $//' \
#     | sort -n | awk '{a[NR]=$1} END{print NR, a[1], a[NR]}'
#
# Everything at or below GRANDFATHER_MAX predates the scheme and is left alone.
# The number is a fact about history, not a policy: it does not move, and a new
# section may not take 3.115 to squeeze under it.

# Highest 3.x section allocated before blocks existed. Do not raise this.
GRANDFATHER_MAX=114

# stream:low-high. Disjoint by construction; check-section-numbers.sh --self-test
# asserts the boundaries rather than trusting them.
SECTION_BLOCKS=(
  "WS-1:200-299"
  "WS-2:300-399"
  "WS-3:400-499"
)

# Which sandbox is which stream. Keyed on the git COMMON directory rather than
# $PWD for two measured reasons:
#
#   * the same tree is reachable as /localssd/rcownie/cleat and as
#     /Users/Shared/localssd/rcownie/cleat, and a $PWD match would see two
#     different checkouts; --path-format=absolute --git-common-dir normalises
#     both to the /Users/Shared form.
#   * there are 14 cleat-wt-* worktrees. A worktree of WS-3's checkout must
#     answer WS-3, and --git-common-dir resolves it there: from
#     /localssd/rcownie/cleat-wt-B it prints .../cleat-agent2/.git.
#
# Re-derive: git rev-parse --path-format=absolute --git-common-dir
SECTION_SANDBOXES=(
  "WS-1:/Users/Shared/localssd/rcownie/cleat"
  "WS-2:/Users/Shared/localssd/rcownie/cleat-agent1"
  "WS-3:/Users/Shared/localssd/rcownie/cleat-agent2"
)

# section_block_for <number> -> "WS-N" | "grandfathered" | "unallocated"
section_block_for() {
  local n="$1" spec stream range lo hi
  if [ "$n" -le "$GRANDFATHER_MAX" ]; then
    echo "grandfathered"
    return
  fi
  for spec in "${SECTION_BLOCKS[@]}"; do
    stream="${spec%%:*}"
    range="${spec#*:}"
    lo="${range%%-*}"
    hi="${range##*-}"
    if [ "$n" -ge "$lo" ] && [ "$n" -le "$hi" ]; then
      echo "$stream"
      return
    fi
  done
  echo "unallocated"
}

# section_stream_for_checkout -> "WS-N" | "" (unknown)
section_stream_for_checkout() {
  local common spec stream path
  common="$(git rev-parse --path-format=absolute --git-common-dir 2>/dev/null)" || return 0
  common="${common%/.git}"
  common="${common%/}"
  for spec in "${SECTION_SANDBOXES[@]}"; do
    stream="${spec%%:*}"
    path="${spec#*:}"
    if [ "$common" = "$path" ]; then
      echo "$stream"
      return
    fi
  done
  echo ""
}

# section_blocks_summary -> one "WS-N  low-high" line per stream, for messages.
section_blocks_summary() {
  local spec
  for spec in "${SECTION_BLOCKS[@]}"; do
    printf '    %s  3.%s\n' "${spec%%:*}" "${spec#*:}"
  done
}
