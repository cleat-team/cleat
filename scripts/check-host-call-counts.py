#!/usr/bin/env python3
"""No document may restate the engine's host-call count, unless it is right.

cleat#1414. The count has moved five times in eight days -- 58 -> 52 (#767) ->
50 (#843) -> 52 (#868) -> 54 -- and on 2026-09-13 eight tracked documents stated
one while exactly ONE was right. The one that was right, ABI.md, is the one a
guard already checks (check-doc-consistency.sh section 3 compares the documented
SET to engine/imports.go). Every document no guard checked was wrong, three of
them by five. That is not eleven stale numbers; it is one missing check.

CLAUDE.md names the class -- "a count of a growing population is guaranteed to
be wrong, and the only question is when" -- and CLAUDE.md was itself in the
census, stale about its own subject.

WHAT THIS DOES NOT COVER, stated because a guard's silence is otherwise read as
coverage:

  * Archives (IMPROVEMENT-PLAN*, REVIEW-2026*, REMEDIATION-PLAN*, CHANGELOG).
    A dated record of a past count is a fact about that date and must keep its
    number.
  * SDK trees (python-sdk/, packages/, crates/, examples/, web/). Those state
    their OWN surface -- "36 host-call names that the Python SDK stubs" -- which
    is a different denominator that legitimately differs from the engine's. See
    "SDK surfaces are not comparable". Their drift is real and is not this
    guard's business; forcing them to agree would be a worse error than the one
    being fixed.

HOW IT LOOKS, and why not the obvious way:

  * PARAGRAPH-AWARE, NOT LINE-ORIENTED. The PRINCIPLES.md claim wrapped across
    two lines, so "HostCall imports" and "52" were never on one line. A grep
    sweep returned EMPTY and was nearly published as "no other occurrences".
    Its positive control -- the same pattern against a line already known to
    match -- failed, which is the only reason the census exists at all.
    --self-test re-runs that control on every invocation of the test.

  * TIGHT, adjacency-based patterns rather than "a number near a host-call
    word". The loose version returned 233 hits, almost all bit-field ranges
    ("responseLen 40-63") in tables about host calls. A guard that cries wolf is
    turned off. The loose scan was kept as the DISCOVERY pass -- its job was to
    disagree with the tight one, and it did: it found four sites the tight one
    misses, which were fixed by hand. The two derivations are not
    interchangeable and neither is the check on its own.

  * A FLOOR. "0 disagreements" and "0 paragraphs examined" look identical, and
    the second is what a changed doc format produces.

Deliberately-historical mentions in covered files are declared in
scripts/host-call-count-baseline.txt, keyed by path, count and a verbatim
substring rather than a line number -- so an entry dies when its sentence is
rewritten instead of drifting silently onto another one.

Usage: scripts/check-host-call-counts.py [--self-test]
"""

import os
import re
import subprocess
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BASELINE = os.path.join(REPO, 'scripts', 'host-call-count-baseline.txt')

SKIP_FILE = re.compile(r'^(IMPROVEMENT-PLAN|REVIEW-2026|REMEDIATION-PLAN|CHANGELOG)')
SKIP_DIR = ('python-sdk/', 'packages/', 'crates/', 'examples/', 'web/')

# Adjacency, in the constructions people actually write. Each must capture the
# number in group(1).
PATTERNS = [
    re.compile(r'\b(\d{2,3})\s+(?:cleat\s+)?host[-\s]?(?:call|function)s?\b', re.I),
    re.compile(r'host[-\s]?(?:call|function)s?[^.|]{0,60}?'
               r'\b(?:is|are|totals?|count is)\s*\**\s*(\d{2,3})\b', re.I),
    re.compile(r'\b(\d{2,3})\s+(?:`?extern`?\s+)?imports?\b[^.|]{0,40}ABI\.md', re.I),
    re.compile(r'\b(\d{2,3})\s+import declarations', re.I),
]

# Below 20 is never this count and never has been; above 99 is not a count of
# host calls in any plausible future of this ABI.
PLAUSIBLE = range(20, 100)


def live_count():
    src = open(os.path.join(REPO, 'engine', 'imports.go')).read()
    return len(set(re.findall(r'\.Export\("([^"]+)"\)', src)))


def tracked_markdown():
    # git ls-files, never rglob or find: those descend into .claude/worktrees/,
    # a whole second copy of the repo, and attribute a finding to a scratch
    # checkout -- the scope mistake that makes a guard MORE permissive as the
    # working tree gets messier.
    out = subprocess.run(['git', 'ls-files', '*.md'], cwd=REPO,
                         capture_output=True, text=True, check=True).stdout
    return [p for p in out.split('\n')
            if p and not SKIP_FILE.match(os.path.basename(p))
            and not p.startswith(SKIP_DIR)]


def paragraphs(text):
    """(start_line, whitespace-flattened text) per blank-line-delimited block."""
    line = 1
    for block in re.split(r'\n\s*\n', text):
        yield line, re.sub(r'\s+', ' ', block).strip()
        line += block.count('\n') + 2


def mentions(path, text):
    seen = set()
    for start, flat in paragraphs(text):
        for pat in PATTERNS:
            for m in pat.finditer(flat):
                n = int(m.group(1))
                if n not in PLAUSIBLE or (start, m.start(), n) in seen:
                    continue
                seen.add((start, m.start(), n))
                yield path, start, n, flat[max(0, m.start() - 70):m.end() + 60]


def load_baseline():
    entries = []
    if not os.path.exists(BASELINE):
        return entries
    for raw in open(BASELINE):
        line = raw.split('#', 1)[0].rstrip()
        if not line.strip():
            continue
        parts = line.split('\t')
        if len(parts) != 3:
            print(f'ERROR: malformed baseline line (want path<TAB>count<TAB>substring): '
                  f'{raw.rstrip()}', file=sys.stderr)
            sys.exit(2)
        entries.append((parts[0], int(parts[1]), parts[2]))
    return entries


def main():
    expected = live_count()
    if expected == 0:
        print('ERROR: extracted 0 exports from engine/imports.go; has the '
              'builder.Export(...) form changed?', file=sys.stderr)
        return 1

    # VACUITY CONTROL, and it runs on every invocation rather than only under
    # --self-test. The obvious control -- "the scan found at least N live
    # mentions" -- is unavailable here, and the reason is the point: the policy
    # this guard enforces is that NO covered document restates the count, so the
    # steady state is zero mentions and a population floor could never be
    # satisfied. A floor over a population that is supposed to be empty is a
    # guard that must be switched off the day it starts working.
    #
    # The synthetic known-positives below cannot go extinct. If the extractor
    # stops seeing a count it is handed on purpose, nothing it reports about the
    # tree means anything, and that is a failure rather than a clean run.
    if self_test(quiet=True) != 0:
        print('ERROR: the extractor failed its own known-positive controls, so '
              'a clean scan of the tree would mean nothing. Run '
              'scripts/check-host-call-counts.py --self-test.', file=sys.stderr)
        return 1

    baseline = load_baseline()
    texts, found, bad, used = {}, 0, [], set()

    for path in tracked_markdown():
        try:
            texts[path] = open(os.path.join(REPO, path), encoding='utf-8').read()
        except (OSError, UnicodeDecodeError):
            continue
        for p, line, n, ctx in mentions(path, texts[path]):
            found += 1
            if n == expected:
                continue
            hit = next((b for b in baseline
                        if b[0] == p and b[1] == n and b[2] in texts[path]), None)
            if hit:
                used.add(hit)
                continue
            bad.append((p, line, n, ctx))

    rc = 0
    if bad:
        print(f'ERROR: {len(bad)} documented host-call count(s) disagree with '
              f'engine/imports.go ({expected}):', file=sys.stderr)
        for p, line, n, ctx in bad:
            print(f'    {p}:~{line}: says {n}', file=sys.stderr)
            print(f'        ...{ctx}...', file=sys.stderr)
        print('\nPoint at `ABI.md` §2 rather than restating the number. This '
              'count has moved five times in eight days, and a reader takes the '
              'number and skips the command -- which is what the number is for.',
              file=sys.stderr)
        print('If the mention is deliberately historical, declare it in '
              f'{os.path.relpath(BASELINE, REPO)}.', file=sys.stderr)
        rc = 1

    stale = [b for b in baseline
             if b not in used and not (b[0] in texts and b[2] in texts[b[0]])]
    if stale:
        print('ERROR: baseline entries whose text is no longer in the file -- '
              'the sentence was rewritten or removed, so delete the entry:',
              file=sys.stderr)
        for p, n, s in stale:
            print(f'    {p}\t{n}\t{s}', file=sys.stderr)
        rc = 1

    if rc == 0:
        print(f'OK: extractor controls pass; {found} host-call count '
              f'statement(s) across {len(texts)} covered markdown files agree '
              f'with engine/imports.go ({expected}); {len(used)} historical '
              f'mention(s) baselined.')
    return rc


def self_test(quiet=False):
    """The controls that were missing when the grep sweep returned empty.

    A negative control asks "can it see the state it looks for". A known-positive
    asks the harder question -- "does it report a case that is genuinely broken"
    -- and those come apart, because "it passes when everything is fine" is
    satisfied by every broken version of a guard.
    """
    ok = True

    def say(msg):
        if not quiet:
            print(msg)

    # KNOWN-POSITIVE 1: a count that wraps onto a later line. This is the exact
    # shape that defeated the line-oriented sweep.
    wrapped = ("**Do this:** route every external interaction through the\n"
               "imports on the `env` module, of which the engine exposes 52\n"
               "host functions.\n")
    hits = [n for _, _, n, _ in mentions('control.md', wrapped)]
    good = 52 in hits
    say(f'known-positive (count wrapping onto a later line): '
        f'{"PASS" if good else "FAIL"} {hits}')
    ok &= good

    # KNOWN-POSITIVE 2: the "the registered set is N" form, which carries no
    # adjacency between the number and the noun.
    prose = "The registered set of host functions is 49 today.\n"
    hits = [n for _, _, n, _ in mentions('control.md', prose)]
    good = 49 in hits
    say(f'known-positive ("...host functions is N"): '
        f'{"PASS" if good else "FAIL"} {hits}')
    ok &= good

    # KNOWN-POSITIVE 3 and 4: the two ABI.md-referring forms. Every pattern
    # needs a control that ONLY it satisfies. Measured 2026-09-13 by killing the
    # patterns one at a time: with two controls, three of the four could die
    # without the controls noticing, because control 1 happened to be matched by
    # two patterns at once. A control that several patterns satisfy tests none
    # of them.
    hdr = "A `cleat.h` header declaring the 59 `extern` imports (see ABI.md §2).\n"
    hits = [n for _, _, n, _ in mentions('control.md', hdr)]
    good = 59 in hits
    say(f'known-positive ("N `extern` imports ... ABI.md"): '
        f'{"PASS" if good else "FAIL"} {hits}')
    ok &= good

    gen = "Zig's comptime could auto-generate the 59 import declarations.\n"
    hits = [n for _, _, n, _ in mentions('control.md', gen)]
    good = 59 in hits
    say(f'known-positive ("N import declarations"): '
        f'{"PASS" if good else "FAIL"} {hits}')
    ok &= good

    # NEGATIVE CONTROL 1: a bit-field table about host calls. The loose version
    # of this scan returned 233 hits, almost all of these.
    table = ("| host call | result layout | bit 31 lands in |\n"
             "|---|---|---|\n"
             "| `cleat_call`, `plugin_call` | `responseLen` 40-63, `errCode` 0-7 | x |\n")
    hits = [n for _, _, n, _ in mentions('control.md', table)]
    good = not hits
    say(f'negative control (bit-field ranges in a host-call table): '
        f'{"PASS" if good else "FAIL"} {hits}')
    ok &= good

    # NEGATIVE CONTROL 2: a different denominator. The WIT world's import count
    # is not the engine's export count and must never be forced to agree.
    wit = ("a real WIT world (`python-sdk/wit/cleat.wit`, 52 function imports across\n"
           "19 interfaces) wired through `componentize-py`, not stubs.\n")
    hits = [n for _, _, n, _ in mentions('control.md', wit)]
    good = not hits
    say(f'negative control (WIT import count, a different denominator): '
        f'{"PASS" if good else "FAIL"} {hits}')
    ok &= good

    return 0 if ok else 1


if __name__ == '__main__':
    sys.exit(self_test() if '--self-test' in sys.argv else main())
