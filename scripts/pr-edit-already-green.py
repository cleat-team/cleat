#!/usr/bin/env python3
"""Was this PR head already tested green by this workflow?

`edited` fires for a body or title change as well as for a base retarget
(cleat#2483), and cleat re-runs every required context on it. A body edit
cannot change what any job in the heavy workflows measures -- a grep of the
tree finds no job that reads `pull_request.title` or `.body` (cleat#3958).
So an `edited` run whose head has *already passed* this workflow has nothing
to do, and skipping it saves the whole suite.

**Skipping is only safe when the head is already green, and that is the whole
subtlety.** A skipped job reports as Success and does not block a required
check (GitHub docs, "Using conditions to control job execution"), so skipping
unconditionally would launder a RED head into a green one: edit the body of a
PR whose tests are failing and the red check-runs are replaced by green
skips. That is worse than the cost this exists to remove.

So the rule is: skip only if every job's latest **non-skipped** conclusion on
this head is `success`. The "non-skipped" qualifier is load-bearing and is
the second way this can go wrong: take the latest conclusion of any kind and
the sequence

    run 1: failure        (the head is red)
    run 2: skipped        (a body edit skipped it, believing ... something)

reads as skipped and passes. Taking the latest conclusion that is NOT
skipped preserves the failure.

Exit status, per this repo's convention for a check that reads something it
does not control:

  0  a verdict was reached; `skip=true` or `skip=false` is printed
  1  self-test failure (only with --self-test)
  2  UNMEASURED -- the API could not be read; the caller must treat this as
     skip=false, because a gate that cannot look must not skip

`verdict()` is pure so --self-test exercises it with no network at all.
"""
import argparse
import json
import subprocess
import sys
import urllib.parse

SUCCESS = "success"
SKIPPED = "skipped"


class Unmeasured(Exception):
    pass


def verdict(runs, want):
    """Decide whether the head is already green.

    runs: list of {"id", "status", "conclusion", "created_at", "jobs": [{"name","conclusion"}]}
          in any order; the current run must already be excluded by the caller.
    want: the set of job names this workflow defines. Used only for reporting:
          a job that has never run is not by itself a reason to run everything.

    Returns (skip, reason).
    """
    if not runs:
        return False, "no completed run of this workflow on this head (nothing to trust)"

    runs = sorted(runs, key=lambda r: (r.get("created_at") or "", r.get("id") or 0))

    for r in runs:
        if r.get("status") != "completed":
            return False, f"run {r.get('id')} is {r.get('status')}, not completed"

    # Latest NON-skipped conclusion per job name.
    latest = {}
    any_success = False
    for r in runs:
        for j in r.get("jobs") or []:
            c = (j.get("conclusion") or "").lower()
            name = j.get("name")
            if c == SKIPPED:
                continue
            latest[name] = c
            if c == SUCCESS:
                any_success = True

    if not latest:
        return False, "no job on this head has ever reported a conclusion other than skipped"

    # Anything that reported and was not `success` blocks the skip: failure,
    # cancelled, timed_out, action_required, stale, startup_failure, and
    # `neutral`. Written as `!= SUCCESS` rather than as a set of known-bad
    # conclusions on purpose -- a conclusion the set has not heard of would be
    # let through as benign, and this predicate's safe direction is to run the
    # suite. SKIPPED never reaches here; it is dropped above, so a skipped job
    # cannot turn a red head green.
    bad = sorted(n for n, c in latest.items() if c != SUCCESS)
    if bad:
        shown = ", ".join(f"{n}={latest[n]}" for n in bad[:5])
        more = "" if len(bad) <= 5 else f" (+{len(bad) - 5} more)"
        return False, f"{len(bad)} job(s) did not pass on this head: {shown}{more}"

    if not any_success:
        return False, "no job on this head has a success"

    missing = sorted(set(want) - set(latest))
    note = f"; {len(missing)} job(s) have never run" if missing else ""
    return True, f"every job that ran on this head passed ({len(latest)} job(s)){note}"


# --------------------------------------------------------------------------- io

def gh_json(args):
    """Run gh and parse the JSON it printed.

    `gh api --paginate` prints one JSON document per page, concatenated, so the
    output is a *stream* of values rather than a single one. raw_decode is run
    in a loop; a single-document read would fail with "Extra data" as soon as
    the first response needed a second page -- and the runs endpoint takes one
    page per 100 runs, which a busy PR head can exceed.
    """
    try:
        out = subprocess.run(["gh"] + args, capture_output=True, check=True, text=True)
    except FileNotFoundError:
        raise Unmeasured("gh is not installed")
    except subprocess.CalledProcessError as e:
        raise Unmeasured(f"gh {' '.join(args)} failed: {e.stderr.strip()[:400]}")
    dec = json.JSONDecoder()
    vals, i, s = [], 0, out.stdout.strip()
    while i < len(s):
        while i < len(s) and s[i] in " \t\r\n":
            i += 1
        if i >= len(s):
            break
        try:
            v, i = dec.raw_decode(s, i)
        except json.JSONDecodeError as e:
            raise Unmeasured(f"gh returned unparseable JSON: {e}")
        vals.append(v)
    return vals


def runs_query(repo, workflow, sha, branch=""):
    """The runs endpoint for one workflow on one head, optionally on one branch.

    `head_sha` alone is not enough: two pull requests can point at the same
    commit (a copy, a retarget, a rebase onto an identical tree), and a newer
    GREEN run of that commit on the other PR would then mask a RED one here --
    the verdict would be skip=true on a head this workflow failed on. The
    branch parameter narrows it to this PR's head ref.

    Not keyed on `pull_requests[]`: it is empty once a PR merges, so a filter
    built on it silently matches nothing at exactly the moment a run history
    matters.
    """
    q = f"repos/{repo}/actions/workflows/{workflow}/runs?head_sha={sha}" \
        f"&event=pull_request&per_page=100"
    if branch:
        q += "&branch=" + urllib.parse.quote(branch, safe="")
    return q


def collect(repo, workflow, sha, exclude_run, branch=""):
    pages = gh_json(["api", "--paginate", runs_query(repo, workflow, sha, branch)])
    wr = []
    for page in pages:
        wr.extend(page.get("workflow_runs") or [])
    keep = []
    for r in wr:
        if str(r.get("id")) == str(exclude_run):
            continue
        pages = gh_json(["api", "--paginate",
                         # filter=latest, not all: a run that was re-run has a
                         # cancelled first attempt, and those cancelled jobs
                         # would poison the verdict for a head that passed on
                         # the attempt that replaced them.
                         f"repos/{repo}/actions/runs/{r['id']}/jobs?per_page=100&filter=latest"])
        js = []
        for page in pages:
            js.extend(page.get("jobs") or [])
        keep.append({"id": r.get("id"), "status": r.get("status"),
                     "conclusion": r.get("conclusion"), "created_at": r.get("created_at"),
                     "jobs": [{"name": j.get("name"), "conclusion": j.get("conclusion")}
                              for j in js]})
    return keep


# --------------------------------------------------------------------------- test

def self_test():
    def run(name, jobs, status="completed"):
        return {"id": hash(name) % 10000, "status": status, "conclusion": "?",
                "created_at": name, "jobs": [{"name": n, "conclusion": c} for n, c in jobs]}

    cases = [
        ("one green run", [run("a", [("t", "success"), ("u", "success")])], True),
        ("a failure", [run("a", [("t", "success"), ("u", "failure")])], False),
        # the laundering case: failure, then a skipped run on top of it
        ("failure then skipped run",
         [run("a", [("t", "success"), ("u", "failure")]),
          run("b", [("t", "skipped"), ("u", "skipped")])], False),
        ("green then skipped run",
         [run("a", [("t", "success"), ("u", "success")]),
          run("b", [("t", "skipped"), ("u", "skipped")])], True),
        ("no runs", [], False),
        ("an in-progress run",
         [run("a", [("t", "success")]), run("b", [("t", "success")], status="in_progress")], False),
        ("all skipped", [run("a", [("t", "skipped")])], False),
        ("cancelled", [run("a", [("t", "success"), ("u", "cancelled")])], False),
        ("a job that only ran later",
         [run("a", [("t", "success")]), run("b", [("t", "success"), ("u", "success")])], True),
        # the subtle one: success, then failure, then skipped -- still red
        ("failure under a skipped run",
         [run("a", [("t", "success")]),
          run("b", [("t", "failure")]),
          run("c", [("t", "skipped")])], False),
    ]
    failures = 0
    for name, runs, want in cases:
        got, why = verdict(runs, set())
        mark = "ok " if got == want else "FAIL"
        if got != want:
            failures += 1
        print(f"  {mark} {name}: skip={got} want={want} -- {why}")
    print(f"{len(cases) - failures}/{len(cases)} verdict cases passed")

    # `runs_query` is the only thing standing between this verdict and another
    # PR's runs on the same commit, so it is asserted rather than assumed.
    q_cases = [
        ("no branch filter when none is given",
         runs_query("o/r", "ci.yml", "a" * 40),
         "head_sha=" + "a" * 40, "&branch=", "&event=pull_request"),
        ("the branch is filtered when given",
         runs_query("o/r", "ci.yml", "a" * 40, "fix/x"),
         "&branch=fix%2Fx", None, None),
        ("a slash and a plus are encoded",
         runs_query("o/r", "ci.yml", "a" * 40, "a/b+c"),
         "&branch=a%2Fb%2Bc", None, None),
    ]
    for name, got, want_in, want_out, want_in2 in q_cases:
        bad = []
        if want_in and want_in not in got:
            bad.append(f"missing {want_in}")
        if want_out and want_out in got:
            bad.append(f"unexpected {want_out}")
        if want_in2 and want_in2 not in got:
            bad.append(f"missing {want_in2}")
        if bad:
            failures += 1
            print(f"  FAIL {name}: {'; '.join(bad)} -- {got}")
        else:
            print(f"  ok   {name}")
    total = len(cases) + len(q_cases)
    print(f"{total - failures}/{total} self-test cases passed")
    return 1 if failures else 0


def main(argv):
    ap = argparse.ArgumentParser()
    ap.add_argument("--repo")
    ap.add_argument("--workflow", help="workflow file name or id, e.g. ci.yml")
    ap.add_argument("--sha")
    ap.add_argument("--exclude-run", default="", help="the current run id")
    ap.add_argument("--branch", default="",
                    help="the PR head ref, to keep another PR on the same commit "
                         "from contributing runs to this verdict")
    ap.add_argument("--self-test", action="store_true")
    a = ap.parse_args(argv)
    if a.self_test:
        return self_test()
    if not (a.repo and a.workflow and a.sha):
        print("usage: --repo OWNER/REPO --workflow FILE --sha SHA [--exclude-run ID] "
              "[--branch REF]", file=sys.stderr)
        return 2
    try:
        runs = collect(a.repo, a.workflow, a.sha, a.exclude_run, a.branch)
        skip, why = verdict(runs, set())
    except Unmeasured as e:
        print(f"UNMEASURED: {e}", file=sys.stderr)
        print("this is a failure of the check, not a finding about the tree", file=sys.stderr)
        return 2
    # stdout carries exactly the line the workflow appends to $GITHUB_OUTPUT;
    # the reason goes to stderr so it can never be mistaken for the verdict.
    print(f"skip={'true' if skip else 'false'}")
    print(why, file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
