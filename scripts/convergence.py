#!/usr/bin/env python3
"""Findings recorded per day: IMPROVEMENT-PLAN sections plus GitHub issues.

WORKSTREAM.md's "convergence metric" is the number this prints. It answers one
question -- is the project finding work faster than it is finishing it -- and it
is the only number that does, because the open-item count cannot: closing items
fast and finding them fast look identical in it.

IT COUNTED ONLY PLAN SECTIONS UNTIL 2026-09-12, AND BY THEN THAT WAS MOST OF THE
ANSWER MISSING. The tracker went into use on 2026-09-06 -- 228 of the repo's 229
issues were filed on or after that date, the 229th in June -- and findings moved
there. The section rate duly fell, from ~23/day over 09-01..09-05 to 3 and 4 on
09-08 and 09-09, which is exactly the shape WORKSTREAM.md tells a reader to
interpret as the work finishing. It was not:

    day     sections  issues        day     sections  issues
    09-05     32         7          09-09      4        33
    09-06     17        12          09-10     14        37
    09-07     19        22          09-11      9        54
    09-08      3        37          09-12      9        27

The combined rate went UP while the published one halved. Note the direction --
a scan that cannot see where the answer moved reports the flattering number, and
nobody re-derives a figure that looks good.

The two populations are near-disjoint, so summing them is not double counting:
of 229 issues, 18 are cited anywhere in either plan file, and a citation is
weaker than a section. Re-derive:

    gh issue list --state all --limit 1000 --json number --jq '.[].number' |
      sort -n > /tmp/i.txt
    grep -ohE '#[0-9]{3,4}' IMPROVEMENT-PLAN.md IMPROVEMENT-PLAN-CLOSED.md |
      tr -d '#' | sort -un > /tmp/c.txt
    comm -12 /tmp/i.txt /tmp/c.txt | wc -l

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
  scripts/convergence.py --no-issues     # plan sections only; needs no network

WHEN `gh` IS UNAVAILABLE THE ISSUE COLUMN READS `UNMEASURED`, NEVER 0, and the
total reads UNMEASURED with it. A zero there is indistinguishable from a quiet
day, and it is the same flattering direction as the bug above: an unauthenticated
or offline run would otherwise reprint the very number this change exists to
retire. --no-issues is the way to ask for the plan-only figure on purpose.

PARTIAL DAYS UNDERCOUNT, and the error is large. 2026-09-04 read 11 at 21:30 and
closed at 17. Today's row is always partial; do not read a low final row as the
metric bending.
"""

from __future__ import annotations

import argparse
import collections
import datetime
import json
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


def local_days(created: list[str], tz: datetime.tzinfo | None = None) -> dict[str, int]:
    """Bucket ISO-8601 UTC timestamps by LOCAL day.

    Pure, so --self-test can drive it. The plan-section side buckets on %cI,
    which is a local day; bucketing the issue side on the UTC prefix would put
    the two columns in different calendars and disagree by the offset at every
    midnight. Measured 2026-09-12 on this repo: issues #1404 and #1410 carry
    2026-09-13T02:24Z and 03:46Z and were filed at 22:24 and 23:46 local on the
    12th, so a UTC bucket opens a day that has not started yet and files two
    findings into it.
    """
    out: dict[str, int] = collections.Counter()
    for iso in created:
        ts = datetime.datetime.fromisoformat(iso.replace("Z", "+00:00"))
        out[ts.astimezone(tz).strftime("%Y-%m-%d")] += 1
    return dict(out)


def render_cells(sections: int, issues: int | None) -> tuple[str, str]:
    """The (issue, total) cells for one day. `issues is None` means UNASKED.

    Hoisted out of main so --self-test can pin it. The failure it guards is a
    one-character edit -- `issues or 0` in place of the None check -- which
    would print a plain number and reprint the retired plan-only figure as if
    it were the whole answer.
    """
    if issues is None:
        return "?", "UNMEASURED"
    return str(issues), str(sections + issues)


def _issue_days() -> dict[str, int] | None:
    """Issues filed per local day, or None when the question could not be asked.

    None is not zero. A `gh` that is missing, unauthenticated or rate-limited
    must not be reported as a day on which nobody found anything.
    """
    try:
        proc = subprocess.run(
            ["gh", "issue", "list", "--state", "all", "--limit", "1000",
             "--json", "createdAt"],
            capture_output=True, text=True,
        )
    except (OSError, FileNotFoundError):
        return None
    if proc.returncode != 0 or not proc.stdout.strip():
        return None
    try:
        rows = json.loads(proc.stdout)
    except json.JSONDecodeError:
        return None
    if not isinstance(rows, list) or not rows:
        # An empty list is indistinguishable here from a filter that matched
        # nothing, and this repo has 229 issues. Refuse to publish it as zero.
        return None
    return local_days([r["createdAt"] for r in rows])


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

    # --- the issue column: bucketed LOCAL, and UNASKED is not zero -------
    #
    # A fixed zone, not the machine's: on a UTC runner an "is it converted?"
    # assertion is satisfied by doing nothing, which is the check that could
    # not have disagreed.
    minus4 = datetime.timezone(datetime.timedelta(hours=-4))
    boundary = ["2026-09-13T02:24:18Z", "2026-09-13T03:46:39Z", "2026-09-12T16:00:00Z"]
    got = local_days(boundary, minus4)
    naive = collections.Counter(s[:10] for s in boundary)

    if got != {"2026-09-12": 3}:
        failures.append(f"at -04:00 all three timestamps are 2026-09-12 locally, got {got}")
    if got == dict(naive):
        failures.append(
            "local_days agrees with the raw UTC prefix, so the conversion is inert -- "
            f"naive={dict(naive)}"
        )

    if render_cells(9, 45) != ("45", "54"):
        failures.append(f"render_cells(9, 45) should be ('45','54'), got {render_cells(9, 45)}")
    if render_cells(9, None) != ("?", "UNMEASURED"):
        failures.append(
            "an unasked issue count must render UNMEASURED, never a number: "
            f"got {render_cells(9, None)}"
        )
    if render_cells(9, None)[1] == render_cells(9, 0)[1]:
        failures.append("UNMEASURED and a measured zero render identically")

    if failures:
        for f in failures:
            print(f"FAIL: {f}", file=sys.stderr)
        return 1
    print("self-test passed: 4 commits mentioning 2 sections count as 2, on first appearance;")
    print("                  three timestamps that are 09-13 in UTC bucket as 09-12 at -04:00;")
    print("                  an unasked issue count renders UNMEASURED, a measured zero does not")
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(add_help=True)
    ap.add_argument("--self-test", action="store_true")
    ap.add_argument("--days", type=int, default=0, help="show only the last N days with data")
    ap.add_argument("--markdown", action="store_true", help="emit the WORKSTREAM.md table")
    ap.add_argument("--no-issues", action="store_true",
                    help="plan sections only -- the pre-2026-09-12 figure, on purpose")
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

    issues = None if args.no_issues else _issue_days()
    if issues is None and not args.no_issues:
        print("gh could not be asked for issues -- the issue and total columns "
              "read UNMEASURED, which is not the same as 0", file=sys.stderr)

    days = sorted(set(counts) | set(issues or {}))
    if args.days:
        days = days[-args.days :]

    def issue_cell(d: str) -> str:
        return render_cells(counts[d], None if issues is None else issues.get(d, 0))[0]

    def total_cell(d: str) -> str:
        return render_cells(counts[d], None if issues is None else issues.get(d, 0))[1]

    if args.markdown:
        print("| | " + " | ".join(d[5:] for d in days) + " |")
        print("|---|" + "---|" * len(days))
        print("| sections | " + " | ".join(str(counts[d]) for d in days) + " |")
        if args.no_issues:
            return 0
        print("| issues | " + " | ".join(issue_cell(d) for d in days) + " |")
        print("| **total** | " + " | ".join(total_cell(d) for d in days) + " |")
        return 0

    if args.no_issues:
        for d in days:
            print(f"{d}  {counts[d]}")
        print(f"total sections ever: {len(first)}")
        return 0

    print("day         sections  issues  total")
    for d in days:
        print(f"{d}  {counts[d]:>8}  {issue_cell(d):>6}  {total_cell(d):>5}")
    print(f"total sections ever: {len(first)}")
    if issues is not None:
        print(f"total issues ever:   {sum(issues.values())}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
