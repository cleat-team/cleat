#!/usr/bin/env python3
"""Assemble IMPROVEMENT-PLAN.md + IMPROVEMENT-PLAN.d/ into one document.

THIS OUTPUT IS NOT COMMITTED, AND THAT IS THE DESIGN RATHER THAN AN OMISSION.

cleat#1727 split the open sections into one-file-per-section because two streams
appending to one file collide at end-of-file however well R2 allocates the
numbers -- measured on cleat#1737, which reached UNMERGEABLE in the merge queue
while its own PR page still read CLEAN.

A committed assembled copy would undo that, and silently. Measured the same
night on scripts/skip-baseline.txt: two PRs that both REGENERATED it did not
conflict at all. The merge was clean and the older scanner's output overwrote
the newer fix, with nothing red anywhere.

    two appends to a shared file        -> CONFLICT, loud, someone resolves it
    two regenerations of a DERIVED file -> CLEAN MERGE, and the stale side wins

So a derived file has no conflict surface. Committing one here would trade the
loud failure this split removes for a silent one, on a much larger blast radius
than a baseline -- and it is the same defect as `merge=union`, which cleat#1727
already rejected on measurement: both make the CONFLICT go away without making
the COLLISION go away.

Usage:
    scripts/assemble-improvement-plan.py            # to stdout
    scripts/assemble-improvement-plan.py -o out.md  # to a file you do not commit
"""

import argparse
import pathlib
import re
import sys

PLAN = pathlib.Path("IMPROVEMENT-PLAN.md")
PLAN_D = pathlib.Path("IMPROVEMENT-PLAN.d")
SECTION_RE = re.compile(r"^### (\d+)\.(\d+) ")


def sources():
    out = [PLAN] if PLAN.exists() else []
    if PLAN_D.is_dir():
        out += sorted(PLAN_D.glob("*.md"))
    return out


def sort_key(path):
    """Order directory files by section number, numerically.

    Lexical order puts 3.9 after 3.10 and 3.100 before 3.2, which would make the
    assembled document's order depend on nothing a reader can predict. The
    number comes from the file's own heading rather than its name: the name is a
    slug and can be renamed, the heading is what every reference resolves to.
    """
    for line in path.read_text(encoding="utf-8").split("\n"):
        m = SECTION_RE.match(line)
        if m:
            return (int(m.group(1)), int(m.group(2)))
    return (10**6, 10**6)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("-o", "--output", help="write here instead of stdout")
    args = ap.parse_args()

    if not PLAN.exists():
        print("ERROR: IMPROVEMENT-PLAN.md not found; run from the repo root.",
              file=sys.stderr)
        return 1

    parts = [PLAN.read_text(encoding="utf-8").rstrip("\n")]

    files = sorted(PLAN_D.glob("*.md"), key=sort_key) if PLAN_D.is_dir() else []
    if files:
        parts.append(
            "\n---\n\n## Open items\n\n"
            "Assembled from `IMPROVEMENT-PLAN.d/`, one file per section, by\n"
            "`scripts/assemble-improvement-plan.py`. This heading exists only in the\n"
            "assembled output; edit the files in that directory, never this text."
        )
        for f in files:
            parts.append(f.read_text(encoding="utf-8").rstrip("\n"))

    text = "\n\n".join(parts) + "\n"

    if args.output:
        pathlib.Path(args.output).write_text(text, encoding="utf-8")
        print(f"wrote {args.output}: {len(text.splitlines())} lines, "
              f"{len(files)} section file(s)", file=sys.stderr)
    else:
        sys.stdout.write(text)
    return 0


if __name__ == "__main__":
    sys.exit(main())
