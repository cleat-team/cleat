#!/usr/bin/env python3
"""Report IMPROVEMENT-PLAN.md headings that carry no status marker.

CLAUDE.md publishes this scan inline, as a heredoc, because that is the form
people actually copy. This file is the durable home of the same predicate, and
--audit re-derives every number CLAUDE.md states about it -- including the check
that the published heredoc and this file still agree, so the copy cannot drift
into being wrong while looking authoritative.

WHY THE PREDICATE HAS THE SHAPE IT HAS. It is two clauses joined by OR, and for
the whole history of the file they matched 0 headings in common: the word clause
allowed nothing between the em-dash and the marker word, so ANY symbol in that
position defeated it. The fallback was guaranteed to be absent in exactly the
case it existed for -- a heading whose symbol the emoji clause does not know.
`[^A-Za-z]*` is what makes the clauses independent; the Unicode category test is
what stops the symbol half needing a vocabulary that goes stale.

    scripts/check-plan-markers.py              # list headings with no status
    scripts/check-plan-markers.py --self-test # the known-positives, and CLAUDE.md's numbers
"""

import pathlib
import re
import sys
import unicodedata

PLAN = pathlib.Path("IMPROVEMENT-PLAN.md")
CLAUDE = pathlib.Path("CLAUDE.md")
HEADING_RE = re.compile(r"^### [0-9]+\.[0-9]+ ")

WORDS = (r"fixed|done|open|wontfix|declined|superseded|parked|deferred|"
         r"partly|partially|core fixed|shipped")
WORD_RE = re.compile(r"—[^A-Za-z]*(?:" + WORDS + r")", re.IGNORECASE)


# The status symbols actually in use. Deliberately a LIST, and the audit below
# is what keeps it honest: it reports any category-'So' character appearing in a
# heading that is not in here, so a genuinely new marker is added on purpose
# rather than absorbed silently.
KNOWN_MARKERS = frozenset("✅🟢🟡🔴🔶🔵🔷⬛⬜⚪❌")


def symbol(line):
    """True if the heading carries a status SYMBOL.

    WHY THIS IS NOT `unicodedata.category(c) == "So"`, which was the first
    attempt and is worse in the direction that matters. This scan PRINTS the
    unmarked headings, so the two failure modes are not symmetric:

        matcher too NARROW -> a real marker is missed -> a spurious LINE appears
        matcher too WIDE   -> prose is read as a marker -> a true line VANISHES

    Category 'So' is far wider than the markers: `✓` U+2713, `✔`, `✗`, `™`, `©`,
    `°`, `★` and `⚠` are all 'So'. A heading reading "a 30° window" or "✓ checked"
    would be counted as carrying a status, and the heading would silently drop
    out of the report -- in the one scan whose whole subject is checks that read
    cleanest where they measured least. A narrow matcher fails loud instead.

    A hand-picked list is only safe because the OTHER clause is now a real
    fallback: `— ⬛ **SUPERSEDED**` is carried by the word clause whatever this
    set contains. Before `[^A-Za-z]*` it was not, which is what made the old
    hand-picked set a single point of failure rather than half of a pair.
    """
    return any(c in KNOWN_MARKERS for c in line)


def marked(line):
    return symbol(line) or bool(WORD_RE.search(line))


def plan_sources():
    """Every file holding plan sections: the plan, then IMPROVEMENT-PLAN.d/.

    Since cleat#1727 the OPEN sections live one-per-file in IMPROVEMENT-PLAN.d/,
    and the open ones are exactly the ones whose marker this guard exists to
    check. Reading PLAN alone does not fail -- it reports "0 unmarked headings"
    over a tree it can only see 253 of 298 of, which is the reassuring answer
    from a check that stopped looking. Measured on the migration commit.
    """
    out = [PLAN] if PLAN.exists() else []
    d = pathlib.Path("IMPROVEMENT-PLAN.d")
    if d.is_dir():
        out += sorted(d.glob("*.md"))
    return out


def headings(path=None):
    paths = [path] if path is not None else plan_sources()
    hs = []
    for p in paths:
        hs += [l.rstrip() for l in p.open(encoding="utf-8") if HEADING_RE.match(l)]
    return hs


# --- the clauses as CLAUDE.md published them before 2026-09-16, kept so --audit
# --- can state what changed rather than asserting it.
OLD_SYMBOL_RE = re.compile(r"[\U0001F300-\U0001FAFF✅❌⬜⚪]")
OLD_WORD_RE = re.compile(r"—\s*(?:\*\*)?\s*(?:" + WORDS + r")", re.IGNORECASE)

# Each fixture declares WHICH CLAUSE IT DISCRIMINATES, and the audit checks that
# claim by reverting one clause at a time -- a label here is a testable assertion,
# not a comment. "SYM" and "WORD" mean the verdict changes when that clause alone
# is reverted; "none" means the OR rescues it in both directions.
#
# A control the OR rescues proves nothing about either half. That is how the
# first version of this audit stayed GREEN while the symbol clause was reverted,
# and WS-2's review found I had then overclaimed the repair: four of these six
# discriminate nothing. The two that do -- 9.1 for the symbol clause and 9.4 for
# the word clause -- are the entire guard, and `--self-test` fails if either
# clause is left without one, so deleting 9.1 as a "terser duplicate of 9.3"
# cannot silently disarm the symbol arm.
FIXTURES = [
    ("### 9.1 synthetic — ⬛", True, "SYM",
     "a marker the old set missed, with NO word to rescue it"),
    ("### 9.2 synthetic — **FIXED**", True, "none",
     "a word with no symbol (the old word clause matches this too)"),
    ("### 9.3 synthetic — ⬛ **SUPERSEDED 2026-09-02**", True, "none",
     "documents the original bug; rescued either way, so it guards nothing"),
    ("### 9.4 synthetic — → **DEFERRED 2026-09-16**", True, "WORD",
     "a word past a non-marker symbol -- the anti-correlation itself"),
    ("### 9.5 synthetic — 231 → 184, and three defects behind the skips", False, "none",
     "`→` in prose is not a marker"),
    ("### 9.6 synthetic — a 30° window, ✓ checked, 89 findings", False, "none",
     "prose symbols (`°`, `✓`) are category So and must NOT count"),
]

# scripts/archive-closed-sections.py's own, third vocabulary.
ARCHIVER_CLOSED_RE = re.compile(
    r"✅|🟢|\bFIXED\b|\bDONE\b|\bCLOSED\b|\bGUARDED\b|fixed in", re.IGNORECASE)
CLOSE_WORDS_RE = re.compile(
    r"superseded|declined|wontfix|won't fix|obsolete|retired|removed|parked|deferred", re.IGNORECASE)


def published_predicate():
    """Extract CLAUDE.md's inline heredoc and return its `marked` function.

    The point of the guard is that it DEPENDS on the published text: if the
    heredoc is edited away or reworded into something that no longer defines
    `marked`, this raises instead of quietly passing.
    """
    text = CLAUDE.read_text(encoding="utf-8")
    blocks = re.findall(r"python3 - <<'EOF'\n(.*?\n)\s*EOF\n", text, re.DOTALL)
    # Select by CONTENT, not by position: CLAUDE.md carries more than one
    # heredoc and the first one is a different scan entirely. Requiring exactly
    # one match means a second copy of this scan fails the audit rather than
    # being silently ignored -- which is the defect this whole guard is about.
    mine = [b for b in blocks
            if "IMPROVEMENT-PLAN.md" in b and "def marked(" in b]
    if len(mine) != 1:
        raise SystemExit(
            f"AUDIT FAILED: expected exactly 1 published marker scan in CLAUDE.md, "
            f"found {len(mine)} (of {len(blocks)} heredocs). If the scan was "
            f"reworded, update this guard with it -- do not delete the guard.")
    lines = mine[0].splitlines(keepends=True)
    pad = min((len(l) - len(l.lstrip()) for l in lines if l.strip()), default=0)
    body = "".join(l[pad:] if l.strip() else l for l in lines)
    # Run it without the file-reading and printing halves.
    body = re.sub(r"^\s*hs = \[.*?\]\n", "", body, flags=re.DOTALL | re.MULTILINE)
    body = re.sub(r"^\s*for l in hs:\n(?:\s+.*\n)*", "", body, flags=re.MULTILINE)
    ns = {}
    # exec is the point: running the copy CLAUDE.md publishes is the only way to
    # show the copy still behaves. Re-implementing it here would compare this
    # file against itself. The input is a tracked file in this repo, changed only
    # through review, and read from a fixed path.
    exec(compile(body, "<CLAUDE.md scan>", "exec"), ns)  # noqa: S102
    return ns["marked"]


def audit():
    hs = headings()
    ok = True

    def check(label, got, want):
        nonlocal ok
        good = got == want
        ok = ok and good
        print(f"  {'OK  ' if good else 'FAIL'} {label:52} {got}")

    print(f"headings across {len(plan_sources())} plan source(s): {len(hs)}")

    print("\nthe clauses CLAUDE.md published before 2026-09-16:")
    both = sum(1 for l in hs if OLD_SYMBOL_RE.search(l) and OLD_WORD_RE.search(l))
    sym_only = sum(1 for l in hs if OLD_SYMBOL_RE.search(l) and not OLD_WORD_RE.search(l))
    word_only = sum(1 for l in hs if OLD_WORD_RE.search(l) and not OLD_SYMBOL_RE.search(l))
    neither = sum(1 for l in hs if not OLD_SYMBOL_RE.search(l) and not OLD_WORD_RE.search(l))
    print(f"    emoji AND word {both}    emoji only {sym_only}    "
          f"word only {word_only}    neither {neither}")
    check("the OR was never a redundancy (overlap == 0)", both, 0)

    print("\nthe clauses as they are now:")
    both_new = sum(1 for l in hs if symbol(l) and WORD_RE.search(l))
    print(f"    overlap {both_new}")
    check("the clauses now overlap (a real fallback)", both_new > 0, True)

    print("\nreplacing the clauses changes no verdict on a real heading:")
    old = lambda l: bool(OLD_SYMBOL_RE.search(l) or OLD_WORD_RE.search(l))
    disagree = [l for l in hs if old(l) != marked(l)]
    for l in disagree:
        print(f"      {l[:100]}")
    check("headings where old and new disagree", len(disagree), 0)

    print("\ncontrols, with each fixture's discrimination claim checked:")
    # Rebuild `marked` with exactly one clause reverted, to test the labels.
    sym_reverted = lambda l: bool(OLD_SYMBOL_RE.search(l)) or bool(WORD_RE.search(l))
    word_reverted = lambda l: symbol(l) or bool(OLD_WORD_RE.search(l))
    covered = set()
    for line, want, arm, why in FIXTURES:
        check(why, marked(line), want)
        actual = []
        if sym_reverted(line) != want:
            actual.append("SYM")
        if word_reverted(line) != want:
            actual.append("WORD")
        claim = [] if arm == "none" else [arm]
        check(f"    discriminates {arm!r}", sorted(actual), sorted(claim))
        covered.update(actual)
    check("a fixture guards the SYMBOL clause", "SYM" in covered, True)
    check("a fixture guards the WORD clause", "WORD" in covered, True)
    check("the headline case was missed by BOTH old clauses", old(FIXTURES[2][0]), False)

    print("\nevery status symbol in the plan is a KNOWN marker:")
    # The matcher is a list on purpose (see symbol()); THIS is what keeps the
    # list honest. Category 'So' is the wide net, used to DETECT rather than to
    # match, so a genuinely new marker is reported and added deliberately, and a
    # `✓` written in prose is caught the first time instead of silently deleting
    # a line from the report.
    found = {c for l in hs for c in l if unicodedata.category(c) == "So"}
    unknown = sorted(found - KNOWN_MARKERS)
    for c in unknown:
        print(f"      {c} U+{ord(c):04X} {unicodedata.name(c, '?')}")
    check("category-So characters not in KNOWN_MARKERS", len(unknown), 0)

    print("\nsymbols appearing in headings, and whether each clause knows them:")
    seen = {}
    for l in hs:
        for c in l:
            if unicodedata.category(c) in ("So", "Sm") and not c.isascii():
                seen[c] = seen.get(c, 0) + 1
    for c, n in sorted(seen.items(), key=lambda kv: -kv[1]):
        print(f"    {c} U+{ord(c):04X} {unicodedata.category(c)} n={n:<4} "
              f"old={'yes' if OLD_SYMBOL_RE.match(c) else 'no ':<3} "
              f"now={'yes' if symbol(c) else 'no'}")

    print("\nagainst scripts/archive-closed-sections.py's third vocabulary:")
    missed = []
    for l in hs:
        tail = l.split(" — ", 1)[1] if " — " in l else ""
        if CLOSE_WORDS_RE.search(tail) and not ARCHIVER_CLOSED_RE.search(tail):
            missed.append(l)
    for l in missed:
        print(f"      {l[:100]}")
    check("headings closed by a word the archiver cannot see", len(missed), 0)

    print("\nCLAUDE.md's published heredoc still agrees with this file:")
    pub = published_predicate()
    # bool() on BOTH sides. The published copy ends in `or word.search(l)`, so it
    # returns a match object or None where this file returns True or False, and
    # `None != False` is True -- comparing them raw reports 13 drifting headings
    # on a pair of predicates that agree about every one of them.
    # Over the real headings AND the fixtures. The headings alone cannot separate
    # the two word clauses -- every heading a reverted word clause would miss is
    # also carried by the symbol clause, so drift would read 0 on a doc copy that
    # had been reverted. The fixtures are the cases that discriminate.
    population = hs + [f[0] for f in FIXTURES]
    drift = [l for l in population if bool(pub(l)) != bool(marked(l))]
    for l in drift:
        print(f"      {l[:100]}")
    check("headings where the doc copy and this file disagree", len(drift), 0)

    print("\nAUDIT OK" if ok else "\nAUDIT FAILED")
    return 0 if ok else 1


def main():
    if {"--self-test", "--audit"} & set(sys.argv[1:]):
        return audit()
    for l in headings():
        if not marked(l):
            print(l[:110])
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
