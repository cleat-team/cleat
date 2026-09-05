#!/usr/bin/env python3
"""New IMPROVEMENT-PLAN sections per day, counted by FIRST APPEARANCE.

WORKSTREAM.md's "convergence metric" is the number this prints. It answers one
question -- is the project finding work faster than it is finishing it -- and it
is the only number that does, because the open-item count cannot: closing items
fast and finding them fast look identical in it.

WHY NOT THE OBVIOUS COMMAND. The metric was published for weeks as

    git log --since=... --until=... -p --format="" -- IMPROVEMENT-PLAN.md \\
      | grep -cE '^\\+### [0-9]+\\.[0-9]+ '

which counts `+###` diff lines. That counts a section again every time its
heading is rewritten -- and a heading is rewritten precisely when someone
corrects a status marker. So the published metric got WORSE the more carefully
the team maintained its markers, which is backwards. Measured on 2026-09-03: 48
lines, 27 distinct sections, 13 sections counted 2-5 times, one counted five.

This walks every commit that touched either plan file, oldest first, and records
the day each section number first appears in the tree. A heading rewritten later
is not counted again, and a section moved to IMPROVEMENT-PLAN-CLOSED.md by
scripts/archive-closed-sections.py is not counted again either -- the archive
move alone once inflated a day to 277.

Usage:
  scripts/convergence.py                 # one row per day
  scripts/convergence.py --days 7        # last 7 days only
  scripts/convergence.py --self-test     # negative control, see below
  scripts/convergence.py --markdown      # the table as WORKSTREAM.md carries it

PARTIAL DAYS UNDERCOUNT, and the error is large. 2026-09-04 read 11 at 21:30 and
closed at 17. Today's row is always partial; do not read a low final row as the
metric bending.
"""

from __future__ import annotations

import argparse
import collections
import re
import subprocess
import sys

PLAN_FILES = ["IMPROVEMENT-PLAN.md", "IMPROVEMENT-PLAN-CLOSED.md"]
SECTION = re.compile(r"^### (\d+\.\d+) ", re.M)


def first_appearance(commits: list[tuple[str, str, set[str]]]) -> dict[str, str]:
    """Map section -> the day it first appears, given oldest-first commits.

    Pure, so --self-test can drive it with synthetic input. `commits` is
    (sha, day, sections-present-in-the-tree-at-that-commit).
    """
    first: dict[str, str] = {}
    for _sha, day, present in commits:
        for section in present:
            if section not in first:
                first[section] = day
    return first


def _git_commits() -> list[tuple[str, str, set[str]]]:
    log = subprocess.run(
        ["git", "log", "--reverse", "--format=%H %cI", "--"] + PLAN_FILES,
        capture_output=True, text=True, check=True,
    ).stdout.splitlines()

    out: list[tuple[str, str, set[str]]] = []
    for line in log:
        if not line.strip():
            continue
        sha, iso = line.split()
        present: set[str] = set()
        for f in PLAN_FILES:
            blob = subprocess.run(["git", "show", f"{sha}:{f}"], capture_output=True, text=True)
            if blob.returncode == 0:
                present |= set(SECTION.findall(blob.stdout))
        # %cI carries the offset; the day is the LOCAL day, which is what a
        # human means by "sections filed on the 3rd". CLAUDE.md records a
        # four-hour error from pasting a local clock reading into a UTC
        # comparison -- the fix is to be explicit about which one you mean,
        # not to prefer one.
        out.append((sha, iso[:10], present))
    return out


def self_test() -> int:
    """Negative control: the diff-line method must disagree, and we must not use it.

    A guard with no negative control is a claim, not a check. The failure this
    exists to catch is silent: if first_appearance ever started counting a
    section once per commit that mentions it, every number would rise and
    nothing would error.
    """
    # Section 1.1 appears on day 1 and its heading is rewritten twice after.
    # 1.2 arrives on day 2. A diff-line count would report 1/2/1 = 4 events;
    # first appearance must report exactly 2, on the right days.
    synthetic = [
        ("sha1", "2026-01-01", {"1.1"}),
        ("sha2", "2026-01-01", {"1.1"}),          # heading rewritten, same day
        ("sha3", "2026-01-02", {"1.1", "1.2"}),   # 1.1 rewritten again, 1.2 new
        ("sha4", "2026-01-03", {"1.1", "1.2"}),   # both rewritten, nothing new
    ]
    first = first_appearance(synthetic)
    counts = collections.Counter(first.values())

    failures = []
    if len(first) != 2:
        failures.append(f"expected 2 distinct sections, got {len(first)}: {first}")
    if first.get("1.1") != "2026-01-01":
        failures.append(f"1.1 should first appear 2026-01-01, got {first.get('1.1')}")
    if first.get("1.2") != "2026-01-02":
        failures.append(f"1.2 should first appear 2026-01-02, got {first.get('1.2')}")
    if counts.get("2026-01-03"):
        failures.append("2026-01-03 introduced nothing and must report 0")
    if sum(counts.values()) != 2:
        failures.append(
            f"total must equal distinct sections (2), got {sum(counts.values())} -- "
            "this is the double-counting the diff-line method does"
        )

    if failures:
        for f in failures:
            print(f"FAIL: {f}", file=sys.stderr)
        return 1
    print("self-test passed: 4 commits mentioning 2 sections count as 2, on first appearance")
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(add_help=True)
    ap.add_argument("--self-test", action="store_true")
    ap.add_argument("--days", type=int, default=0, help="show only the last N days with data")
    ap.add_argument("--markdown", action="store_true", help="emit the WORKSTREAM.md table")
    args = ap.parse_args()

    if args.self_test:
        return self_test()

    commits = _git_commits()
    if not commits:
        print("no commits touched the plan files -- this is scanning nothing", file=sys.stderr)
        return 2

    first = first_appearance(commits)
    if not first:
        print("found no sections in any revision -- the pattern is broken, not the tree", file=sys.stderr)
        return 2

    counts = collections.Counter(first.values())
    days = sorted(counts)
    if args.days:
        days = days[-args.days :]

    if args.markdown:
        print("| " + " | ".join(d[5:] for d in days) + " |")
        print("|" + "---|" * len(days))
        print("| " + " | ".join(str(counts[d]) for d in days) + " |")
        return 0

    for d in days:
        print(f"{d}  {counts[d]}")
    print(f"total sections ever: {len(first)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
