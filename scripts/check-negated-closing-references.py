#!/usr/bin/env python3
"""A negated closing keyword still closes the issue it names (cleat#2436).

GitHub links `close(s|d)`/`fix(es|ed)`/`resolve(s|d)` followed by a bare `#N` regardless of the English
around it: "Deliberately does **not** close #2402." registers as closing #2402. The negation is invisible
to GitHub's own parser, so a PR body or commit message can satisfy `closingIssuesReferences` -- and any
required check gated on it -- with prose that says the opposite of what will happen.

THIS IS NOT cleat#2072's CHECK (scripts/check-closing-references.sh). That one catches the FORM defect:
`cleat#N` renders as text and links nothing, so it never reaches `closingIssuesReferences` at all. This
catches the SEMANTICS defect: a correctly-formed bare `#N` that DOES link, and the text around it disclaims
it. Do not fold the two together -- #2072's half is enforced and covered; this is what was still open.

THE AUTHORITY IS `closingIssuesReferences`, NOT A TEXT SCAN. WS-4 (cleat#2436's addenda) measured a bare
text-pattern scan for "negation near a closing keyword" against real PRs and got THREE false positives --
"does not close **it**" (a pronoun) and "nothing to **fix** there" (unrelated prose) -- because neither PR
had anything in `closingIssuesReferences` at all. Gating on the field first, then searching only for a
negation near the SPECIFIC number the field names, excludes both: a pattern match with no live reference is
not a finding.

WHY A NEGATION MUST BE "NEAR" THE REFERENCE, NOT ANYWHERE IN THE TEXT. "This does not affect anything.
Closes #1973." has a negation and a real closing reference in the same body, and closing #1973 is entirely
correct -- the negation is about something else. `[^.\\n;]` bounds the search to one clause so a period,
newline or semicolon between an unrelated negation and the reference breaks the match.

Exit status, and why there are three (see CLAUDE.md, "Is this result real?"):

  0  no negated closing reference found (including: no closing reference at all)
  1  a finding: a closing reference is negated in the text that names it
  2  this check could not establish what it measures -- CLOSING_ISSUES is not valid JSON, or the inputs
     this script depends on (PR_BODY, COMMIT_MESSAGES, CLOSING_ISSUES) were not supplied. A scan that
     silently treated "cannot parse the field" as "no closing references" would agree with every PR,
     correct or not.

`--self-test` asserts on the TEXT of each verdict against fixture inputs, not only the status.
"""
import json
import os
import re
import sys

# A closing keyword, optionally negated, followed (within one clause) by a bare `#N`. Both orderings of
# negation and keyword are covered: "does not close #N" (negation first, the shape of every real instance
# found so far) and "closes, but not, #N" (keyword first) -- see NEGATED_AFTER below for the second.
#
# [^.\n;] bounds each side to one clause. A `#N` is NOT anchored with \b on its left: `cleat#2340` and
# `#2340` both match here on purpose -- the CLOSING_ISSUES cross-check below is what tells a live link
# (`#2340`) apart from an inert one (`cleat#2340`), not this pattern. Anchoring here would just duplicate
# that filter in a second, weaker form.
NEGATED_BEFORE = re.compile(
    r"\b(?:not|never|n't|cannot|can't|won't)\b"
    r"[^.\n;]{0,40}?"
    r"\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\b"
    r"[^.\n;]{0,20}?"
    r"#(\d+)",
    re.IGNORECASE,
)
NEGATED_AFTER = re.compile(
    r"\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\b"
    r"[^.\n;]{0,20}?"
    r"\b(?:not|never|n't|cannot|can't|won't)\b"
    r"[^.\n;]{0,40}?"
    r"#(\d+)",
    re.IGNORECASE,
)


class Unmeasured(Exception):
    pass


def find_negated_references(text):
    """Returns {issue_number: [snippet, ...]} for every negated-keyword-then-#N match in text."""
    found = {}
    for pattern in (NEGATED_BEFORE, NEGATED_AFTER):
        for m in pattern.finditer(text):
            n = int(m.group(1))
            snippet = " ".join(text[max(0, m.start() - 10) : m.end() + 5].split())
            found.setdefault(n, []).append(snippet)
    return found


def check(pr_body, commit_messages, closing_issues_json):
    try:
        closing_issues = set(json.loads(closing_issues_json))
    except (json.JSONDecodeError, TypeError) as e:
        raise Unmeasured(f"CLOSING_ISSUES is not valid JSON ({closing_issues_json!r}): {e}")

    if not closing_issues:
        return [], "No closing issue references on this PR."

    combined = "PR BODY:\n" + pr_body + "\n\nCOMMIT MESSAGES:\n" + commit_messages
    candidates = find_negated_references(combined)

    # THE CROSS-CHECK. A candidate whose number is not in closingIssuesReferences is either the inert
    # `cleat#N` form (cleat#2072's subject, not this one) or a plain mention this script's own pattern
    # happened to match with no real link behind it -- either way, GitHub is not going to close it, so it
    # is not this check's finding.
    problems = [(n, snippets) for n, snippets in candidates.items() if n in closing_issues]
    problems.sort()

    if not problems:
        return [], f"OK: {len(closing_issues)} closing reference(s), no negation found near any."
    return problems, None


def report(problems):
    lines = [
        "=" * 44,
        "  NEGATED CLOSING REFERENCE -- FAILED",
        "=" * 44,
        "",
        "GitHub will CLOSE these issues when this PR merges, even though the text",
        "naming them says otherwise:",
        "",
    ]
    for n, snippets in problems:
        for s in snippets:
            lines.append(f"  #{n}: ...{s}...")
    lines += [
        "",
        "If the issue should NOT close: remove the closing keyword (write 'see #N' or",
        "'related to #N' instead of 'closes #N'), or drop the reference entirely.",
        "If it SHOULD close: remove the negation.",
    ]
    return "\n".join(lines)


def main():
    pr_body = os.environ.get("PR_BODY")
    commit_messages = os.environ.get("COMMIT_MESSAGES")
    closing_issues_json = os.environ.get("CLOSING_ISSUES")
    if pr_body is None or commit_messages is None or closing_issues_json is None:
        print("UNMEASURED: PR_BODY, COMMIT_MESSAGES and CLOSING_ISSUES must all be set.")
        return 2

    try:
        problems, ok_message = check(pr_body, commit_messages, closing_issues_json)
    except Unmeasured as e:
        print(f"UNMEASURED: {e}")
        return 2

    if not problems:
        print(ok_message)
        return 0
    print(report(problems))
    return 1


def self_test():
    failures = []

    def case(name, pr_body, commit_messages, closing_issues_json, want_status, want_substring=None):
        try:
            problems, ok_message = check(pr_body, commit_messages, closing_issues_json)
            status = 1 if problems else 0
            text = report(problems) if problems else ok_message
        except Unmeasured as e:
            status, text = 2, str(e)
        if status != want_status:
            failures.append(f"{name}: exit {status}, want {want_status} (text: {text!r})")
            return
        if want_substring is not None and want_substring not in text:
            failures.append(f"{name}: {want_substring!r} not in output: {text!r}")

    # KNOWN-POSITIVE: the exact real-world shape (cleat#2436's addenda), negation before the keyword.
    case(
        "negated-before, real link",
        "Deliberately does not close #2402.",
        "",
        json.dumps([2402]),
        1,
        "#2402",
    )

    # The other ordering.
    case(
        "negated-after, real link",
        "This closes, but not, #2402.",
        "",
        json.dumps([2402]),
        1,
        "#2402",
    )

    # KNOWN-NEGATIVE: cleat#N is inert (cleat#2072's subject) -- GitHub never puts it in
    # closingIssuesReferences, so this must NOT be flagged by a check whose whole premise is the field.
    #
    # closing_issues is [9999], NOT [] -- an empty field short-circuits at the "no closing issue
    # references" guard before the cross-check this case exists to exercise ever runs, which is exactly
    # how this case was wrong the first time it was written (caught by a deliberate mutation: removing the
    # cross-check left every self-test case passing, because none of them reached it). [9999] represents a
    # real, unrelated closing reference elsewhere in the same PR, so the cross-check has actual work to do:
    # exclude candidate 2402 (present, negated, inert form) while leaving 9999 alone (present in the field,
    # no negated mention of it in this text).
    case(
        "negated, but cleat#N form (inert, not in the field)",
        "Deliberately does not close cleat#2402. Closes #9999.",
        "",
        json.dumps([9999]),
        0,
    )

    # A field entry that IS real but the body's negation is for a DIFFERENT clause -- the false positive a
    # bare "negation anywhere" scan would produce, measured by WS-4 on real PRs.
    case(
        "negation present, unrelated to the reference",
        "This does not affect anything. Closes #1973.",
        "",
        json.dumps([1973]),
        0,
    )

    # WS-4's own false positives: a negation near a keyword with no adjacent number at all.
    case(
        "negation near keyword, no number nearby (pronoun)",
        "This does not close it, only marks it in progress.",
        "",
        json.dumps([]),
        0,
    )
    case(
        "negation near a different keyword, no number nearby",
        "Nothing to fix there.",
        "",
        json.dumps([]),
        0,
    )

    # Ordinary, correct, UNNEGATED closing reference: must never be flagged.
    case(
        "clean closing reference",
        "Closes #1973.",
        "",
        json.dumps([1973]),
        0,
    )

    # No closing references on the PR at all.
    case(
        "no closing issues",
        "Nothing to close here.",
        "",
        json.dumps([]),
        0,
        "No closing issue references",
    )

    # THE COMMIT-MESSAGE SURFACE (Addendum 1): the negation lives in a commit message, not the PR body.
    case(
        "negation in a commit message, not the body",
        "See the commits.",
        "Does not close #1991: this is step 1 of the owner's ordering.",
        json.dumps([1991]),
        1,
        "#1991",
    )

    # UNMEASURED: the precondition (valid JSON) fails, and that must be its own status, not a silent pass.
    # Passed as a genuinely broken string, not json.dumps()'d -- that would just produce a valid JSON
    # string literal and defeat the point of this case.
    case(
        "malformed CLOSING_ISSUES",
        "Closes #1973.",
        "",
        "{not valid json",
        2,
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
