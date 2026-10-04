"""A closing keyword written inside a code span is INERT, and says nothing.

cleat#3028. GitHub links an issue to a pull request -- and closes it when the PR
merges -- only for a closing keyword in the PR body's *prose*. Text inside an
inline code span or a fenced block is not parsed for it, so

    `Closes #3018`

is correct, well-formed, and does nothing. The PR merges green and the issue
stays open, and the only detector is a person asking why.

THE MEASUREMENT THIS RESTS ON. Two of the last forty merged PR bodies contain a
code-spanned well-formed closer, and they are the two possible cases:

  #3021  `Closes #3018`   closingIssuesReferences: []      issue stayed OPEN
  #3038  `Closes #3003`   closingIssuesReferences: [3003]  issue CLOSED

#3038 is the case that makes this check safe to have: its zero is a PR QUOTING
the syntax while explaining it -- its own body says so, "from the real
`Closes #3003`" -- and it has a live closer elsewhere. THE DISCRIMINATOR IS
`closingIssuesReferences`: flag a code-spanned closer only when the issue it
names is not in that list, because that list is GitHub's own answer to "what
will this PR close" and a nearer reading of the intent is not available.

WHAT IT MUST NOT DO. Both other closing guards exempt code spans deliberately,
because a PR that QUOTES the malformed form (`Closes cleat#N`) must not be
flagged for quoting it -- that exemption is this guard's sibling's own
first-run failure. This check is ADDITIVE and must not revoke it: it never
looks at `cleat#N`, and #3038 is the acceptance fixture (a code span whose issue
IS closed live elsewhere must pass).

STATUSES. 0 nothing trapped, 1 a trapped closer, 2 could not establish what it
measures -- the same three-way split `check-negated-closing-references.py`
uses, and for the same reason: a guard that measures nothing agrees with every
tree, so it must not report the finding's status.
"""

import os
import re
import sys

# Explicit alternations rather than `fix(?:e[sd])?`, which matches "fixe" in
# "fixed" and reports it as the keyword "fixe".
KEYWORD = r"(?:close[sd]?|fix(?:ed|es)?|resolve[sd]?)"

# A WELL-FORMED reference: whitespace then `#N`. `Closes cleat#N` does not match,
# because a repo name sits between the keyword and the `#` -- that is the FORM
# guard's subject, and this one must not touch it.
CLOSER = re.compile(KEYWORD + r"[ \t]+#(\d+)", re.I)

# A code span: one line, between single backticks. Fenced blocks are handled
# separately because a multi-line span is not what the reported case looked
# like, and erring toward NOT flagging is the direction this guard should err.
SPAN = re.compile(r"`[^`\n]*`")
FENCE = re.compile(r"^[ \t]*(```|~~~).*?^\1[^\n]*$", re.S | re.M)


def trapped(body, closed):
    """Return {issue: the span} for closers that are in a code span and inert."""
    found = {}
    for span in SPAN.findall(FENCE.sub("", body)):
        for m in CLOSER.finditer(span):
            n = int(m.group(1))
            if n not in closed:
                found.setdefault(n, span.strip())
    return found


def main():
    body = os.environ.get("PR_BODY")
    closing = os.environ.get("CLOSING_ISSUES")
    if body is None or closing is None:
        which = " and ".join(
            n for n, v in (("PR_BODY", body), ("CLOSING_ISSUES", closing)) if v is None
        )
        print(f"UNMEASURED: {which} must be set.", file=sys.stderr)
        print(
            "            Without them this check cannot tell a code span from a live\n"
            "            reference, and would agree with every tree. (This is the check's\n"
            "            own precondition failing; it says nothing about the PR.)",
            file=sys.stderr,
        )
        return 2

    closed = {int(n) for n in re.findall(r"\d+", closing)}
    hits = trapped(body, closed)
    if not hits:
        print("No closing keyword is trapped in a code span.")
        return 0

    print("=" * 44)
    print("  CODE-SPAN CLOSING KEYWORD -- FAILED")
    print("=" * 44)
    print()
    print("GitHub does not read a closing keyword inside a code span, so these will")
    print("NOT close the issues they name when this PR merges:")
    print()
    for n, span in sorted(hits.items()):
        print(f"  #{n}: {span}")
    print()
    # THE REMEDY IS PART OF THE OUTPUT, not left to the reader: this context is
    # REQUIRED (confirmed against branches/develop/protection), so a match blocks
    # the merge, and a guard that blocks without saying how to proceed is
    # cleat#2510's subject -- there, a stale "not required" read as permission not
    # to act. Both remedies below are real and one of them applies.
    print("WHAT TO DO -- one of these:")
    print()
    print("  If you MEANT this to close the issue: remove the backticks, so the")
    print("  keyword is prose and GitHub reads it.")
    print()
    print("  If you are DOCUMENTING that a code-spanned closer is inert: this is the")
    print("  one class this check cannot tell from the real thing, and it is expected")
    print("  here. Use a form that cannot close anything -- a placeholder,")
    print("  `Closes #<n>`, or the malformed `Closes cleat#N` -- which still shows the")
    print("  point and is not flagged. (That is what both sibling guards do in their")
    print("  own comments, for this reason.)")
    print()
    print(f"  Discriminator: {sorted(hits)} {'is' if len(hits) == 1 else 'are'} named in")
    print("  closingIssuesReferences, so nothing live is closing it.")
    return 1


if __name__ == "__main__":
    sys.exit(main())
