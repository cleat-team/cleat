#!/usr/bin/env python3
"""Refuse a readiness scorecard that presents a closed gap as open.

docs/full-stack-readiness.md answers "is cleat ready", row by row, and each row
in its gap table cites the issue carrying the evidence. That makes it the one
document where a wrong claim is most expensive -- and the only doc-vs-reality
surface with no guard.

MEASURED, WHICH IS WHY THIS EXISTS. On 2026-09-18 the file had not been touched
in 125 commits. Every one of the ten gaps it tracked was closed, nine of them
COMPLETED, while the document still said "Four of ten miss". A reader deciding
whether to adopt cleat was being told four bars failed that had shipped.

THE FAILURE DIRECTION IS UNUSUAL AND WORTH NAMING. Most stale documents
oversell. This one UNDERSOLD, which is why nobody noticed: an overclaim gets
contradicted the first time someone tries the feature, an underclaim just
quietly loses the evaluation.

WHAT IT CHECKS: every issue cited in the gap table. If the issue is closed, the
row must say so -- "shipped", "done", or "closed". If it is open, the row must
not claim it shipped. Both directions, because a row claiming a shipped gap is
still open is the same defect pointed the other way.

WHAT IT DOES NOT CHECK: whether the prose around the table agrees with the
table. "Four of ten miss" is a sentence, and counting it reliably would need to
understand the document rather than parse it. The table is the part that cites
evidence, so the table is the part that is checkable -- and a table that is
right is enough to make a wrong sentence next to it obvious.
"""

import json
import re
import subprocess
import sys

DOC = "docs/full-stack-readiness.md"

# A gap-table row: a leading index, then cells, one of which carries a [#NNNN]
# link. Matching the LINK rather than the row shape, because the table's column
# layout is prose and will change.
ISSUE_LINK = re.compile(r"\[#(\d+)\]\(https://github\.com/[^)]*/issues/(\d+)\)")

# An EXPLICIT marker, not any occurrence of a word like "done".
#
# The first version accepted "shipped", "done", "closed", "resolved" or "fixed"
# anywhere in the row, and the self-test below caught it immediately: a gap
# described as "Long done" was read as a status. Those words appear in ordinary
# prose about what a gap IS, so matching them means a row can claim shipped by
# accident -- which is the failure this guard exists to prevent, arriving
# through the guard itself.
#
# `**shipped**` is the convention docs/full-stack-readiness.md already uses
# (row 8 of its gap table). Requiring the emphasis makes it a deliberate act.
SHIPPED_MARKER = re.compile(r"\*\*\s*shipped\s*\*\*", re.IGNORECASE)

# CLOSED IS TWO DIFFERENT FACTS and they need different rows. An issue closed
# COMPLETED shipped; one closed NOT_PLANNED was DECLINED, and its gap may be
# entirely intact -- #1571, the queryable read model, is exactly that. Marking
# it "shipped" would be a lie and reopening it would misrepresent a deliberate
# scope decision, so the doc needs a third word and this needs to know it.
#
# Surfaced by running the guard against the real document rather than by
# designing it: the first version offered only "shipped or reopen".
DECLINED_MARKER = re.compile(r"\*\*\s*(declined|not planned|out of scope)\s*\*\*", re.IGNORECASE)


def rows_with_issues(text):
    """Yield (line_number, line, issue_number) for every cited issue in a table row."""
    for i, line in enumerate(text.split("\n"), start=1):
        if not line.lstrip().startswith("|"):
            continue
        for m in ISSUE_LINK.finditer(line):
            label, href = m.group(1), m.group(2)
            if label != href:
                # A link whose text and target disagree is its own defect: a
                # reader follows the number they can see.
                yield i, line, None, f"cites #{label} but links to issue {href}"
                continue
            yield i, line, int(label), None


def says_shipped(line):
    return SHIPPED_MARKER.search(line) is not None


def says_declined(line):
    return DECLINED_MARKER.search(line) is not None


def check(text, state_of):
    """Return a list of complaints. state_of(n) -> "OPEN" | "CLOSED"."""
    problems = []
    seen = 0
    for lineno, line, issue, malformed in rows_with_issues(text):
        if malformed:
            problems.append(f"{DOC}:{lineno}: {malformed}")
            continue
        seen += 1
        state, reason = state_of(issue)
        shipped, declined = says_shipped(line), says_declined(line)
        if state == "CLOSED" and reason == "NOT_PLANNED" and not declined:
            problems.append(
                f"{DOC}:{lineno}: #{issue} was closed NOT_PLANNED -- declined, not shipped -- "
                f"but this row does not say so.\n"
                f"    Its gap may be entirely real; what changed is that nobody intends to "
                f"close it. Mark the row **declined** and say why.\n"
                f"    {line.strip()}"
            )
        elif state == "CLOSED" and reason != "NOT_PLANNED" and not shipped:
            problems.append(
                f"{DOC}:{lineno}: #{issue} is CLOSED but this row still presents it as an "
                f"open gap.\n"
                f"    A reader deciding whether to adopt cleat is being told this bar "
                f"fails when it may not.\n"
                f"    Mark the row **shipped**, or reopen the issue if the gap is real.\n"
                f"    {line.strip()}"
            )
        elif state == "OPEN" and (shipped or declined):
            problems.append(
                f"{DOC}:{lineno}: #{issue} is OPEN but this row claims it is settled.\n"
                f"    {line.strip()}"
            )
    # A scan that matched nothing reports a clean document, which reads
    # identically to success.
    if seen == 0:
        problems.append(
            f"{DOC}: no issue links were found in any table row. Either the gap table "
            f"is gone or ISSUE_LINK no longer matches how it cites issues -- and a scan "
            f"that measures nothing passes."
        )
    return problems


def github_state(issue):
    out = subprocess.run(
        ["gh", "issue", "view", str(issue), "--json", "state,stateReason"],
        capture_output=True, text=True, check=True,
    )
    d = json.loads(out.stdout)
    return d["state"].upper(), (d.get("stateReason") or "").upper()


SELF_TEST_DOC = """
| | Gap | Issue | Kind |
|---|---|---|---|
| 1 | Still open | [#100](https://github.com/x/y/issues/100) | security |
| 2 | Long done | [#200](https://github.com/x/y/issues/200) | feature |
| 3 | Done and said so -- **shipped** | [#300](https://github.com/x/y/issues/300) | feature |
| 4 | Open but claimed **shipped** | [#400](https://github.com/x/y/issues/400) | feature |
| 5 | Mismatched link | [#500](https://github.com/x/y/issues/501) | feature |
| 6 | Declined and said so -- **declined** | [#600](https://github.com/x/y/issues/600) | feature |
| 7 | Declined but presented as open | [#700](https://github.com/x/y/issues/700) | feature |
"""

SELF_TEST_STATES = {
    100: ("OPEN", ""),
    200: ("CLOSED", "COMPLETED"),
    300: ("CLOSED", "COMPLETED"),
    400: ("OPEN", ""),
    500: ("CLOSED", "COMPLETED"),
    600: ("CLOSED", "NOT_PLANNED"),
    700: ("CLOSED", "NOT_PLANNED"),
}


def self_test():
    """Prove the scan REPORTS a planted defect, not merely that it stays quiet.

    A guard that quietly stops matching reports a clean document, which reads
    identically to success. The known-positives below are what distinguish
    them.
    """
    problems = check(SELF_TEST_DOC, lambda n: SELF_TEST_STATES[n])
    joined = "\n".join(problems)

    expectations = [
        ("#200 is CLOSED", "a closed issue presented as an open gap must be reported"),
        ("#400 is OPEN", "an open issue claimed as shipped must be reported"),
        ("cites #500 but links to issue 501", "a link whose text and target disagree must be reported"),
        ("#700 was closed NOT_PLANNED", "a declined gap presented as open must be reported"),
    ]
    failures = []
    for needle, why in expectations:
        if needle not in joined:
            failures.append(f"  MISSED: {why}\n    (looked for {needle!r})")

    # And the negative controls: rows 1 and 3 are correct and must be silent.
    if "#100" in joined:
        failures.append("  FALSE POSITIVE: an open issue on an open-gap row was reported")
    if "#600" in joined:
        failures.append("  FALSE POSITIVE: a declined issue on a row marked **declined** was reported")
    if "#300" in joined:
        failures.append("  FALSE POSITIVE: a closed issue on a row marked **shipped** was reported")
    # The bug the self-test caught on its first run: row 2's gap is described as
    # "Long done", and an earlier version read that incidental word as a status.
    if "Long done" not in SELF_TEST_DOC:
        failures.append("  the fixture lost the case that caught the loose matcher")

    # And the empty-document case, which is how this stops measuring anything.
    if not check("# nothing here\n", lambda n: ("OPEN", "")):
        failures.append("  MISSED: a document with no issue links must be reported, "
                        "not treated as clean")

    if failures:
        print("self-test FAILED:\n" + "\n".join(failures), file=sys.stderr)
        return 1
    print("self-test passed")
    return 0


def main():
    if "--self-test" in sys.argv:
        return self_test()
    with open(DOC, encoding="utf-8") as fh:
        text = fh.read()
    problems = check(text, github_state)
    if problems:
        print("\n".join(problems), file=sys.stderr)
        print(
            f"\n{DOC} is the document that answers 'is this ready'. A row that "
            f"presents a shipped gap as open costs an evaluation quietly, because "
            f"nothing contradicts an underclaim.",
            file=sys.stderr,
        )
        return 1
    print(f"OK: {DOC} agrees with the issue tracker")
    return 0


if __name__ == "__main__":
    sys.exit(main())
