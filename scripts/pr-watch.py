#!/usr/bin/env python3
"""Say, per pull request, whether it is WAITING or NEEDS-AUTHOR -- including
the case the PR page cannot show: dropped from the merge queue.

WHY THIS EXISTS. A PR removed from the merge queue for failed checks goes on
reading `mergeStateStatus: CLEAN`. The failure is on the queue's BATCH commit,
not the PR's head, so the PR's own checks stay green too. Every watcher that
polled mergeStateStatus -- which was every stream's watcher -- kept reporting
"waiting" on a PR that nothing was going to merge. Measured 2026-09-23, from
REMOVED_FROM_MERGE_QUEUE_EVENT on every PR updated that day:

    8 removals that were not merges: 3 `failed_checks` (#2016, #2037, #2057)
    and 5 `manual` (#2003, #2032, #2037, #2040, #2056)

    gh api graphql -f query='{repository(owner:"cleat-team",name:"cleat"){
      pullRequest(number:N){timelineItems(first:20,itemTypes:[REMOVED_FROM_MERGE_QUEUE_EVENT]){
        nodes{... on RemovedFromMergeQueueEvent{reason createdAt}}}}}}'

#2057's drop (15:12:25Z) was found by the coordinator's 15-minute tick, not by
its author's watcher. The cause was a plugins-job failure on batch a543752ead
(cleat#2063), visible only in the merge_group run's jobs.

WHAT IT CHECKS, in the order WORKSTREAM.md R9 states them:
  - in the queue: the ENTRY's state, allowlisted (QUEUED, AWAITING_CHECKS,
    MERGEABLE). Anything else -- UNMERGEABLE, LOCKED, a value added later --
    is NEEDS-AUTHOR. The allowlist fails closed; R9 says why.
  - not in the queue, and the last queue event is a removal whose reason is
    not `merged`, and no commit has been pushed since: DROPPED. The failing
    jobs are read from the batch's merge_group runs (head branch
    gh-readonly-queue/develop/pr-<N>-<sha>), never from the PR head.
  - otherwise the PR's own checks: anything red, a conflict, or a cancelled
    check run is NEEDS-AUTHOR -- told apart as R9's twin (needs a new SHA)
    or a lone cancellation (a re-run clears it); anything still running is
    WAITING.
  - CLEAN, all green, and not queued is NEEDS-AUTHOR too: nothing merges it
    until someone enqueues it -- unless auto-merge is set, which enqueues it.

Exit status: 0 when every PR is WAITING or MERGED; 1 when any NEEDS-AUTHOR
or is CLOSED; 2 when something could not be measured. With --watch, it polls
until one of those becomes true of every PR -- it returns the moment any PR
needs its author, rather than waiting for the rest.

Usage:
    scripts/pr-watch.py 2057 2061            # one report
    scripts/pr-watch.py                      # every open non-dependabot PR
    scripts/pr-watch.py --watch 120 2057     # poll every 120s until settled
    scripts/pr-watch.py --dump 2057 > f.json # the measured state, for a fixture
    scripts/pr-watch.py --fixture f.json     # classify a saved state
"""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
import time

REPO = "cleat-team/cleat"
OWNER, NAME = REPO.split("/")
QUEUE_BASE = "develop"

# R9's allowlist. A state not named here is not healthy.
HEALTHY_ENTRY_STATES = {"QUEUED", "AWAITING_CHECKS", "MERGEABLE"}
RED = {"FAILURE", "TIMED_OUT", "STARTUP_FAILURE", "ACTION_REQUIRED", "ERROR"}

PR_QUERY = """
query($owner:String!,$name:String!,$n:Int!){repository(owner:$owner,name:$name){
  pullRequest(number:$n){
    number state isDraft mergeStateStatus headRefOid
    autoMergeRequest{enabledAt}
    author{login}
    mergeQueueEntry{state position}
    timelineItems(last:1,itemTypes:[ADDED_TO_MERGE_QUEUE_EVENT,REMOVED_FROM_MERGE_QUEUE_EVENT]){
      nodes{__typename
        ... on RemovedFromMergeQueueEvent{reason createdAt}
        ... on AddedToMergeQueueEvent{createdAt}}}
    commits(last:1){nodes{commit{committedDate
      statusCheckRollup{contexts(first:100){nodes{__typename
        ... on CheckRun{name status conclusion}
        ... on StatusContext{context state}}}}}}}
  }}}
"""


class Unmeasured(Exception):
    """A call failed or returned something this script does not model."""


def gh_json(args: list[str]) -> object:
    proc = subprocess.run(["gh", *args], capture_output=True, text=True, check=False)
    if proc.returncode != 0:
        raise Unmeasured(f"gh {' '.join(args[:3])}...: {proc.stderr.strip()}")
    try:
        return json.loads(proc.stdout)
    except json.JSONDecodeError as e:
        raise Unmeasured(f"gh {' '.join(args[:3])}...: not JSON: {e}") from e


def open_prs() -> list[int]:
    prs = gh_json(["pr", "list", "-R", REPO, "--state", "open", "--limit", "200",
                   "--json", "number,author"])
    return [p["number"] for p in prs if "dependabot" not in p["author"]["login"]]


def cancelled_on(sha: str) -> dict[str, list[str]]:
    """Check names whose LATEST run on sha was cancelled, split in two.

    The endpoint's default filter is `latest`: a re-run replaces the cancelled
    run in the listing (measured on #2073, 2026-09-23: one cancelled Tier 1
    Gate, gone from the default listing once re-run, still in `filter=all`).
    So a name that is ONLY cancelled here is a lone cancellation -- a runner
    lost mid-job -- and a re-run clears it. A name that is cancelled AND has a
    completed run of the same name here is R9's twin (cleat#1688): two run
    sets, and it needs a new SHA.
    """
    # --paginate: R9 records per_page=100 alone truncating 121 check runs to
    # 100, which cost 17 of the cancelled ones. headRefOid is the full SHA.
    pages = gh_json(["api", "--paginate", "--slurp",
                     f"repos/{REPO}/commits/{sha}/check-runs?per_page=100"])
    runs = [r for page in pages for r in page["check_runs"]]
    cancelled = {r["name"] for r in runs if r.get("conclusion") == "cancelled"}
    finished = {r["name"] for r in runs
                if r.get("conclusion") not in (None, "cancelled")}
    return {"twin": sorted(cancelled & finished), "lone": sorted(cancelled - finished)}


def batch_failures(n: int) -> dict:
    """The failed jobs of PR n's most recent merge-queue batch."""
    prefix = f"gh-readonly-queue/{QUEUE_BASE}/pr-{n}-"
    runs = gh_json(["api", f"repos/{REPO}/actions/runs?event=merge_group&per_page=100"])
    mine = [r for r in runs["workflow_runs"] if r["head_branch"].startswith(prefix)]
    if not mine:
        return {"batch": None, "failed": []}
    sha = max(mine, key=lambda r: r["created_at"])["head_sha"]
    failed = []
    for r in (r for r in mine if r["head_sha"] == sha):
        jobs = gh_json(["api", "--paginate", "--slurp",
                        f"repos/{REPO}/actions/runs/{r['id']}/jobs?per_page=100"])
        failed += [{"job": j["name"], "id": j["id"], "run": r["id"]}
                   for page in jobs for j in page["jobs"]
                   if j.get("conclusion") in ("failure", "timed_out")]
    return {"batch": sha, "failed": failed}


def measure(n: int) -> dict:
    data = gh_json(["api", "graphql", "-f", f"query={PR_QUERY}",
                    "-F", f"owner={OWNER}", "-F", f"name={NAME}", "-F", f"n={n}"])
    pr = data["data"]["repository"]["pullRequest"]
    if pr is None:
        raise Unmeasured(f"#{n}: no such pull request")
    state = {"pr": pr, "cancelled": {"twin": [], "lone": []}, "batch": None}
    if pr["state"] == "OPEN":
        state["cancelled"] = cancelled_on(pr["headRefOid"])
        last = pr["timelineItems"]["nodes"]
        if (pr["mergeQueueEntry"] is None and last
                and last[0]["__typename"] == "RemovedFromMergeQueueEvent"
                and last[0]["reason"] != "merged"):
            state["batch"] = batch_failures(n)
    return state


def classify(state: dict) -> tuple[str, str]:
    """(verdict, detail). Verdict is MERGED, CLOSED, WAITING or NEEDS-AUTHOR."""
    pr = state["pr"]
    if pr["state"] == "MERGED":
        return "MERGED", ""
    if pr["state"] == "CLOSED":
        return "CLOSED", "closed without merging"
    if pr["isDraft"]:
        return "NEEDS-AUTHOR", "draft"

    entry = pr["mergeQueueEntry"]
    if entry is not None:
        if entry["state"] in HEALTHY_ENTRY_STATES:
            return "WAITING", f"merge queue {entry['state']} (position {entry['position']})"
        return "NEEDS-AUTHOR", (f"merge queue entry is {entry['state']} while the PR "
                                f"reads {pr['mergeStateStatus']}")

    commit = pr["commits"]["nodes"][0]["commit"]
    last = pr["timelineItems"]["nodes"]
    if last and last[0]["__typename"] == "RemovedFromMergeQueueEvent" \
            and last[0]["reason"] != "merged" \
            and commit["committedDate"] <= last[0]["createdAt"]:
        detail = f"DROPPED from the merge queue at {last[0]['createdAt']} ({last[0]['reason']})"
        b = state.get("batch") or {}
        if b.get("failed"):
            jobs = "; ".join(f"{f['job']} (run {f['run']}, job {f['id']})" for f in b["failed"])
            detail += f"; batch {b['batch'][:10]} failed: {jobs}"
        elif b.get("batch"):
            detail += f"; batch {b['batch'][:10]} has no failed job (yet) -- read its runs"
        return "NEEDS-AUTHOR", detail + ". The PR page still reads " + pr["mergeStateStatus"]

    contexts = (commit.get("statusCheckRollup") or {}).get("contexts", {}).get("nodes", [])
    red, running = [], 0
    for c in contexts:
        if c["__typename"] == "CheckRun":
            if c["status"] != "COMPLETED":
                running += 1
            elif c["conclusion"] in RED:
                red.append(c["name"])
        elif c["state"] in RED:
            red.append(c["context"])
        elif c["state"] in ("PENDING", "EXPECTED"):
            running += 1
    if red:
        return "NEEDS-AUTHOR", "failing: " + ", ".join(sorted(set(red)))
    if pr["mergeStateStatus"] == "DIRTY":
        return "NEEDS-AUTHOR", "merge conflict"
    cancelled = state["cancelled"]
    if cancelled["twin"]:
        return "NEEDS-AUTHOR", ("cancelled TWIN check run(s) on the head SHA -- push a new SHA, "
                                "a re-run does not clear it (R9): " + ", ".join(cancelled["twin"]))
    if cancelled["lone"]:
        return "NEEDS-AUTHOR", ("cancelled check run(s), the latest of their name -- re-run: "
                                "gh run rerun <run> --failed; " + ", ".join(cancelled["lone"]))
    if running:
        return "WAITING", f"{running} check(s) running, not yet queued"
    if pr["mergeStateStatus"] == "CLEAN":
        # Auto-merge enqueues a CLEAN PR on its own, a few seconds after the last
        # required check passes. Sampled inside that window the PR reads exactly
        # like a forgotten one: #2072 did, at 16:21:15Z on 2026-09-23, and its
        # timeline shows AddedToMergeQueueEvent at that same second.
        if pr.get("autoMergeRequest"):
            return "WAITING", "CLEAN with auto-merge set; GitHub is enqueueing it"
        return "NEEDS-AUTHOR", f"CLEAN but not in the merge queue -- enqueue: gh pr merge {pr['number']}"
    return "NEEDS-AUTHOR", f"{pr['mergeStateStatus']} with nothing red or running"


def report(states: dict[int, dict | Unmeasured]) -> int:
    worst = 0
    for n, s in states.items():
        if isinstance(s, Unmeasured):
            print(f"#{n}  UNMEASURED  {s}")
            worst = max(worst, 2)
            continue
        verdict, detail = classify(s)
        print(f"#{n}  {verdict}  {detail}".rstrip())
        if verdict in ("NEEDS-AUTHOR", "CLOSED"):
            worst = max(worst, 1)
    return worst


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("prs", nargs="*", type=int)
    ap.add_argument("--watch", type=int, metavar="SECONDS")
    ap.add_argument("--dump", action="store_true", help="print the measured state as JSON")
    ap.add_argument("--fixture", metavar="FILE", help="classify a state saved by --dump")
    args = ap.parse_args()

    if args.fixture:
        with open(args.fixture) as f:
            saved = json.load(f)
        return report({int(k): v for k, v in saved.items()})

    prs = args.prs or open_prs()
    if not prs:
        # A zero-denominator run is not a pass.
        print("UNMEASURED: no pull requests to check")
        return 2

    while True:
        states: dict[int, dict | Unmeasured] = {}
        for n in prs:
            try:
                states[n] = measure(n)
            except Unmeasured as e:
                states[n] = e
        if args.dump:
            json.dump({n: s for n, s in states.items() if not isinstance(s, Unmeasured)},
                      sys.stdout, indent=1)
            print()
            return 2 if any(isinstance(s, Unmeasured) for s in states.values()) else 0
        if args.watch:
            print(time.strftime("-- %H:%M:%SZ", time.gmtime()))
        rc = report(states)
        settled = all(not isinstance(s, Unmeasured) and classify(s)[0] == "MERGED"
                      for s in states.values())
        if not args.watch or rc != 0 or settled:
            return rc
        time.sleep(args.watch)


if __name__ == "__main__":
    sys.exit(main())
