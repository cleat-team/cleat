#!/usr/bin/env python3
"""A PR must declare either a closing reference or that it closes none (cleat#1962).

A PR that fixes an issue and omits a closing reference leaves that issue open after the
fix merges -- silently, since nothing goes red. Measured on develop: 12 of the last 20
merged PRs closed nothing, and the field being read correctly is confirmed by the other
8 being non-zero (an all-zero result would be equally consistent with "nobody links
anything" and "the field is being read wrong"). #1116 sat open for two days on exactly
this gap, holding #1917 behind it.

A `closingIssuesReferences`-empty PR is NOT automatically a defect -- cleat#1938 (parser
unification) and cleat#1957 (diagnostic logging) close nothing BY DESIGN, and both say so
in their own bodies, in two different shapes:

    cleat#1938: "Closes nothing. cleat#1116 remains open and is unaffected..."
    cleat#1957: "Refs #1955. Does not fix it -- makes the next occurrence diagnosable..."

Neither can be told apart from a PR that simply forgot, by the `closingIssuesReferences`
field alone -- both are empty. So this does not re-derive GitHub's own answer (that is
cleat#2072's and cleat#2436's job, upstream of this); it asks the narrower question GitHub
cannot answer at all: did the AUTHOR say anything, either way?

THIS STARTS ADVISORY, NOT REQUIRED (see the workflow file's own comment on why it is a
separate job rather than a step in `Closing References`). The issue that asks for this
check says so explicitly: "a check that blocks merges on a prose convention needs its
false-positive behaviour thought through before it is turned on." A curated phrase list
cannot cover every way of saying "this closes nothing" -- cleat#1957's own phrasing is
already a near-miss (anaphoric: "Does not fix IT", not a repeated number) -- so a false
positive here costs a visible, non-blocking notice, not a stuck merge. Promoting it to a
required check is a separate decision, made after watching its false-positive rate the
same way `Closing References` itself was.

Exit status, same three-way split as the sibling checks in this file (see CLAUDE.md,
"Is this result real?"):

  0  declared -- a real closing reference, or an explicit "closes nothing" statement
  1  a finding: no closing reference and no explicit statement either way
  2  this check could not establish what it measures -- CLOSING_ISSUES is not valid
     JSON, or PR_BODY / CLOSING_ISSUES were not supplied

`--self-test` asserts on the TEXT of each verdict against fixture inputs, not only the
status -- a self-test that only checked the exit code would pass a version that reported
the wrong REASON for it.
"""
import json
import os
import re
import sys


class Unmeasured(Exception):
    pass


# A closing keyword followed (within one clause) by "nothing" -- cleat#1938's own
# phrasing ("Closes nothing.") and its natural variants ("Fixes nothing here.",
# "Resolves nothing in this repo."). [^.\n;] bounds the search to one clause so an
# unrelated "nothing" elsewhere in the body cannot satisfy this on the keyword's behalf
# -- the same bounding NEGATED_BEFORE in check-negated-closing-references.py uses, and
# for the same reason.
BARE_NOTHING = re.compile(
    r"(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\b[^.\n;]{0,40}?\bnothing\b"
)

# A negated keyword -- "does not close", "doesn't fix", "do not resolve", "didn't
# close" -- covering both the apostrophe and the spelled-out form. This is cleat#1957's
# shape: "Refs #1955. Does not fix it." declares the PR closes nothing FOR #1955
# without repeating the number, so this intentionally does not require a number near
# the negation the way check-negated-closing-references.py's cross-check does -- that
# script asks "is a REAL link negated"; this one only asks "did the author address the
# question at all", and a bare "does not fix it" already answers it.
NEGATED_KEYWORD = re.compile(
    r"(?i)\b(?:does|do|did)\s*n['o]t\s+(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\b"
)

NO_ISSUE_PHRASES = (BARE_NOTHING, NEGATED_KEYWORD)


def check(pr_body, closing_issues_json):
    try:
        closing_issues = json.loads(closing_issues_json)
    except (json.JSONDecodeError, TypeError) as e:
        raise Unmeasured(f"CLOSING_ISSUES is not valid JSON ({closing_issues_json!r}): {e}")

    if closing_issues:
        return True, f"OK: this PR links {len(closing_issues)} issue(s): {closing_issues}."

    for pattern in NO_ISSUE_PHRASES:
        if pattern.search(pr_body):
            return True, "OK: no linked issue, but the body explicitly says this PR closes none."

    return False, (
        "No linked issue, and no explicit statement that this PR closes none.\n\n"
        "This is advisory, not required (see cleat#1962) -- but before merging, either:\n"
        "  - add a closing reference as a bare number, e.g. 'Closes #1234'\n"
        "    (not 'cleat#1234' -- see the Closing References check), or\n"
        "  - say so explicitly if this genuinely closes nothing, e.g. 'Closes nothing.'\n"
        "    or 'Does not fix it.' / 'Does not close it.' when the PR is a step toward\n"
        "    an issue it does not close yet. Do NOT repeat the number: a bare '#N' after\n"
        "    the keyword arms GitHub's own parser, and the negated-closing-reference\n"
        "    check will flag it.\n\n"
        "Silence here is exactly what left cleat#1116 open for two days after its fix\n"
        "had already merged."
    )


def main():
    pr_body = os.environ.get("PR_BODY")
    closing_issues_json = os.environ.get("CLOSING_ISSUES")
    if pr_body is None or closing_issues_json is None:
        print("UNMEASURED: PR_BODY and CLOSING_ISSUES must both be set.")
        return 2

    try:
        declared, message = check(pr_body, closing_issues_json)
    except Unmeasured as e:
        print(f"UNMEASURED: {e}")
        return 2

    print(message)
    return 0 if declared else 1


def self_test():
    failures = []

    def case(name, pr_body, closing_issues_json, want_status, want_substring):
        if not want_substring:
            failures.append(f"{name}: caller passed an empty want_substring, which matches anything")
            return
        try:
            declared, message = check(pr_body, closing_issues_json)
            status = 0 if declared else 1
        except Unmeasured as e:
            status, message = 2, str(e)
        if status != want_status:
            failures.append(f"{name}: exit {status}, want {want_status} (text: {message!r})")
            return
        if want_substring not in message:
            failures.append(f"{name}: {want_substring!r} not in output: {message!r}")

    # A real link: the field alone is enough, no text inspection needed.
    case("linked issue, nothing else required", "Adds a feature.", json.dumps([42]), 0, "links 1 issue")

    # cleat#1938's EXACT real-world phrasing.
    case(
        "bare 'Closes nothing.' (cleat#1938's own wording)",
        "Closes nothing. cleat#1116 remains open and is unaffected.",
        json.dumps([]),
        0,
        "explicitly says",
    )

    # cleat#1957's EXACT real-world phrasing -- anaphoric, no number repeated.
    case(
        "'Does not fix it' (cleat#1957's own wording)",
        "Refs #1955. Does not fix it -- makes the next occurrence diagnosable.",
        json.dumps([]),
        0,
        "explicitly says",
    )

    # Variants of the negated form, pinned individually so the alternation cannot
    # silently drop one without a test noticing.
    case("doesn't-contraction", "This doesn't close the underlying issue.", json.dumps([]), 0, "explicitly says")
    case("do-not, spelled out", "We do not resolve the root cause here.", json.dumps([]), 0, "explicitly says")
    case("didn't-contraction, past tense", "This didn't fix the flake.", json.dumps([]), 0, "explicitly says")

    # Variants of the bare-nothing form.
    case("fixes nothing", "Fixes nothing; this is a pure refactor.", json.dumps([]), 0, "explicitly says")
    case("resolves nothing", "Resolves nothing in this repo.", json.dumps([]), 0, "explicitly says")

    # THE ACTUAL GAP: no link, no declaration either way.
    case(
        "nothing declared at all",
        "Refactors the retry loop for clarity.",
        json.dumps([]),
        1,
        "No linked issue",
    )

    # A clause boundary stops the bare-nothing pattern from reaching across an
    # unrelated sentence -- same bounding discipline as the sibling negation check.
    case(
        "an unrelated 'nothing' elsewhere must not satisfy this",
        "Fixes the race. Nothing else changed in this PR.",
        json.dumps([]),
        1,
        "No linked issue",
    )

    # UNMEASURED: the precondition (valid JSON) fails.
    case(
        "malformed CLOSING_ISSUES",
        "Closes #1973.",
        "{not valid json",
        2,
        "not valid JSON",
    )

    if failures:
        print(f"{len(failures)} self-test failure(s):")
        for f in failures:
            print(f"  - {f}")
        return 1
    print("self-test: all cases passed")
    return 0


if __name__ == "__main__":
    if "--self-test" in sys.argv:
        sys.exit(self_test())
    sys.exit(main())
