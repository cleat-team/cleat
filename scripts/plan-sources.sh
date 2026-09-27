#!/usr/bin/env bash
#
# Print every file that holds IMPROVEMENT-PLAN sections, newest namespace last.
#
# THE PLAN IS TWO THINGS SINCE cleat#1727. `IMPROVEMENT-PLAN.md` holds the
# closed sections and the phase tables; `IMPROVEMENT-PLAN.d/` holds one file per
# OPEN section. The split exists because three streams appending a section to
# one file collide at end-of-file however well the numbers are allocated -- R2
# keeps 3.265 and 3.339 from colliding as NUMBERS and does nothing about the
# TEXT landing on the same line. Measured on cleat#1737, which reached
# UNMERGEABLE in the merge queue while its own PR page read CLEAN.
#
# EVERY GUARD THAT SCANS FOR SECTIONS MUST READ BOTH. A guard that reads only
# IMPROVEMENT-PLAN.md still passes -- it simply stops seeing the open sections,
# which is the half that matters. That is a silent narrowing, not a failure, so
# it is the reason this list is one command rather than a convention.
#
# Emits one path per line, so a caller can `while read -r`. Do not word-split
# the output: this repo's interactive shell is zsh, which does not split an
# unquoted expansion, and a script gets bash, which does.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

[ -f IMPROVEMENT-PLAN.md ] && echo "IMPROVEMENT-PLAN.md"

# Sorted so output is stable across machines: the shell glob's order is locale
# dependent, and a guard that diffs its own output would see phantom changes.
if [ -d IMPROVEMENT-PLAN.d ]; then
  find IMPROVEMENT-PLAN.d -maxdepth 1 -name '*.md' -type f | LC_ALL=C sort
fi
