#!/usr/bin/env python3
"""Refuse to publish an rc-* image from a commit nobody reviewed.

develop-image.yml publishes rc-<sha> on every push to release/**. The `release/**`
ruleset requires a pull request to UPDATE such a branch, but it has no `creation`
rule, and every stream pushes as the owner's identity, so a creation rule with an
owner bypass would not constrain them either. Anyone with write access can create
release/x at an arbitrary commit; the branch then reads as "covered by a ruleset",
and the job would publish unreviewed code under a public tag (cleat#3398).

So the job asks about the COMMIT, which is what the tag names. A sha is reviewed
code when EITHER

  ancestor   it is an ancestor of develop, which is how a release branch starts: a
             cut is made AT a develop commit, and every develop commit arrived
             through the merge queue; or
  pr         it is the merge commit of a MERGED pull request whose base is this
             release branch -- a later fix, landed under the ruleset.

and is refused otherwise, naming which test failed.

Three outcomes, three exit statuses, and the third is not decoration:

  0  reviewed (names the arm that said so)
  1  REFUSED: both arms were read and neither holds. A finding about the commit.
  2  UNMEASURED: an arm could not be read and the other did not hold. A failure of
     the check, not a finding about the commit. Both 1 and 2 stop the job.

An unreadable arm must not be read as "no": the API being down is not evidence that
a commit is unreviewed, and reading it as a refusal would send the next person to
the commit when the fault is in the check. Equally, one arm holding is enough, so
an unreadable second arm does not block a commit the first one proves.

Usage:
  check-rc-sha-reviewed.py --repo O/R --branch release/x --sha <40-hex>
  check-rc-sha-reviewed.py --arm ancestor|pr ...   consider ONE arm only. A
        diagnostic for exercising an arm against the real API; the job never
        passes it.
  check-rc-sha-reviewed.py --self-test
"""
import argparse
import json
import re
import subprocess
import sys

OK, REFUSED, UNMEASURED = 0, 1, 2

SHA_RE = re.compile(r"^[0-9a-f]{40}$")


class Unreadable(Exception):
    """An API call failed or returned something that is not the expected shape."""


def judge(branch, sha, ancestor, pulls, arms=("ancestor", "pr")):
    """Decide from what the two arms READ. Pure: no network, no git.

    ancestor -- the compare status of develop...sha ("identical", "behind",
                "ahead", "diverged"), or None when it could not be read.
    pulls    -- the list of pull requests the API associates with sha, or None
                when it could not be read.
    Returns (exit_status, message).
    """
    reasons = []
    unread = []

    if "ancestor" in arms:
        if ancestor is None:
            unread.append("ancestor (could not compare develop...sha)")
        elif ancestor in ("identical", "behind"):
            return OK, f"reviewed: {sha} is an ancestor of develop (compare status {ancestor})"
        else:
            reasons.append(f"ancestor: {sha} is not an ancestor of develop (compare status {ancestor})")

    if "pr" in arms:
        if pulls is None:
            unread.append("pr (could not read the pull requests for the commit)")
        else:
            for p in pulls:
                base = (p.get("base") or {}).get("ref")
                if p.get("merged_at") and base == branch and p.get("merge_commit_sha") == sha:
                    return OK, f"reviewed: {sha} is the merge commit of merged PR #{p.get('number')} into {branch}"
            if not pulls:
                reasons.append(f"pr: no pull request is associated with {sha}")
            else:
                seen = ", ".join(
                    "#%s (%s, base %s%s)" % (
                        p.get("number"),
                        "merged" if p.get("merged_at") else "not merged",
                        (p.get("base") or {}).get("ref"),
                        "" if p.get("merge_commit_sha") == sha else ", merge commit is not this sha",
                    )
                    for p in pulls
                )
                reasons.append(f"pr: none of the pull requests for {sha} is a merged PR into {branch}: {seen}")

    if unread:
        return UNMEASURED, (
            "UNMEASURED: " + "; ".join(unread) + (
                "; the arm(s) that were read do not hold: " + "; ".join(reasons) if reasons else ""
            ) + "\nThis is a failure of the check, not a finding about the commit."
        )
    return REFUSED, f"REFUSED: {sha} on {branch} is not reviewed code.\n  " + "\n  ".join(reasons)


def gh(args, run=subprocess.run):
    r = run(["gh", "api", *args], capture_output=True, text=True)
    if r.returncode != 0:
        raise Unreadable((r.stderr or r.stdout).strip() or f"gh api exited {r.returncode}")
    return r.stdout


def read_ancestor(repo, sha, call=gh):
    try:
        out = call([f"repos/{repo}/compare/develop...{sha}", "--jq", ".status"])
    except Unreadable:
        return None
    status = out.strip()
    return status or None


def read_pulls(repo, sha, call=gh):
    try:
        out = call(["--paginate", "--slurp", f"repos/{repo}/commits/{sha}/pulls"])
        pages = json.loads(out)
    except (Unreadable, ValueError):
        return None
    if not isinstance(pages, list) or not all(isinstance(pg, list) for pg in pages):
        return None
    return [p for pg in pages for p in pg]


def self_test():
    B = "release/v0.5.0"
    S = "a" * 40
    T = "b" * 40
    merged_here = {"number": 7, "merged_at": "2026-10-26T00:00:00Z", "base": {"ref": B}, "merge_commit_sha": S}
    failures = []

    def want(name, got, status, *needles):
        code, msg = got
        ok = code == status and all(n in msg for n in needles)
        print(("ok   " if ok else "FAIL ") + name + f" -> {code}")
        if not ok:
            failures.append(f"{name}: got {code}: {msg!r}, want {status} containing {needles}")

    # Arm 1.
    want("ancestor (behind)", judge(B, S, "behind", []), OK, "ancestor of develop")
    want("ancestor (identical)", judge(B, S, "identical", []), OK, "ancestor of develop")
    # Arm 2.
    want("merged PR into this branch", judge(B, S, "diverged", [merged_here]), OK, "merge commit of merged PR #7")
    # Refusals, each naming the arm that failed. A test that checks the status alone
    # would pass for the wrong reason, so these assert the text.
    want("on neither", judge(B, S, "diverged", []), REFUSED, "not an ancestor of develop", "no pull request is associated")
    want("ahead is not an ancestor", judge(B, S, "ahead", []), REFUSED, "compare status ahead")
    want(
        "PR merged into ANOTHER branch",
        judge(B, S, "diverged", [dict(merged_here, base={"ref": "develop"})]),
        REFUSED, "base develop",
    )
    want(
        "PR not merged",
        judge(B, S, "diverged", [dict(merged_here, merged_at=None)]),
        REFUSED, "not merged",
    )
    want(
        "PR merged here, but sha is only its HEAD commit",
        judge(B, T, "diverged", [merged_here]),
        REFUSED, "merge commit is not this sha",
    )
    # UNMEASURED: an unreadable arm is not a "no", and must not look like a refusal.
    want("ancestor unreadable, pr says no", judge(B, S, None, []), UNMEASURED, "ancestor (could not compare", "failure of the check")
    want("pr unreadable, ancestor says no", judge(B, S, "diverged", None), UNMEASURED, "pr (could not read")
    want("both unreadable", judge(B, S, None, None), UNMEASURED, "ancestor", "pr")
    # One arm holding is enough: an unreadable sibling does not block it.
    want("ancestor holds, pr unreadable", judge(B, S, "behind", None), OK)
    want("pr holds, ancestor unreadable", judge(B, S, None, [merged_here]), OK)
    # --arm restricts what counts.
    want("only the pr arm ignores an ancestor", judge(B, S, "behind", [], arms=("pr",)), REFUSED, "no pull request")
    want("only the ancestor arm ignores a merged PR", judge(B, S, "diverged", [merged_here], arms=("ancestor",)), REFUSED, "not an ancestor")

    # The readers, against a fake `gh` that fails and that returns junk.
    def failing(_):
        raise Unreadable("HTTP 502")

    want(
        "reader: a failed compare is None, not 'diverged'",
        (UNMEASURED if read_ancestor("o/r", S, failing) is None else OK, "x"), UNMEASURED,
    )
    want(
        "reader: a failed pulls call is None, not []",
        (UNMEASURED if read_pulls("o/r", S, failing) is None else OK, "x"), UNMEASURED,
    )
    want(
        "reader: non-JSON pulls output is None",
        (UNMEASURED if read_pulls("o/r", S, lambda _: "not json") is None else OK, "x"), UNMEASURED,
    )
    want(
        "reader: a non-list pulls document is None",
        (UNMEASURED if read_pulls("o/r", S, lambda _: '{"message":"x"}') is None else OK, "x"), UNMEASURED,
    )
    pages = json.dumps([[merged_here], [dict(merged_here, number=8)]])
    flat = read_pulls("o/r", S, lambda _: pages)
    want("reader: pages are flattened", (OK if flat and [p["number"] for p in flat] == [7, 8] else REFUSED, "x"), OK)
    want("reader: an empty pulls list is [] (not None)", (OK if read_pulls("o/r", S, lambda _: "[[]]") == [] else REFUSED, "x"), OK)

    if failures:
        print("\n".join(failures), file=sys.stderr)
        return 1
    print("check-rc-sha-reviewed: self-test OK")
    return 0


def main(argv):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--repo")
    ap.add_argument("--branch")
    ap.add_argument("--sha")
    ap.add_argument("--arm", choices=["ancestor", "pr"])
    ap.add_argument("--self-test", action="store_true")
    a = ap.parse_args(argv)
    if a.self_test:
        return self_test()
    if not (a.repo and a.branch and a.sha):
        print("usage: --repo O/R --branch release/x --sha <40-hex>", file=sys.stderr)
        return UNMEASURED
    if not SHA_RE.match(a.sha):
        print(f"UNMEASURED: '{a.sha}' is not a 40-char hex sha; refusing to guess\nThis is a failure of the check, not a finding about the commit.", file=sys.stderr)
        return UNMEASURED
    arms = (a.arm,) if a.arm else ("ancestor", "pr")
    ancestor = read_ancestor(a.repo, a.sha) if "ancestor" in arms else None
    pulls = read_pulls(a.repo, a.sha) if "pr" in arms else None
    code, msg = judge(a.branch, a.sha, ancestor, pulls, arms)
    print(msg, file=sys.stderr if code else sys.stdout)
    return code


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
