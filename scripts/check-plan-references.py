#!/usr/bin/env python3
"""Every `§N.M` in the repo still resolves to exactly one place.

cleat#1727 split the open sections out of IMPROVEMENT-PLAN.md into
IMPROVEMENT-PLAN.d/. R3 exists because breaking cross-references is the failure
that made "delete the closed sections" unimplementable, so the split is only
safe if every reference still lands somewhere. This is that check.

TWO NAMESPACES, AND THE SECOND ONE IS WHY THIS SCRIPT IS NOT A ONE-LINER.

A `§N.M` does not always name a `### N.M` heading. The Phase 2 table carries
rows `2.1`-`2.9` in the same numeric shape, and three sections cited FROM CODE --
`§2.4`, `§2.5`, `§2.7` -- resolve to *rows*, not headings. Measured:

    rows that are not headings : 0.1-0.6, 2.1-2.7, 2.9   (14)
    ...of those, cited in code : 2.4, 2.5, 2.7

A checker that models only headings reports those three as dangling. That check
has been written once already, it did exactly that, and the finding was wrong.

THE KNOWN-POSITIVE IS §2.8, WHERE THE TWO NAMESPACES DELIBERATELY JOIN. Row 2.8
says "see below" and `### 2.8 results` is the write-up, so 2.8 is the one number
present in both. --self-test asserts that a deliberately single-namespace reading
FAILS on the row-only references and that the real reading does not: a self-test
that only checks the happy path is satisfied by every broken version of this
script.

Usage:
    scripts/check-plan-references.py
    scripts/check-plan-references.py --self-test
"""

import pathlib
import re
import subprocess
import sys

PLAN = pathlib.Path("IMPROVEMENT-PLAN.md")
PLAN_D = pathlib.Path("IMPROVEMENT-PLAN.d")
ARCHIVE = pathlib.Path("IMPROVEMENT-PLAN-CLOSED.md")

HEADING_RE = re.compile(r"^### (\d+\.\d+) ", re.M)
ROW_RE = re.compile(r"^\|\s*(\d+\.\d+)\s*\|", re.M)
REF_RE = re.compile(r"§(\d+\.\d+)")

CODE_GLOBS = ["*.go", "*.sh", "*.yml", "*.yaml", "*.rs", "*.py", "*.java", "*.ts"]

# A §N.M immediately preceded by ANOTHER document's name belongs to that
# document, not to the plan. Anchored to the end of the preceding text so it
# only fires when the filename actually qualifies this reference -- a filename
# earlier in a long line must not silence a real plan citation after it.
QUALIFIED_RE = re.compile(
    r"(?:^|[\s(\[`*])(?!IMPROVEMENT-PLAN)[A-Za-z0-9_-]+\.md[`*\]) ]*\s*$")


def plan_text():
    """Everything that can DEFINE a section, concatenated."""
    parts = []
    for p in [PLAN, ARCHIVE]:
        if p.exists():
            parts.append(p.read_text(encoding="utf-8"))
    if PLAN_D.is_dir():
        for f in sorted(PLAN_D.glob("*.md")):
            parts.append(f.read_text(encoding="utf-8"))
    return "\n".join(parts)


def defined(text, headings_only=False):
    """The set of section numbers a reference can resolve to.

    headings_only exists for the self-test: it is the deliberately-wrong reading
    whose job is to DISAGREE, so the difference can be examined. A scan that
    cannot be made to fail proves nothing about the scan that ships.
    """
    d = set(HEADING_RE.findall(text))
    if not headings_only:
        d |= set(ROW_RE.findall(text))
    return d


# A document that numbers its own sections owns its own §N.M namespace.
SELF_HEADING_RE = re.compile(r"^#{1,4}\s+(\d+\.\d+)[ .]", re.M)


def self_defined(path):
    """Section numbers a file defines FOR ITSELF.

    `docs/contributor/design/event-routing-design.md` cites §4.3 and §6.2, and
    those are ITS OWN headings -- not IMPROVEMENT-PLAN sections. Resolving every
    §N.M against the plan reports six such references as dangling and sends the
    reader to fix links that were never broken. This is the same trap as a text
    search that cannot tell a thing from a sentence about the thing: the sigil
    is identical, the namespace is not.
    """
    try:
        return set(SELF_HEADING_RE.findall(
            pathlib.Path(path).read_text(encoding="utf-8", errors="replace")))
    except OSError:
        return set()


def references():
    """Every §N.M in the repo that plausibly names a PLAN section.

    A reference is skipped when the citing file defines that number as one of
    its own headings; see self_defined().
    """
    refs = {}
    out = subprocess.run(
        ["git", "grep", "-nE", r"§[0-9]+\.[0-9]+", "--"] + CODE_GLOBS + ["*.md"],
        capture_output=True, text=True).stdout
    own = {}
    for line in out.split("\n"):
        if not line:
            continue
        loc, _, body = line.partition(":")
        if loc not in own:
            own[loc] = self_defined(loc)
        for m in REF_RE.finditer(body):
            n = m.group(1)
            if n in own[loc]:
                continue
            if QUALIFIED_RE.search(body[:m.start()]):
                # "`ABI.md` §2.53", "database-backends.md §7.4" -- the reference
                # names ANOTHER document's numbering explicitly. Resolving those
                # against the plan reported four dangling links that are not
                # links to the plan at all.
                continue
            refs.setdefault(n, set()).add(loc)
    return refs


def run(headings_only=False):
    text = plan_text()
    have = defined(text, headings_only)
    refs = references()
    dangling = {n: locs for n, locs in refs.items() if n not in have}
    return have, refs, dangling


def self_test():
    """Known-positive: the single-namespace reading must FAIL on row-only refs."""
    ok = True
    _, _, loose_dangling = run(headings_only=False)
    _, _, tight_dangling = run(headings_only=True)

    # 1. The shipping reading resolves the three row-only references.
    for n in ("2.4", "2.5", "2.7"):
        if n in loose_dangling:
            print(f"SELF-TEST FAIL: §{n} reported dangling by the real reading; "
                  f"it resolves to a Phase 2 table ROW.", file=sys.stderr)
            ok = False

    # 2. The deliberately-wrong reading MUST report them. If it does not, this
    #    self-test is asserting nothing -- the two readings have collapsed into
    #    one and the row namespace is no longer being exercised at all.
    missed = [n for n in ("2.4", "2.5", "2.7") if n not in tight_dangling]
    if missed:
        print(f"SELF-TEST FAIL: the headings-only reading did NOT flag {missed}. "
              f"This test can no longer tell the two namespaces apart, so a "
              f"regression to headings-only would pass it.", file=sys.stderr)
        ok = False

    # 3. §2.8 is the join: present in BOTH namespaces, so it must resolve under
    #    either reading. A checker that handles one namespace passes every
    #    reference except the ones that matter, and 2.8 is where that shows.
    text = plan_text()
    if "2.8" not in defined(text, headings_only=True):
        print("SELF-TEST FAIL: §2.8 is not a heading; it is the documented join "
              "between the two namespaces and the anchor this test rests on.",
              file=sys.stderr)
        ok = False
    if "2.8" not in set(ROW_RE.findall(text)):
        print("SELF-TEST FAIL: §2.8 is not a table row; the join it anchors is "
              "gone, so this self-test is no longer testing what it claims.",
              file=sys.stderr)
        ok = False

    # 4. Vacuity: a git-grep that matched nothing makes every reference resolve.
    _, refs, _ = run()
    if len(refs) < 100:
        print(f"SELF-TEST FAIL: found only {len(refs)} distinct §N.M references; "
              f"the scan is reading almost nothing and everything would look "
              f"resolvable.", file=sys.stderr)
        ok = False

    print("self-test: PASS" if ok else "self-test: FAIL", file=sys.stderr)
    return 0 if ok else 1


def main():
    if "--self-test" in sys.argv:
        return self_test()

    have, refs, dangling = run()
    if not have:
        print("ERROR: no section numbers found at all. The plan is unreadable "
              "from here, and every reference would look dangling.",
              file=sys.stderr)
        return 2
    if dangling:
        print("ERROR: §N.M references that resolve to nothing:", file=sys.stderr)
        for n in sorted(dangling, key=lambda x: [int(p) for p in x.split(".")]):
            locs = sorted(dangling[n])
            print(f"  §{n}  cited from {len(locs)} place(s): "
                  f"{', '.join(locs[:3])}{' ...' if len(locs) > 3 else ''}",
                  file=sys.stderr)
        print("\nA reference that resolves to nothing is a pointer into prose "
              "that reads as correct.", file=sys.stderr)
        return 1

    print(f"OK: all {len(refs)} distinct §N.M reference(s) resolve, against "
          f"{len(have)} defined section number(s) across both namespaces.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
