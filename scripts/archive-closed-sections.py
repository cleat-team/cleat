#!/usr/bin/env python3
"""Move closed IMPROVEMENT-PLAN sections into IMPROVEMENT-PLAN-CLOSED.md.

R3: a closed item leaves the plan by being ARCHIVED, not deleted. The plan was
15,885 lines with 145 of 156 sections closed, and both stale-marker incidents in
the week before this ran were findability failures caused by size.

WHY A STUB IS LEFT BEHIND rather than the section simply disappearing. Three
things depend on the heading still being in the plan:

  * 819 in-plan and 2,354 repo-wide citations name sections by number, and a
    reader who greps IMPROVEMENT-PLAN.md for a number must still find it;
  * scripts/check-section-numbers.sh asserts uniqueness and per-stream block
    membership over `^### N.M` headings, and would stop protecting an archived
    number the moment it left -- so the number could be re-used;
  * that same script has a vacuity floor of 50 headings. Removing 145 of 156
    would trip it, and the guard would fail for the right reason in a way that
    looks like the wrong one.

So each archived section is replaced by its own heading plus one pointer line.
The plan keeps every number and every status marker; it loses the bodies.

WHY THE CLASSIFIER IS CONSERVATIVE. A heading may carry a close marker and still
describe open work -- "FIXED (wiring still OPEN)", "FIXED for Go WASM, residual
gap below", "fixed; one part still open", "PARTLY FIXED". Archiving those would
hide open items, which is the opposite of the point. Anything whose heading
carries a qualifier stays in the plan whole, and the script prints what it held
back so the decision is auditable rather than silent.

Usage:
    scripts/archive-closed-sections.py --dry-run   # classify and report
    scripts/archive-closed-sections.py             # rewrite both files
"""

import argparse
import pathlib
import re
import sys

PLAN = pathlib.Path("IMPROVEMENT-PLAN.md")
ARCHIVE = pathlib.Path("IMPROVEMENT-PLAN-CLOSED.md")

SECTION_RE = re.compile(r"^### (\d+\.\d+) (.*)$")
STUB_RE = re.compile(r"^Archived — full text in ")
PARENT_RE = re.compile(r"^## (.*)$")

# A heading is a candidate for archiving only if it says the work is finished.
CLOSED_RE = re.compile(r"✅|🟢|\bFIXED\b|\bDONE\b|\bCLOSED\b|\bGUARDED\b|fixed in", re.IGNORECASE)

# ...and is held back if it also says some of it is not. These were read off the
# 14 headings that matched CLOSED_RE while describing open work; the script
# prints every held-back heading so the list can be checked rather than trusted.
QUALIFIED_RE = re.compile(
    r"partly|partial|residual|still|remains|one part|\bopen\b|🔶|🔴|🔷|"
    r"for Go WASM|for the normal path|for deployments",
    re.IGNORECASE,
)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--dry-run", action="store_true")
    args = ap.parse_args()

    if not PLAN.exists():
        sys.exit(f"{PLAN} not found")

    text = PLAN.read_text(encoding="utf-8")
    lines = text.split("\n")

    # Classify first, and report, before touching anything.
    archive, keep = [], []
    for line in lines:
        m = SECTION_RE.match(line)
        if not m:
            continue
        heading = line
        # Test the MARKER, not the whole heading. The qualifier vocabulary
        # overlaps with words this project puts in titles: "A defer segment
        # could STILL take a distributed lock -- FIXED" is a clean close whose
        # title contains "still". Scoping the veto to the text after the last
        # em-dash separator held back four such sections on the first run.
        #
        # Split on the FIRST separator, not the last: this project's convention
        # is `### N.M Title — STATUS`, and a status can itself contain an
        # em-dash. §3.88's does ("steps 1 and 2 DONE; step 3 DONE 2026-09-04 —
        # §3.112 and §3.114 built two of the three transitions..."), so
        # last-split saw only the tail, found no close marker, and held back a
        # section that is done.
        marker = heading.split(" — ", 1)[1] if " — " in heading else ""
        if CLOSED_RE.search(marker) and not QUALIFIED_RE.search(marker):
            archive.append((m.group(1), heading))
        else:
            keep.append((m.group(1), heading))

    print(f"sections total     : {len(archive) + len(keep)}")
    print(f"  archive (closed) : {len(archive)}")
    print(f"  keep in the plan : {len(keep)}")
    print()
    print("HELD BACK -- carries a close marker but also a qualifier, or is open:")
    for num, heading in keep:
        print(f"  {num:<7} {heading[4:][:104]}")

    if args.dry_run:
        return

    # INCREMENTAL, AND THAT IS NOT A REFINEMENT. The first version regenerated
    # the archive from the plan every run. On the second run the plan's archived
    # sections are stubs, so it faithfully archived the stubs and overwrote the
    # archive with them: 13,696 lines to 711, every closed section's body gone.
    # It was caught by checking idempotence -- `cmp` after a second run -- which
    # is the only reason this is not a data-loss bug in a docs tool.
    #
    # So the archive is APPEND-ONLY, keyed by section number. A section already
    # in the archive is left alone in both files.
    already = set()
    if ARCHIVE.exists():
        for line in ARCHIVE.read_text(encoding="utf-8").split("\n"):
            m = SECTION_RE.match(line)
            if m:
                already.add(m.group(1))

    archived_nums = {n for n, _ in archive} - already
    if already:
        print(f"already archived   : {len(already)} (left untouched)")
        print(f"newly archiving    : {len(archived_nums)}")
    out, arch_out = [], []
    cur_num, cur_parent = None, None
    arch_parent_written = None

    for line in lines:
        pm = PARENT_RE.match(line)
        if pm:
            cur_parent, cur_num = pm.group(1), None
            out.append(line)
            continue
        sm = SECTION_RE.match(line)
        if sm:
            cur_num = sm.group(1)
            if cur_num in archived_nums:
                out.append(line)
                out.append("")
                out.append(f"Archived — full text in [`{ARCHIVE}`]({ARCHIVE}).")
                out.append("")
                if arch_parent_written != cur_parent:
                    arch_out.append("")
                    arch_out.append(f"## {cur_parent}")
                    arch_parent_written = cur_parent
                arch_out.append("")
                arch_out.append(line)
            else:
                out.append(line)
            continue
        if cur_num in archived_nums:
            if STUB_RE.match(line):
                # Defence in depth behind `already`: never copy a stub into the
                # archive, whatever the bookkeeping says.
                out.append(line)
            else:
                arch_out.append(line)
        else:
            out.append(line)

    header = [
        "# Improvement Plan — closed items",
        "",
        "The archive half of `IMPROVEMENT-PLAN.md`. Every section here is CLOSED; the plan keeps a",
        "one-line stub for each so that its number still resolves there, still cannot be re-used, and",
        "still carries its status marker.",
        "",
        "Nothing is deleted, because the numbers are load-bearing: 2,354 citations across the repo name",
        "a plan section as the authoritative description of what a package or test is for.",
        "",
        "Regenerate with `scripts/archive-closed-sections.py`. Sections keep the phase heading they were",
        "filed under.",
        "",
        "---",
    ]

    if not archived_nums:
        print("\nnothing new to archive; both files left unchanged.")
        return

    PLAN.write_text("\n".join(out), encoding="utf-8")
    if ARCHIVE.exists():
        existing = ARCHIVE.read_text(encoding="utf-8").rstrip("\n")
        ARCHIVE.write_text(existing + "\n" + "\n".join(arch_out) + "\n", encoding="utf-8")
    else:
        ARCHIVE.write_text("\n".join(header + arch_out) + "\n", encoding="utf-8")
    print()
    print(f"wrote {PLAN} ({len(out)} lines) and {ARCHIVE} "
          f"({len(ARCHIVE.read_text(encoding='utf-8').split(chr(10)))} lines)")


if __name__ == "__main__":
    main()
