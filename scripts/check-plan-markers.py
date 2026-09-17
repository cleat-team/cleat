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


def symbol(line):
    """True if the heading carries a status SYMBOL.

    Category 'So' rather than a codepoint range. A range picked by hand misses
    the next marker exactly as a list does -- the previous version of this scan
    carried `⬜` U+2B1C and missed `⬛` U+2B1B, its neighbour. Widening by block
    is not the fix either: it sweeps in `→` U+2192, which appears in headings as
    prose. `→` is category 'Sm', every marker in use is 'So'.
    """
    return any(unicodedata.category(c) == "So" for c in line)


def marked(line):
    return symbol(line) or bool(WORD_RE.search(line))


def headings(path=PLAN):
    return [l.rstrip() for l in path.open(encoding="utf-8") if HEADING_RE.match(l)]


# --- the clauses as CLAUDE.md published them before 2026-09-16, kept so --audit
# --- can state what changed rather than asserting it.
OLD_SYMBOL_RE = re.compile(r"[\U0001F300-\U0001FAFF✅❌⬜⚪]")
OLD_WORD_RE = re.compile(r"—\s*(?:\*\*)?\s*(?:" + WORDS + r")", re.IGNORECASE)

# Cases chosen so that each BREAKS IF ONE CLAUSE IS REVERTED, rather than being
# caught by the other. A control that the OR rescues proves nothing about either
# half -- which is how the first version of this audit stayed green while the
# symbol clause was reverted to the codepoint range it replaced.
FIXTURES = [
    ("### 9.1 synthetic — ⬛", True,
     "symbol clause alone: a marker with no word after it"),
    ("### 9.2 synthetic — **FIXED**", True,
     "word clause alone: a word with no symbol at all"),
    ("### 9.3 synthetic — ⬛ **SUPERSEDED 2026-09-02**", True,
     "the headline: a symbol the old class missed, ahead of a word"),
    ("### 9.4 synthetic — → **DEFERRED 2026-09-16**", True,
     "word clause past a non-marker symbol (the anti-correlation)"),
    ("### 9.5 synthetic — 231 → 184, and three defects behind the skips", False,
     "`→` in prose is not a marker"),
    ("### 9.6 synthetic — 89 findings, and one that changes a support claim", False,
     "no marker at all is still no marker"),
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

    print(f"headings in {PLAN}: {len(hs)}")

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

    print("\ncontrols (each isolates ONE clause -- see FIXTURES):")
    for line, want, why in FIXTURES:
        check(f"{why}", marked(line), want)
    headline = FIXTURES[2][0]
    check("...and the headline case was missed by BOTH old clauses", old(headline), False)

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
