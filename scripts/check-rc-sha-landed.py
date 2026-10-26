#!/usr/bin/env python3
"""Refuse to publish an rc-* image from a commit that did not land the sanctioned way.

develop-image.yml publishes rc-<sha> on every push to release/**. The `release/**`
ruleset requires a pull request (and the required status checks) to UPDATE such a
branch, but it has no `creation` rule, and every stream pushes as the owner's
identity, so a creation rule with an owner bypass would not constrain them either.
Anyone with write access can create release/x at an arbitrary commit; the branch
then reads as "covered by a ruleset", and the job would publish unreviewed code
under a public tag (cleat#3398).

So the job asks about the COMMIT, which is what the tag names. "Landed" is a claim
about the path, not about a person reading it: ruleset 25901002 sets
required_approving_review_count to 0, so a merged PR proves it went through a pull
request and the required checks, not that anyone approved it. A commit has landed
when EITHER

  ancestor   it is an ancestor of develop, which is how a release branch starts: a
             cut is made AT a develop commit, and every develop commit arrived
             through the merge queue; or
  pr         it is the merge commit of a MERGED pull request whose base is this
             release branch (a later fix) AND ITS FIRST PARENT HAS LANDED TOO.

The second clause is the whole of the walk, and it was missing from the first
version: a PR merged onto a branch that was created at an unreviewed commit X has a
merge commit M that contains all of X, and M passes the "merged PR" test on its own.
So the check follows first parents until it reaches a develop ancestor, and a
commit that is neither is refused there, naming the link that broke the chain. The
walk is bounded; running out of bound is UNMEASURED, not a pass. (The repository
allows squash and merge-commit merges, and both leave the branch's previous tip as
the first parent. Rebase merges are disabled, which is what makes first parents the
right thing to follow.)

Three outcomes, three exit statuses, and the third is not decoration:

  0  landed (names the arm that said so)
  1  REFUSED: every arm was read and none holds. A finding about the commit.
  2  UNMEASURED: an arm could not be read and the other did not hold, or the walk
     ran out of bound. A failure of the check, not a finding about the commit.
     Both 1 and 2 stop the job.

An unreadable arm must not be read as "no": the API being down is not evidence that
a commit is unreviewed, and reading it as a refusal would send the next person to
the commit when the fault is in the check. Equally, one arm holding is enough, so
an unreadable second arm does not block a commit the first one proves.

Usage:
  check-rc-sha-landed.py --repo O/R --branch release/x --sha <40-hex>
  check-rc-sha-landed.py --arm ancestor|pr ...   judge ONE commit by ONE arm, with
        no walk. A diagnostic for exercising an arm against the real API; the job
        never passes it.
  check-rc-sha-landed.py --self-test
"""
import argparse
import json
import re
import subprocess
import sys

OK, REFUSED, UNMEASURED = 0, 1, 2

SHA_RE = re.compile(r"^[0-9a-f]{40}$")

# A release branch is a handful of fix merges on top of a cut. Fifty is far past
# any real one, and past it the answer is "could not tell", never "landed".
MAX_WALK = 50


class Unreadable(Exception):
    """An API call failed or returned something that is not the expected shape."""


def judge(branch, sha, ancestor, pulls, arms=("ancestor", "pr")):
    """Decide about ONE commit from what the two arms READ. Pure: no network, no git.

    ancestor -- the compare status of develop...sha ("identical", "behind",
                "ahead", "diverged"), or None when it could not be read.
    pulls    -- the list of pull requests the API associates with sha, or None
                when it could not be read.
    Returns (exit_status, message, arm). `arm` is "ancestor" or "pr" when the
    status is OK. "pr" is NOT yet a pass for the commit: the caller owes a look at
    its first parent (walk below).
    """
    reasons = []
    unread = []

    if "ancestor" in arms:
        if ancestor is None:
            unread.append("ancestor (could not compare develop...sha)")
        elif ancestor in ("identical", "behind"):
            return OK, f"{sha} is an ancestor of develop (compare status {ancestor})", "ancestor"
        else:
            reasons.append(f"ancestor: {sha} is not an ancestor of develop (compare status {ancestor})")

    if "pr" in arms:
        if pulls is None:
            unread.append("pr (could not read the pull requests for the commit)")
        else:
            for p in pulls:
                base = (p.get("base") or {}).get("ref")
                if p.get("merged_at") and base == branch and p.get("merge_commit_sha") == sha:
                    return OK, f"{sha} is the merge commit of merged PR #{p.get('number')} into {branch}", "pr"
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
        ), None
    return REFUSED, "\n  ".join(reasons), None


def walk(branch, sha, ancestor_of, pulls_of, parent_of, max_walk=MAX_WALK):
    """Follow first parents from sha until a commit that is an ancestor of develop.

    Every commit on the way must be the merge commit of a merged PR into branch.
    ancestor_of / pulls_of / parent_of are the readers (None on failure), so the
    whole decision can be tested against a fake graph. Returns (status, message).
    """
    path = []
    cur = sha
    for _ in range(max_walk):
        code, msg, arm = judge(branch, cur, ancestor_of(cur), pulls_of(cur))
        if code == UNMEASURED:
            return UNMEASURED, msg
        if code == REFUSED:
            if not path:
                return REFUSED, f"REFUSED: {sha} on {branch} has not landed through develop or a pull request.\n  {msg}"
            return REFUSED, (
                f"REFUSED: {sha} on {branch} is on top of a commit that never landed.\n"
                f"  The chain of merged PRs from {sha} reaches {cur}, which is neither an ancestor of develop nor a merged PR's merge commit into {branch}.\n"
                f"  walked: {' -> '.join(path)}\n  at {cur}: {msg}\n"
                f"  A PR merged onto a branch created at an unlanded commit carries that commit with it."
            )
        if arm == "ancestor":
            how = "an ancestor of develop" if not path else (
                f"the end of a chain of {len(path)} merged PR(s) into {branch} that starts at {cur}, an ancestor of develop"
            )
            return OK, f"landed: {sha} is {how}" if not path else f"landed: {sha} is {how} ({msg})"
        path.append(cur)
        parent = parent_of(cur)
        if not parent:
            return UNMEASURED, (
                f"UNMEASURED: could not read the first parent of {cur}\n"
                "This is a failure of the check, not a finding about the commit."
            )
        cur = parent
    return UNMEASURED, (
        f"UNMEASURED: walked {max_walk} merged PRs from {sha} without reaching an ancestor of develop\n"
        "This is a failure of the check, not a finding about the commit."
    )


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


def read_parent(repo, sha, call=gh):
    """The FIRST parent of sha, or None when it cannot be read or there is none."""
    try:
        out = call([f"repos/{repo}/commits/{sha}", "--jq", ".parents[0].sha // empty"])
    except Unreadable:
        return None
    parent = out.strip()
    return parent if SHA_RE.match(parent) else None


def self_test():
    B = "release/v0.5.0"

    def h(c):
        return c * 40

    def pr(n, merge_sha, base=B, merged=True):
        return {"number": n, "merged_at": "2026-10-26T00:00:00Z" if merged else None,
                "base": {"ref": base}, "merge_commit_sha": merge_sha}

    failures = []

    def want(name, got, status, *needles):
        code, msg = got
        ok = code == status and all(n in msg for n in needles)
        print(("ok   " if ok else "FAIL ") + name + f" -> {code}")
        if not ok:
            failures.append(f"{name}: got {code}: {msg!r}, want {status} containing {needles}")

    # --- judge: one commit, both arms -------------------------------------------
    S = h("a")
    j = lambda *a, **k: judge(B, S, *a, **k)[:2]
    want("ancestor (behind)", j("behind", []), OK, "ancestor of develop")
    want("ancestor (identical)", j("identical", []), OK, "ancestor of develop")
    want("merged PR into this branch", j("diverged", [pr(7, S)]), OK, "merge commit of merged PR #7")
    want("on neither", j("diverged", []), REFUSED, "not an ancestor of develop", "no pull request is associated")
    want("ahead is not an ancestor", j("ahead", []), REFUSED, "compare status ahead")
    want("PR merged into ANOTHER branch", j("diverged", [pr(7, S, base="develop")]), REFUSED, "base develop")
    want("PR not merged", j("diverged", [pr(7, S, merged=False)]), REFUSED, "not merged")
    want("PR merged here, but sha is only its HEAD commit", judge(B, h("b"), "diverged", [pr(7, S)])[:2], REFUSED, "merge commit is not this sha")
    want("ancestor unreadable, pr says no", j(None, []), UNMEASURED, "ancestor (could not compare", "failure of the check")
    want("pr unreadable, ancestor says no", j("diverged", None), UNMEASURED, "pr (could not read")
    want("both unreadable", j(None, None), UNMEASURED, "ancestor", "pr")
    want("ancestor holds, pr unreadable", j("behind", None), OK)
    want("pr holds, ancestor unreadable", j(None, [pr(7, S)]), OK)
    want("only the pr arm ignores an ancestor", j("behind", [], arms=("pr",)), REFUSED, "no pull request")
    want("only the ancestor arm ignores a merged PR", j("diverged", [pr(7, S)], arms=("ancestor",)), REFUSED, "not an ancestor")

    # --- walk: a fake graph. parent[c] is c's first parent --------------------
    D, X, M, M2 = h("d"), h("e"), h("f"), h("1")
    status = {D: "behind", X: "diverged", M: "diverged", M2: "diverged"}
    prs = {D: [], X: [], M: [pr(99, M)], M2: [pr(100, M2)]}
    parent = {M: X, M2: M}

    def w(sha, **over):
        st = dict(status, **over.get("status", {}))
        pl = dict(prs, **over.get("prs", {}))
        pa = dict(parent, **over.get("parent", {}))
        return walk(B, sha, st.get, pl.get, pa.get, **over.get("kw", {}))

    # THE CASE THE FIRST VERSION GOT WRONG. release/x was created at X, which
    # never landed; M is a fix PR merged on top. X alone is refused, and M must be
    # too, because M contains X. Without the walk, M reads "merge commit of merged PR".
    want("X alone (the branch's creation point) is refused", w(X), REFUSED, "has not landed")
    want("X -> M: a merged PR on top of an unlanded base is refused", w(M), REFUSED,
         "on top of a commit that never landed", X, "walked: " + M)
    want("X -> M -> M2: still refused two merges deep", w(M2), REFUSED, "never landed", X)

    # And the cases that must still pass.
    want("D (a develop commit) alone", w(D), OK, "an ancestor of develop")
    want("D -> M", w(M, parent={M: D}), OK, "landed", "chain of 1 merged PR")
    want("D -> M -> M2", w(M2, parent={M: D}), OK, "chain of 2 merged PR")

    # Unreadable links are not refusals.
    want("a first parent that cannot be read", w(M, parent={M: None}), UNMEASURED, "first parent of " + M, "failure of the check")
    want("the parent's ancestor arm unreadable, no PR", w(M, parent={M: X}, status={X: None}), UNMEASURED, "ancestor (could not compare")
    want("the walk is bounded", w(M2, parent={M: D}, kw={"max_walk": 1}), UNMEASURED, "without reaching an ancestor of develop")
    # A cycle (impossible in git, possible from a broken reader) must hit the bound.
    want("a parent that points back at itself", w(M, parent={M: M}, kw={"max_walk": 5}), UNMEASURED, "without reaching")

    # --- the readers, against a fake `gh` -----------------------------------
    def failing(_):
        raise Unreadable("HTTP 502")

    want("reader: a failed compare is None, not 'diverged'",
         (UNMEASURED if read_ancestor("o/r", S, failing) is None else OK, "x"), UNMEASURED)
    want("reader: a failed pulls call is None, not []",
         (UNMEASURED if read_pulls("o/r", S, failing) is None else OK, "x"), UNMEASURED)
    want("reader: non-JSON pulls output is None",
         (UNMEASURED if read_pulls("o/r", S, lambda _: "not json") is None else OK, "x"), UNMEASURED)
    want("reader: a non-list pulls document is None",
         (UNMEASURED if read_pulls("o/r", S, lambda _: '{"message":"x"}') is None else OK, "x"), UNMEASURED)
    pages = json.dumps([[pr(7, S)], [pr(8, S)]])
    flat = read_pulls("o/r", S, lambda _: pages)
    want("reader: pages are flattened", (OK if flat and [p["number"] for p in flat] == [7, 8] else REFUSED, "x"), OK)
    want("reader: an empty pulls list is [] (not None)", (OK if read_pulls("o/r", S, lambda _: "[[]]") == [] else REFUSED, "x"), OK)
    want("reader: a failed parent call is None", (UNMEASURED if read_parent("o/r", S, failing) is None else OK, "x"), UNMEASURED)
    want("reader: a parent that is not a sha is None", (UNMEASURED if read_parent("o/r", S, lambda _: "garbage\n") is None else OK, "x"), UNMEASURED)
    want("reader: a root commit has no parent", (UNMEASURED if read_parent("o/r", S, lambda _: "\n") is None else OK, "x"), UNMEASURED)
    want("reader: a parent sha is returned", (OK if read_parent("o/r", S, lambda _: h("c") + "\n") == h("c") else REFUSED, "x"), OK)

    if failures:
        print("\n".join(failures), file=sys.stderr)
        return 1
    print("check-rc-sha-landed: self-test OK")
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
    if a.arm:
        arms = (a.arm,)
        code, msg, _ = judge(
            a.branch, a.sha,
            read_ancestor(a.repo, a.sha) if "ancestor" in arms else None,
            read_pulls(a.repo, a.sha) if "pr" in arms else None,
            arms,
        )
        msg = ("landed: " + msg) if code == OK else (("REFUSED: " + msg) if code == REFUSED else msg)
    else:
        code, msg = walk(
            a.branch, a.sha,
            lambda c: read_ancestor(a.repo, c),
            lambda c: read_pulls(a.repo, c),
            lambda c: read_parent(a.repo, c),
        )
    print(msg, file=sys.stderr if code else sys.stdout)
    return code


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
