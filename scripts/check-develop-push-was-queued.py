#!/usr/bin/env python3
"""Assert that a develop push was gated by the merge queue (cleat#3958).

WHY THIS EXISTS. The develop push no longer runs the jobs the merge group
already ran on the same commit: `ci.yml`'s heavy jobs, and all of Multi-DB CI,
Cross-Language E2E, Tier 2 Gate and Plugin Harness Tests, are now skipped on a
push to develop. That is only safe because the queue's merge commit IS the
develop commit, so those workflows ran on exactly this tree before it landed.

Nothing ENFORCES that, and the failure is silent. A direct push by an admin, or
a queue regroup that cancels a group run, would leave a commit whose heavy jobs
ran nowhere at all -- and the develop push run would still conclude `success`,
because the jobs are absent rather than failing. This script converts that
silence into a red Tier 1 Gate run, which the existing
`tier1-push-failure-notifier` already turns into a tracking issue.

KEYED ON THE WORKFLOW PATH, not on the run's `name`. Measured 2026-11-16 over
the 24h census: a merge_group run's `name` is the queue ref --
`gh-readonly-queue/develop/pr-3942-f0e717ac… [checks_requested]` -- because
each of these workflows sets `run-name:`, whose value the API returns in that
field (they DO have top-level `name:` keys; that is not the reason). A guard
keyed on the display name finds nothing and would report every develop push as
ungated; keyed on the path it matches 125 of 125 known-good (25 commits x 5).

A RUN STILL IN PROGRESS IS NOT A FINDING (cleat-review on #3961). The queue
merges once the 35 REQUIRED contexts pass, and the crash shards are not
required, so `ci.yml`'s merge_group run was still running when the develop push
fired on 6 of the 25 pushes in that census, by up to 516s (cleat-review
measured 828s over 40 pushes). Judging that state as "ungated" would have
paged on every one of them. So this script reports THREE states and lets the
caller poll: 0 all five passed; 1 a finding that waiting cannot fix (a
workflow is absent, or completed with a non-success conclusion); 3 still
running. The caller -- tier1-gate.yml's `develop-push-was-queued` job -- polls
on 3 and converts a bound expiry into UNMEASURED (2), which is a different
statement from "ungated".

WHAT IT DOES NOT DO. It does not read check runs. A check run's presence on a
commit can come from a re-run, from a different attempt, or from a workflow
that only ran some of its jobs; this asks the narrower, answerable question
"did a merge_group RUN of this workflow complete successfully on this commit".
"""

import argparse
import json
import sys

# The workflows whose coverage moved off the develop push (cleat#3958). Each is
# required to have a successful merge_group run on the push's own commit.
DEFAULT_WORKFLOWS = [
    ".github/workflows/ci.yml",
    ".github/workflows/multi-db-ci.yml",
    ".github/workflows/e2e-cross-language.yml",
    ".github/workflows/tier2-gate.yml",
    ".github/workflows/plugin-harness-ci.yml",
]


def load_runs(path):
    """Return the run list, or raise ValueError with a reason we can report."""
    try:
        with open(path, "r", encoding="utf-8") as fh:
            text = fh.read()
    except OSError as exc:
        raise ValueError(f"cannot read {path}: {exc}") from exc
    if not text.strip():
        raise ValueError(f"{path} is empty")
    try:
        data = json.loads(text)
    except json.JSONDecodeError as exc:
        raise ValueError(f"{path} is not JSON: {exc}") from exc
    # Accept either the API's {"workflow_runs": [...]}, a bare list, or NDJSON
    # from `gh api --paginate --jq '.workflow_runs[]'`.
    if isinstance(data, dict):
        runs = data.get("workflow_runs")
        if runs is None:
            raise ValueError(f"{path} has no workflow_runs key")
        return runs
    if isinstance(data, list):
        runs = []
        for i, item in enumerate(data):
            if isinstance(item, dict) and "workflow_runs" in item:
                runs.extend(item["workflow_runs"])
            else:
                runs.append(item)
        return runs
    raise ValueError(f"{path} holds {type(data).__name__}, want an object or array")


# A run in one of these states has not finished; its conclusion is null.
RUNNING = {"queued", "in_progress", "waiting", "pending", "requested"}


def judge(runs, sha, workflows):
    """Classify each workflow.

    Returns (findings, running, saw_any):
      findings -- [(workflow, why)] that waiting cannot fix (absent, or a
                  completed run whose conclusion is not `success`);
      running  -- [workflow] whose newest run exists but has not completed;
      saw_any  -- whether any run for this commit exists at all.
    """
    mine = [r for r in runs if r.get("head_sha") == sha]
    findings, running = [], []
    for wf in workflows:
        candidates = [r for r in mine if r.get("path") == wf]
        if not candidates:
            findings.append((wf, "no merge_group run on this commit"))
            continue
        # THE NEWEST ATTEMPT DECIDES (cleat#3962). Same workflow, same commit,
        # more than one run record happens whenever the run was re-triggered: a
        # cancelled run followed by a fresh one. Judging the SET made a newer
        # in-progress run read as "ungated" because an older cancelled one was
        # in it, and -- the mirror case -- let an older SUCCESS mask a newer
        # failure. So order first, then judge one record.
        newest = max(candidates, key=_attempt_order)
        conclusion = str(newest.get("conclusion"))
        if conclusion == "success":
            continue
        if str(newest.get("status") or "").lower() in RUNNING or newest.get("conclusion") is None:
            running.append(wf)
        elif conclusion in ("None", "null", ""):
            findings.append((wf, "merge_group run has no usable status"))
        else:
            findings.append((wf, "newest merge_group run concluded " + conclusion))
    return findings, running, bool(mine)


def _attempt_order(run):
    """Sort key for "which of these is the current attempt".

    created_at first -- a re-triggered run is a new record with a later
    timestamp -- then run_attempt, then a monotonic tiebreak on the id so the
    order is total even for fixtures that carry none of these fields.
    """
    created = str(run.get("created_at") or "")
    try:
        attempt = int(run.get("run_attempt") or 0)
    except (TypeError, ValueError):
        attempt = 0
    try:
        rid = int(run.get("id") or 0)
    except (TypeError, ValueError):
        rid = 0
    return (created, attempt, rid)


def pull_request_only(cond: str) -> bool:
    """True when `cond` provably cannot be true on a push.

    `github.event_name == 'pull_request'` ANDed with anything, and no `||`
    anywhere. A substring test would also exempt
    `github.event_name == 'pull_request' || github.ref == 'refs/heads/develop'`,
    which runs on a develop push -- a guard hole rather than a classification.
    """
    c = " ".join(str(cond).split())
    if not c or "||" in c:
        return False
    return any(part.strip() == "github.event_name == 'pull_request'"
               for part in c.split("&&"))


def check_cut_list(root):
    """The cut and the canary must list the same workflows (cleat#3958).

    A canary over a list that has drifted from the cut is worse than no
    canary: it reports green while guarding the wrong set. So the tree is
    checked directly -- every workflow whose heavy jobs left the develop push
    must be in DEFAULT_WORKFLOWS, and nothing in DEFAULT_WORKFLOWS may still
    trigger on a develop push (except ci.yml, whose cut is job-level and is
    checked job by job).
    """
    import os
    try:
        import yaml
    except ImportError:
        print("UNMEASURED: pyyaml is not installed; this is a failure of the "
              "check, not a finding", file=sys.stderr)
        return 2

    # Jobs in ci.yml that legitimately still run on a develop push: the nine
    # scenarios (event-conditioned), `coverage` (not in the merge group at all;
    # cut #3 owns it) and `benchmarks` (main-only).
    CI_KEEP = {"benchmarks", "coverage"}

    errors = []
    for wf in DEFAULT_WORKFLOWS:
        path = os.path.join(root, wf)
        if not os.path.exists(path):
            errors.append(f"{wf}: listed in the canary but does not exist")
            continue
        try:
            doc = yaml.safe_load(open(path, encoding="utf-8"))
        except yaml.YAMLError as exc:
            print(f"UNMEASURED: {wf} does not parse: {exc}", file=sys.stderr)
            return 2
        on = doc.get("on") or doc.get(True) or {}
        push = on.get("push") if isinstance(on, dict) else None
        branches = None
        if isinstance(push, dict):
            branches = push.get("branches")
        elif push is not None:
            branches = ["<all branches>"]

        if wf.endswith("/ci.yml"):
            if not branches or "develop" not in branches:
                errors.append(f"{wf}: no longer triggers on develop push at all, "
                              f"so its job-level cut is moot -- remove it from the "
                              f"canary or restore the trigger")
                continue
            jobs = doc.get("jobs") or {}
            for jn, job in jobs.items():
                if not isinstance(job, dict) or jn in CI_KEEP:
                    continue
                cond = str(job.get("if") or "")
                if "merge_group" in cond:
                    continue  # a scenario: never runs in the group, keeps push
                # A job restricted to pull_request cannot run on a develop push
                # either, so it is outside the cut rather than missing from it.
                # The test is a PROOF, not a substring: `X == 'pull_request' ||
                # github.ref == 'refs/heads/develop'` contains the clause and
                # still runs on a push (cleat#3958, cleat-review's finding).
                if pull_request_only(cond):
                    continue
                if "refs/heads/develop" not in cond:
                    errors.append(
                        f"{wf}: job {jn!r} still runs on a develop push and is not "
                        f"one of the {sorted(CI_KEEP)} or a `!= 'merge_group'` scenario")
                else:
                    ok_cond = "!= 'push'" in cond
                    if not ok_cond:
                        errors.append(f"{wf}: job {jn!r} carries the develop ref but "
                                      f"not the push condition: {cond!r}")
            continue

        if branches is None:
            continue  # no push trigger at all: nothing to cut
        if "develop" in branches or "<all branches>" in branches:
            errors.append(f"{wf}: still triggers on develop push ({branches}) -- "
                          f"the cut has drifted from the canary's list")

    if errors:
        for e in errors:
            print(f"::error::{e}", file=sys.stderr)
        return 1
    print(f"OK: the cut and the canary agree on {len(DEFAULT_WORKFLOWS)} workflow(s)")
    return 0


def self_test_cut_list():
    """Known-positive/negative pairs for --check-cut-list.

    The `||` fixture is the one that matters: the first version of the
    pull_request exemption was a substring test, so a job conditioned
    `github.event_name == 'pull_request' || github.ref == 'refs/heads/develop'`
    -- which DOES run on a develop push -- was exempted. Reverting
    pull_request_only() to a substring check must turn this red.
    """
    import os
    import tempfile

    other = ("on:\n  push:\n    branches: [main]\njobs:\n  x:\n"
             "    runs-on: ubuntu-latest\n    steps: [{run: echo}]\n")

    def tree(ci_jobs: str) -> str:
        root = tempfile.mkdtemp(prefix="cutlist-")
        wf = os.path.join(root, ".github", "workflows")
        os.makedirs(wf)
        open(os.path.join(wf, "ci.yml"), "w", encoding="utf-8").write(
            "on:\n  push:\n    branches: [develop]\njobs:\n" + ci_jobs)
        for name in DEFAULT_WORKFLOWS[1:]:
            open(os.path.join(root, name), "w", encoding="utf-8").write(other)
        return root

    non_pr = "    runs-on: ubuntu-latest\n    steps: [{run: echo}]\n"
    cases = [
        ("an edit-gate-shaped job is outside the cut",
         "  edit-gate:\n    if: github.event_name == 'pull_request' && "
         "github.event.action == 'edited'\n" + non_pr, 0),
        ("a scenario keeps the push",
         "  scenario:\n    if: github.event_name != 'merge_group'\n" + non_pr, 0),
        ("a pull_request clause ORed with the develop ref still runs on a push",
         "  sneaky:\n    if: github.event_name == 'pull_request' || "
         "github.ref == 'refs/heads/develop'\n" + non_pr, 1),
        ("an unclassified job still runs on a push",
         "  bare:\n" + non_pr, 1),
    ]
    failures = 0
    for name, jobs, want in cases:
        rc = check_cut_list(tree(jobs))
        ok = rc == want
        if not ok:
            failures += 1
            print(f"  FAIL {name}: rc={rc} want={want}")
        else:
            print(f"  ok   {name}: rc={rc}")
    print(f"{len(cases) - failures}/{len(cases)} cut-list cases passed")
    return 1 if failures else 0


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--runs", required=False,
                    help="JSON file holding the merge_group runs for this commit")
    # Not `required=True`: `--self-test` is a valid invocation on its own, and
    # argparse's own refusal also exits 2, which would make a missing argument
    # indistinguishable from UNMEASURED. Checked explicitly below.
    ap.add_argument("--sha", help="the develop push's head commit")
    ap.add_argument("--workflow", action="append", dest="workflows",
                    help="workflow path that must have a successful merge_group "
                         "run (repeatable; defaults to the cut list)")
    ap.add_argument("--self-test", action="store_true")
    ap.add_argument("--verdict-file", help="write the verdict here for the "
                    "failure notifier to quote (cleat#3962)")
    ap.add_argument("--attempt", type=int, default=1,
                    help="github.run_attempt, recorded inside the verdict file "
                         "so a flat artifact extraction can still be ordered")
    ap.add_argument("--check-cut-list", action="store_true",
                    help="assert the tree's cut matches the canary's workflow list")
    ap.add_argument("--root", default=".", help="repository root for --check-cut-list")
    args = ap.parse_args(argv)

    if args.self_test:
        rc = self_test()
        return rc or self_test_cut_list()
    if args.check_cut_list:
        return check_cut_list(args.root)

    workflows = args.workflows or DEFAULT_WORKFLOWS
    if not args.sha or not args.runs:
        return _verdict(args, 2, ["UNMEASURED: --sha and --runs are both required "
                                  "(this is a failure of the check, not a finding "
                                  "about the commit)"])
    try:
        runs = load_runs(args.runs)
    except ValueError as exc:
        return _verdict(args, 2, [f"UNMEASURED: {exc} -- this is a failure of the "
                                  f"check, not a finding about the commit"])

    findings, running, saw_any = judge(runs, args.sha, workflows)
    if not saw_any:
        return _verdict(args, 1, [
            f"no merge_group run exists for {args.sha}",
            "This commit did not go through the merge queue, so the develop push is "
            "the only place its code can be tested -- and this push no longer runs "
            "the heavy jobs. Re-run them for this commit by hand.",
        ])
    if findings:
        # NOT "no successful run": an older attempt may well have succeeded and
        # the NEWEST still be cancelled or failed (cleat-review on #3967). The
        # reason carries the verdict; the prefix does not assert a second one.
        lines = [f"{wf}: {why} (on {args.sha})" for wf, why in findings]
        lines.append("The develop push skips these workflows because the merge "
                     "group is supposed to have run them on this exact commit. "
                     "The newest attempt for each did not pass, so treat this "
                     "commit as ungated until it does: re-run the skipped jobs by "
                     "hand on this sha.")
        return _verdict(args, 1, lines)
    if running:
        # NOT a finding. The queue merges once the REQUIRED contexts pass, and
        # these workflows finish later than that: judging "still running" as
        # "ungated" would page on every develop push that overtakes a crash
        # shard. Exit 3 means "ask again"; the caller polls and turns a bound
        # expiry into UNMEASURED.
        return _verdict(args, 3, ["STILL RUNNING: " + ", ".join(running)])
    return _verdict(args, 0, [f"OK: all {len(workflows)} workflow(s) passed in a "
                              f"merge_group run on {args.sha}"])


def _verdict(args, rc, lines):
    """Print the verdict, and write it where the notifier can quote it.

    cleat#3962: the tracking issue used to say only `Failed jobs: Develop push
    was queued`, so "still running at the bound" (UNKNOWN) and "no run exists"
    (ungated) looked identical unless someone opened the log. The reason is
    the message THIS function carries, so it is written to a file the notifier
    reads -- not re-derived there from a job name.
    """
    for line in lines:
        prefix = "::error::" if rc == 1 else ""
        print(prefix + line, file=sys.stderr)
    if getattr(args, "verdict_file", None):
        try:
            with open(args.verdict_file, "w", encoding="utf-8") as fh:
                status = {0: "ok", 1: "finding", 2: "UNMEASURED",
                               3: "still running"}.get(rc, str(rc))
                # THE ATTEMPT IS INSIDE THE FILE ON PURPOSE. download-artifact
                # extracts a single match FLAT, with no per-artifact directory,
                # so a reader cannot take the attempt from a directory name --
                # and a single failing push is exactly the common case.
                fh.write(f"canary: {status}\n")
                fh.write(f"attempt: {getattr(args, 'attempt', 1) or 1}\n")
                fh.write("\n".join(lines) + "\n")
        except OSError as exc:  # a write failure must not change the verdict
            print(f"::warning::could not write {args.verdict_file}: {exc}",
                  file=sys.stderr)
    return rc


# --------------------------------------------------------------------------
# Self-test. Fixtures rather than a live API, so it runs in Lint with no token.
# Every case is a case the guard could get WRONG, not a happy path restated.
# --------------------------------------------------------------------------
def self_test():
    good = [{"head_sha": "a" * 40, "path": wf, "conclusion": "success"}
            for wf in DEFAULT_WORKFLOWS]
    cases = []

    def case(name, runs, want_rc, want_in_stderr=None, sha="a" * 40, wfs=None):
        cases.append((name, runs, want_rc, want_in_stderr, sha, wfs))

    case("all five passed", good, 0)
    case("one workflow never ran",
         [r for r in good if r["path"] != DEFAULT_WORKFLOWS[0]], 1,
         "no merge_group run on this commit")
    case("one workflow was cancelled",
         [dict(r, conclusion="cancelled") if r["path"] == DEFAULT_WORKFLOWS[1] else r
          for r in good], 1, "concluded cancelled")
    case("a later attempt succeeded after a cancelled one", good + [
        {"head_sha": "a" * 40, "path": DEFAULT_WORKFLOWS[1], "conclusion": "cancelled"}],
         0)
    case("no merge_group run at all", [], 1, "did not go through the merge queue")
    case("runs belong to a DIFFERENT sha", [dict(r, head_sha="b" * 40) for r in good],
         1, "did not go through the merge queue")

    # cleat#3962: WITHIN ONE WORKFLOW, the NEWEST attempt decides -- in both
    # directions. Before this, the set was judged: an older cancelled attempt
    # plus a newer in-progress run read as "ungated", and, the mirror, an older
    # success could mask a newer failure. Every case below differs ONLY in the
    # ordering fields, so each one fails if _attempt_order stops being used.
    T1, T2 = "2026-11-16T10:00:00Z", "2026-11-16T10:10:00Z"
    WF = DEFAULT_WORKFLOWS[1]

    def others(ts):
        return [dict(r, created_at=ts, run_attempt=1)
                for r in good if r["path"] != WF]

    def two(older, newer):
        return others(T1) + [older, newer]

    case("cancelled attempt, then a NEWER run still in progress -> wait",
         two({"head_sha": "a" * 40, "path": WF, "conclusion": "cancelled",
              "status": "completed", "created_at": T1, "run_attempt": 1},
             {"head_sha": "a" * 40, "path": WF, "conclusion": None,
              "status": "in_progress", "created_at": T2, "run_attempt": 2}),
         3, "STILL RUNNING")
    case("cancelled attempt, then a NEWER run that failed -> a finding",
         two({"head_sha": "a" * 40, "path": WF, "conclusion": "cancelled",
              "status": "completed", "created_at": T1, "run_attempt": 1},
             {"head_sha": "a" * 40, "path": WF, "conclusion": "failure",
              "status": "completed", "created_at": T2, "run_attempt": 2}),
         1, "newest merge_group run concluded failure")
    case("an OLDER success does not mask a NEWER failure",
         two({"head_sha": "a" * 40, "path": WF, "conclusion": "success",
              "status": "completed", "created_at": T1, "run_attempt": 1},
             {"head_sha": "a" * 40, "path": WF, "conclusion": "failure",
              "status": "completed", "created_at": T2, "run_attempt": 2}),
         1, "newest merge_group run concluded failure")
    case("a NEWER success wins over an older failure",
         two({"head_sha": "a" * 40, "path": WF, "conclusion": "failure",
              "status": "completed", "created_at": T1, "run_attempt": 1},
             {"head_sha": "a" * 40, "path": WF, "conclusion": "success",
              "status": "completed", "created_at": T2, "run_attempt": 2}),
         0)

    # cleat-review on #3961: a run that EXISTS but has not finished is WAIT,
    # never a finding. This is the case that shipped broken -- on 343c1cca the
    # ci.yml group run completed at 13:19:45Z against a push run created at
    # 13:17:05Z, so the first version reported "ungated" on a normal merge.
    def running(wf, status="in_progress"):
        return [r if r["path"] != wf else
                {"head_sha": "a" * 40, "path": wf, "conclusion": None, "status": status}
                for r in good]

    case("a workflow still in progress", running(DEFAULT_WORKFLOWS[0]), 3,
         "STILL RUNNING")
    case("a workflow still QUEUED", running(DEFAULT_WORKFLOWS[3], "queued"), 3,
         "STILL RUNNING")
    # The replay: 343c1cca at 13:17:05Z, ci.yml the one still going.
    case("343c1cca as of 13:17:05Z (ci.yml in progress, the rest passed)",
         running(".github/workflows/ci.yml"), 3, "STILL RUNNING")
    # Precedence: a definite finding outranks a transient one. Polling a
    # failure would just wait 25 minutes to report the same thing.
    case("one failed and one still running -> the finding wins",
         [dict(r, conclusion="failure", status="completed")
          if r["path"] == DEFAULT_WORKFLOWS[2] else
          ({"head_sha": "a" * 40, "path": r["path"], "conclusion": None,
            "status": "in_progress"} if r["path"] == DEFAULT_WORKFLOWS[0] else r)
          for r in good], 1, "concluded failure")
    # And the transition itself: once the last one completes, it is a pass.
    case("the same fixture once every run completed", good, 0)

    failed = 0
    for name, runs, want_rc, want_msg, sha, wfs in cases:
        import io
        import tempfile
        import os
        fd, path = tempfile.mkstemp(suffix=".json")
        try:
            with os.fdopen(fd, "w") as fh:
                json.dump({"workflow_runs": runs}, fh)
            err = io.StringIO()
            old, oldout = sys.stderr, sys.stdout
            sys.stderr = sys.stdout = err
            try:
                rc = main(["--runs", path, "--sha", sha]
                          + ([] if not wfs else sum((["--workflow", w] for w in wfs), [])))
            finally:
                sys.stderr, sys.stdout = old, oldout
            text = err.getvalue()
            ok = rc == want_rc and (want_msg is None or want_msg in text)
            print(("ok   " if ok else "FAIL ") + f"{name}: rc={rc} want={want_rc}")
            if not ok:
                failed = 1
                if want_msg is not None:
                    print(f"       wanted {want_msg!r} in: {text.strip()[:200]}")
        finally:
            os.unlink(path)

    # Unmeasured is its own status, and must not be 1 (a finding).
    import io
    import tempfile
    import os
    fd, path = tempfile.mkstemp(suffix=".json")
    os.write(fd, b"not json at all")
    os.close(fd)
    err = io.StringIO()
    old = sys.stderr
    sys.stderr = err
    try:
        rc = main(["--runs", path, "--sha", "a" * 40])
    finally:
        sys.stderr = old
        os.unlink(path)
    ok = rc == 2 and "UNMEASURED" in err.getvalue()
    print(("ok   " if ok else "FAIL ") + f"unparsable input is UNMEASURED: rc={rc} want=2")
    if not ok:
        failed = 1

    print("check-develop-push-was-queued: " +
          ("FAILED" if failed else "all checks passed"))
    return failed


if __name__ == "__main__":
    sys.exit(main())
