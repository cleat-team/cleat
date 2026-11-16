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
`gh-readonly-queue/develop/pr-3942-f0e717ac… [checks_requested]` -- not the
workflow's name, because these workflows carry no top-level `name:`. A guard
keyed on the display name finds nothing and would report every develop push as
ungated; keyed on the path it matches 125 of 125 known-good (25 commits x 5).

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


def judge(runs, sha, workflows):
    """Return (missing, saw_any): missing is a list of (workflow, why)."""
    mine = [r for r in runs if r.get("head_sha") == sha]
    missing = []
    for wf in workflows:
        candidates = [r for r in mine if r.get("path") == wf]
        if not candidates:
            missing.append((wf, "no merge_group run on this commit"))
            continue
        conclusions = sorted({str(r.get("conclusion")) for r in candidates})
        if "success" not in conclusions:
            missing.append((wf, "merge_group run concluded " + ", ".join(conclusions)))
    return missing, bool(mine)


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
    ap.add_argument("--check-cut-list", action="store_true",
                    help="assert the tree's cut matches the canary's workflow list")
    ap.add_argument("--root", default=".", help="repository root for --check-cut-list")
    args = ap.parse_args(argv)

    if args.self_test:
        return self_test()
    if args.check_cut_list:
        return check_cut_list(args.root)

    workflows = args.workflows or DEFAULT_WORKFLOWS
    if not args.sha or not args.runs:
        print("UNMEASURED: --sha and --runs are both required (this is a failure "
              "of the check, not a finding about the commit)", file=sys.stderr)
        return 2
    try:
        runs = load_runs(args.runs)
    except ValueError as exc:
        print(f"UNMEASURED: {exc} -- this is a failure of the check, not a "
              f"finding about the commit", file=sys.stderr)
        return 2

    missing, saw_any = judge(runs, args.sha, workflows)
    if not saw_any:
        print(f"::error::no merge_group run exists for {args.sha}. This commit "
              f"did not go through the merge queue, so the develop push is the "
              f"only place its code can be tested -- and this push no longer "
              f"runs the heavy jobs. Re-run them for this commit by hand.",
              file=sys.stderr)
        return 1
    if missing:
        for wf, why in missing:
            print(f"::error::{wf} has no successful merge_group run on "
                  f"{args.sha}: {why}", file=sys.stderr)
        print("::error::the develop push skips these workflows because the merge "
              "group is supposed to have run them on this exact commit. It did "
              "not, so this commit is ungated and the skipped jobs must be run "
              "by hand on this sha.", file=sys.stderr)
        return 1
    print(f"OK: all {len(workflows)} workflow(s) passed in a merge_group run on "
          f"{args.sha}")
    return 0


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
