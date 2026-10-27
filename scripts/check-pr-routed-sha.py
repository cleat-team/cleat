#!/usr/bin/env python3
"""A PR's latest ROUTED comment names the PR's current, full, 40-character head sha.

Three routing-hygiene misses in one night (2026-10-09/10) made this checkable rather than
conventional: a stale ROUTED comment left after a push (the convention this file's own author's
PR #3286 hit), a PR routed by direct message with no comment posted at all (#3293, caught by a
peer reading the thread), and a ROUTED comment whose cited sha was mistyped past its first 8
characters and resolves to no commit at all (#3293 again, caught by a peer who checked).

The third is why this compares FULL shas, never prefixes, and why that is the one hard
constraint: #3293's fabricated sha shared its first EIGHT characters with the real head
(`9fa45118...`) and then diverged. A prefix check -- `cited.startswith(prefix_of(head))` or the
reverse -- would have read that stale comment as valid. See CLAUDE.md's whole "Is this result
real?" section and `a-sha-is-quoted-at-many-lengths-so-match-a-prefix.md`: the remedy there is
the same one this script exists to make mechanical -- always the full 40, never a prefix.

What this does NOT check (said out loud, per this repo's own convention for a guard's limits):
it has no opinion about the FIRST miss (no comment posted at all) beyond `--require-routed`,
which only asserts one exists somewhere in the PR's history, not that it was posted promptly, or
that a cross-session message was also sent. Convention still carries that half.

Exit status, and why there are three:

  0  the PR's latest ROUTED comment (if any, or if found when --require-routed is not passed)
     names the current head, verbatim, as a full 40-character sha
  1  a finding: a ROUTED comment exists and names something else -- a different sha, an
     abbreviated one, or text that is not sha-shaped at all
  2  this check could not establish what it measures: the PR could not be read (`gh` failed, no
     network, a bad PR number), or --require-routed was passed and no ROUTED comment exists at
     all. A scan that finds nothing agrees with every tree, correct or not -- see CLAUDE.md,
     "Is this result real?".

The comparison logic (`verdict`) takes plain data -- a list of comment bodies in creation order,
and the head sha -- so --self-test exercises it directly, with no `gh` call and no network.
"""
import json
import re
import subprocess
import sys

FULL_SHA = re.compile(r"\b([0-9a-f]{40})\b")
# Any run of 7+ hex characters that is NOT part of a longer run -- used only to distinguish
# "cited something sha-shaped but short" from "cited no sha at all" for the finding message.
SHA_SHAPED = re.compile(r"\b([0-9a-f]{7,39})\b")


class Unmeasured(Exception):
    pass


def latest_routed(bodies):
    """The last comment (by position -- callers pass them in creation order) whose body's
    FIRST LINE starts with "ROUTED", per this repo's convention that a verdict is detected by
    its leading line only, never by a substring search of the whole body. Returns None if none
    does."""
    found = None
    for body in bodies:
        first_line = body.split("\n", 1)[0]
        if first_line.startswith("ROUTED"):
            found = body
    return found


def verdict(bodies, head_sha, require_routed):
    """Returns (code, message). Pure: no I/O, so --self-test exercises this directly."""
    head_sha = head_sha.strip().lower()
    if not FULL_SHA.fullmatch(head_sha):
        raise Unmeasured(f"the supplied head sha {head_sha!r} is not a full 40-character hex string")

    comment = latest_routed(bodies)
    if comment is None:
        if require_routed:
            raise Unmeasured("no comment whose first line starts with \"ROUTED\" was found on this PR")
        return 0, "OK: no ROUTED comment on this PR (nothing to check; pass --require-routed to demand one)"

    full_matches = FULL_SHA.findall(comment.lower())
    if not full_matches:
        shaped = SHA_SHAPED.findall(comment.lower())
        if shaped:
            return 1, (f"the latest ROUTED comment cites {shaped[0]!r} ({len(shaped[0])} hex characters), "
                        "not a full 40-character sha -- an abbreviated or truncated sha reads as valid under "
                        "a prefix check and is exactly the shape of mistake this script exists to catch")
        return 1, "the latest ROUTED comment names no sha-shaped text at all"

    if len(full_matches) > 1 and len(set(full_matches)) > 1:
        return 1, (f"the latest ROUTED comment cites {len(set(full_matches))} different full shas "
                    f"({', '.join(sorted(set(full_matches)))}) -- ambiguous, needs a human to disambiguate")

    cited = full_matches[0]
    if cited != head_sha:
        return 1, (f"the latest ROUTED comment cites {cited}, but the PR's current head is {head_sha} -- "
                    "stale (pushed since) or mistyped; compared as full strings, never as a prefix")

    return 0, f"OK: the latest ROUTED comment names the current head {head_sha}"


def gh_pr_comments(repo, pr):
    try:
        out = subprocess.run(
            ["gh", "pr", "view", str(pr), "--repo", repo, "--json", "comments,headRefOid"],
            capture_output=True, check=True, text=True,
        ).stdout
    except FileNotFoundError:
        raise Unmeasured("gh is not installed")
    except subprocess.CalledProcessError as e:
        raise Unmeasured(f"gh pr view {pr} --repo {repo} failed: {e.stderr.strip()}")
    try:
        data = json.loads(out)
    except json.JSONDecodeError as e:
        raise Unmeasured(f"gh pr view returned unparseable JSON: {e}")
    bodies = [c["body"] for c in data.get("comments", [])]
    head = data.get("headRefOid", "")
    if not head:
        raise Unmeasured(f"gh pr view {pr} returned no headRefOid")
    return bodies, head


def main(repo, pr, require_routed):
    bodies, head = gh_pr_comments(repo, pr)
    code, message = verdict(bodies, head, require_routed)
    print(message)
    return code


def self_test():
    failures = []

    def expect(name, bodies, head, require_routed, want_rc, want_in_message):
        try:
            rc, msg = verdict(bodies, head, require_routed)
        except Unmeasured as e:
            rc, msg = 2, f"UNMEASURED: {e}"
        ok = rc == want_rc and want_in_message in msg
        print(f"  {'ok  ' if ok else 'FAIL'} {name}: rc={rc} (want {want_rc}), msg has {want_in_message!r}: {want_in_message in msg}")
        if not ok:
            failures.append(name)
            print(f"    full message: {msg}")

    real_head = "9fa45118370d0d916672e2588b9836fc90eb5892"
    stale_head = "9fa45118370d0d916672e2588b9836fc90eb5893"  # last hex digit differs
    fabricated = "9fa451189e0edd7c1f2a98c07bb9fcd9be6d7bdc"  # #3293's real incident: shares only the first 8 chars

    print("check-pr-routed-sha --self-test")

    expect("no ROUTED comment, not required: pass",
           ["NOT A VERDICT -- holding for CI", "just a regular comment"], real_head, False,
           0, "no ROUTED comment")

    expect("no ROUTED comment, required: UNMEASURED",
           ["NOT A VERDICT -- holding for CI"], real_head, True,
           2, "no comment whose first line")

    expect("ROUTED comment names the current head exactly: pass",
           [f"ROUTED — sent to cleat-review for a verdict on {real_head}. Scope: ..."], real_head, False,
           0, "OK: the latest ROUTED comment names the current head")

    expect("ROUTED comment is stale (names the previous head): finding",
           [f"ROUTED — sent to cleat-review for a verdict on {stale_head}."], real_head, False,
           1, "stale (pushed since) or mistyped")

    expect("the #3293 incident: a sha sharing only its first 8 characters with the real head",
           [f"ROUTED — sent to cleat-review for a verdict on {fabricated}."], real_head, False,
           1, f"but the PR's current head is {real_head}")

    expect("a prefix would wrongly pass this if it were used -- confirm it is NOT used",
           [f"ROUTED — sent to cleat-review for a verdict on {real_head[:8]}."], real_head, False,
           1, "not a full 40-character sha")

    expect("only the LATEST comment counts, even when an earlier ROUTED named something else",
           [f"ROUTED — sent to cleat-review for a verdict on {stale_head}.",
            "NOT A VERDICT -- cleat-review holding for CI",
            f"ROUTED — sent to cleat-review for a verdict on {real_head}. Corrected head."],
           real_head, False,
           0, "OK: the latest ROUTED comment names the current head")

    expect("ROUTED only as a substring mid-body does not count -- must be the first line",
           [f"Something else first.\nROUTED — sent to cleat-review for a verdict on {real_head}."],
           real_head, False,
           0, "no ROUTED comment")

    expect("a NOT A VERDICT comment that happens to quote a full sha is not itself a ROUTED comment",
           [f"NOT A VERDICT — cleat-review on #3293 at {real_head}, still checking."],
           real_head, False,
           0, "no ROUTED comment")

    expect("two different full shas cited in one ROUTED comment: ambiguous finding",
           [f"ROUTED — verdict on {real_head}, superseding {stale_head}."], real_head, False,
           1, "different full shas")

    expect("no sha-shaped text at all in the ROUTED comment: finding",
           ["ROUTED — sent to cleat-review for a verdict on the latest push."], real_head, False,
           1, "no sha-shaped text at all")

    try:
        verdict(["ROUTED — x"], "not-a-sha", False)
        ok = False
    except Unmeasured as e:
        ok = "not a full 40-character hex string" in str(e)
    print(f"  {'ok  ' if ok else 'FAIL'} a non-sha-shaped head sha argument is UNMEASURED, not a verdict")
    if not ok:
        failures.append("non-sha head argument")

    if failures:
        print(f"SELF-TEST FAILED: {failures}")
        return 1
    print("self-test passed")
    return 0


if __name__ == "__main__":
    if len(sys.argv) > 1 and sys.argv[1] == "--self-test":
        sys.exit(self_test())
    args = sys.argv[1:]
    require_routed = "--require-routed" in args
    args = [a for a in args if a != "--require-routed"]
    if len(args) != 2:
        print("usage: check-pr-routed-sha.py <owner/repo> <pr-number> [--require-routed]", file=sys.stderr)
        print("       check-pr-routed-sha.py --self-test", file=sys.stderr)
        sys.exit(2)
    try:
        sys.exit(main(args[0], args[1], require_routed))
    except Unmeasured as e:
        print(f"UNMEASURED: {e}", file=sys.stderr)
        sys.exit(2)
