#!/usr/bin/env python3
"""Guard the required-status-context block in tiers.yaml.

Branch protection on develop lists 32 required contexts. That list lives in GitHub,
not in this tree, and nothing kept it honest against the tiers -- so five contexts had
come to gate tier-2 code as must-pass with nothing recording the decision, and two
packages inside a required context belonged to no tier at all.

`tiers.yaml: required_contexts` is the in-tree half of that mapping. This script checks
it, and fails on:

  1. A declared context whose workflow file does not exist, or whose `job:` is not a
     job in it. That is the anti-rot check scripts/tier2-gate.sh already applies to
     tier2.gated_by: a renamed job leaves the mapping claiming coverage that is gone,
     and a required context nothing reports blocks every PR while looking like a slow
     check.
  2. A context marked `covers: tier2` or `covers: undeclared` with no `why_required`.
     This is the rule the block exists for. A tier-2 package held to must-pass is a
     fine thing to decide and a bad thing to discover.
  3. `covers: tier1` on a context that in fact runs a package tiers.yaml puts in
     tier 2, or in no tier. Computed from ci.yml's test-go matrix against
     tier1/tier2.packages, so the classification cannot be asserted by hand -- which
     is how the block would rot back into the state it was written to fix.
  4. A duplicate context name, or a `total:` that disagrees with the number declared.

Check 3 only reaches `Test Go (...)` contexts, because the test-go matrix is the only
place a context's packages are written down mechanically. `covers:` on the other 21 --
`Tier 2 Gate`, `Cluster Integration Tests`, `Java Tests` and so on -- is a hand claim
that nothing verifies. The self-test found that limit rather than being told it: its
first version asked check 3 to catch a relabelled `Tier 2 Gate` and printed MISSED.

WHAT IT CANNOT CHECK, stated because a guard whose limits are unwritten gets read as
covering more than it does: whether the declared list still equals what GitHub
actually requires. Reading branch protection needs admin scope and GITHUB_TOKEN does
not have it. tier2.gated_by records the same limitation for its own mapping. The live
half is one command, and `--report` prints it:

    gh api repos/cleat-team/cleat/branches/develop/protection \
      --jq '.required_status_checks.contexts[]' | sort

Usage:
    scripts/check-required-contexts.py              enforce (exit 1 on a finding)
    scripts/check-required-contexts.py --report     print the mapping and re-derivations
    scripts/check-required-contexts.py --self-test  negative control; see below
    scripts/check-required-contexts.py --check-live diff against branch protection,
                                                    when the caller has the scope

--self-test is not optional decoration. CLAUDE.md: "a verification script needs its own
negative control ... a loop that cannot see the state it looks for does not fail -- it
prints a confident green." It runs each of the four checks against a deliberately
broken in-memory manifest and fails if any of them passes it -- plus a positive
control, since six checks that reject everything would also print six "caught" lines.
"""

import argparse
import copy
import os
import subprocess
import sys

import yaml

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
WORKFLOW_DIR = os.path.join(REPO_ROOT, ".github", "workflows")
NEEDS_REASON = ("tier2", "undeclared")

findings = []


def fail(msg):
    findings.append(msg)


def load(path=None):
    with open(path or os.path.join(REPO_ROOT, "tiers.yaml")) as f:
        return yaml.safe_load(f)


def workflow_jobs(filename):
    """Job ids declared in a workflow file, or None if the file is missing."""
    path = os.path.join(WORKFLOW_DIR, filename)
    if not os.path.isfile(path):
        return None
    with open(path) as f:
        doc = yaml.safe_load(f)
    return set((doc or {}).get("jobs", {}) or {})


def matrix_tiers(tiers):
    """Map each test-go matrix entry name to the tiers its paths belong to.

    Returns {name: set(...)} over {"tier1", "tier2", "undeclared"}. The `core` entry
    carries `dir: cleat`, so its ./... is the cleat module -- tier1.modules, not the
    root tree -- and is classified tier1 rather than by path.
    """
    t1 = set(tiers.get("tier1", {}).get("packages") or [])
    t2 = set(tiers.get("tier2", {}).get("packages") or [])
    modules = {m.get("dir") for m in (tiers.get("tier1", {}).get("modules") or [])}

    jobs = workflow_jobs("ci.yml")
    if jobs is None:
        return {}
    with open(os.path.join(WORKFLOW_DIR, "ci.yml")) as f:
        ci = yaml.safe_load(f)
    out = {}
    for entry in ci["jobs"]["test-go"]["strategy"]["matrix"]["package"]:
        if entry.get("dir"):
            out[entry["name"]] = {"tier1"} if entry["dir"] in modules else {"undeclared"}
            continue
        kinds = set()
        for path in entry["path"].split():
            kinds.add("tier1" if path in t1 else "tier2" if path in t2 else "undeclared")
        out[entry["name"]] = kinds
    return out


def go_matrix_name(context):
    """'Test Go (scale) on 1.26' -> 'scale'; None for anything else."""
    if context.startswith("Test Go (") and ")" in context:
        return context[len("Test Go (") : context.index(")")]
    return None


def check(tiers, *, verbose=False):
    block = tiers.get("required_contexts")
    if not block:
        fail("tiers.yaml has no `required_contexts:` block")
        return
    contexts = block.get("contexts") or []

    # 4. duplicates and the declared total
    names = [c.get("context") for c in contexts]
    for name in sorted({n for n in names if names.count(n) > 1}):
        fail(f"context declared more than once: {name!r}")
    if block.get("total") is not None and block["total"] != len(contexts):
        fail(f"required_contexts.total is {block['total']} but {len(contexts)} "
             f"contexts are declared")

    tiers_of = matrix_tiers(tiers)

    for entry in contexts:
        name = entry.get("context", "<unnamed>")

        # 1. the job it names still exists
        wf, job = entry.get("workflow"), entry.get("job")
        jobs = workflow_jobs(wf) if wf else None
        if jobs is None:
            fail(f"{name!r}: workflow {wf!r} does not exist under .github/workflows/")
        elif job not in jobs:
            fail(f"{name!r}: job {job!r} is not a job in {wf} "
                 f"(a renamed job leaves this mapping claiming coverage that is gone)")

        covers = entry.get("covers")
        if covers not in ("tier1", "tier2", "undeclared", "infra"):
            fail(f"{name!r}: covers must be tier1|tier2|undeclared|infra, got {covers!r}")
            continue

        # 2. anything gating non-tier-1 code has to say why
        if covers in NEEDS_REASON and not (entry.get("why_required") or "").strip():
            fail(f"{name!r}: covers: {covers} requires a `why_required`. A tier-2 or "
                 f"untiered package held to must-pass must say why it blocks a merge.")

        # 3. the claim is checked against the matrix, not taken
        matrix = go_matrix_name(name)
        if matrix and matrix in tiers_of:
            actual = tiers_of[matrix]
            if covers == "tier1" and actual != {"tier1"}:
                fail(f"{name!r}: declared covers: tier1, but its matrix paths resolve "
                     f"to {sorted(actual)} against tiers.yaml")
            if covers == "tier2" and "tier2" not in actual:
                fail(f"{name!r}: declared covers: tier2, but no path resolves to tier 2 "
                     f"(resolves to {sorted(actual)})")
            if verbose:
                print(f"  {name:32s} covers={covers:10s} matrix={sorted(actual)}")


def report(tiers):
    block = tiers["required_contexts"]
    contexts = block["contexts"]
    print(f"tiers.yaml required_contexts: {len(contexts)} on "
          f"{block.get('branch')} (measured {block.get('measured')})\n")
    by = {}
    for c in contexts:
        by.setdefault(c.get("covers"), []).append(c["context"])
    for kind in ("tier1", "tier2", "undeclared", "infra"):
        got = by.get(kind, [])
        print(f"  {kind:11s} {len(got):2d}")
        for n in got:
            print(f"      {n}")
    print("\ntest-go matrix resolved against tier1/tier2.packages:")
    for name, kinds in sorted(matrix_tiers(tiers).items()):
        print(f"  {name:11s} {sorted(kinds)}")
    print("\nThe live half this cannot see (needs admin scope):")
    print("  gh api repos/cleat-team/cleat/branches/develop/protection \\")
    print("    --jq '.required_status_checks.contexts[]' | sort")


def check_live(tiers):
    """Diff the declared list against branch protection. Needs admin scope."""
    branch = tiers["required_contexts"].get("branch", "develop")
    proc = subprocess.run(
        ["gh", "api", f"repos/:owner/:repo/branches/{branch}/protection",
         "--jq", ".required_status_checks.contexts[]"],
        capture_output=True, text=True, cwd=REPO_ROOT)
    if proc.returncode != 0:
        print(f"--check-live: cannot read branch protection for {branch} "
              f"(this usually means the token lacks admin scope):\n"
              f"  {proc.stderr.strip()}", file=sys.stderr)
        return 2
    live = {l.strip() for l in proc.stdout.splitlines() if l.strip()}
    declared = {c["context"] for c in tiers["required_contexts"]["contexts"]}
    rc = 0
    for missing in sorted(live - declared):
        print(f"required on {branch} but NOT declared in tiers.yaml: {missing!r}")
        rc = 1
    for extra in sorted(declared - live):
        print(f"declared in tiers.yaml but NOT required on {branch}: {extra!r}")
        rc = 1
    if rc == 0:
        print(f"--check-live: {len(live)} contexts, declared list matches exactly.")
    return rc


def self_test():
    """Negative control: each check must reject a manifest broken in exactly its way."""
    base = load()
    cases = []

    def case(label, mutate):
        cases.append((label, mutate))

    def first_of(doc, covers, go_matrix=False):
        """A declared context with this `covers`, optionally one check 3 can see.

        go_matrix matters: check 3 resolves a context against ci.yml's test-go
        matrix, so it can only fire on a `Test Go (...)` context. The first draft of
        this self-test asked it to catch a relabelled `Tier 2 Gate` and reported
        MISSED -- correctly, and that is the whole reason this runs.
        """
        for c in doc["required_contexts"]["contexts"]:
            if c.get("covers") != covers:
                continue
            if go_matrix and go_matrix_name(c["context"]) is None:
                continue
            return c
        raise AssertionError(f"self-test needs a context with covers: {covers}")

    case("a job that no longer exists",
         lambda d: d["required_contexts"]["contexts"][0].update(job="no-such-job-id"))
    case("a workflow file that no longer exists",
         lambda d: d["required_contexts"]["contexts"][0].update(workflow="gone.yml"))
    case("covers: tier2 with no why_required",
         lambda d: first_of(d, "tier2").pop("why_required", None))
    case("a tier-2 context relabelled covers: tier1",
         lambda d: first_of(d, "tier2", go_matrix=True).update(covers="tier1"))
    case("a duplicated context",
         lambda d: d["required_contexts"]["contexts"].append(
             copy.deepcopy(d["required_contexts"]["contexts"][0])))
    case("a total that disagrees with the list",
         lambda d: d["required_contexts"].update(total=999))

    ok = True
    for label, mutate in cases:
        doc = copy.deepcopy(base)
        mutate(doc)
        global findings
        saved, findings = findings, []
        check(doc)
        caught, findings = findings, saved
        status = "caught" if caught else "MISSED"
        if not caught:
            ok = False
        print(f"  {status:6s} {label}")
        if caught:
            print(f"         -> {caught[0]}")

    # The positive control. Six checks that reject everything would also print six
    # "caught" lines, so the committed manifest has to pass or the run above proves
    # nothing about the checks being specific.
    saved, findings = findings, []
    check(base)
    residual, findings = findings, saved
    if residual:
        ok = False
        print("  MISSED tiers.yaml as committed should pass, but:")
        for f in residual:
            print(f"         -> {f}")
    else:
        print("  passes tiers.yaml as committed (positive control)")
    return ok


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--report", action="store_true")
    ap.add_argument("--self-test", action="store_true")
    ap.add_argument("--check-live", action="store_true")
    args = ap.parse_args()

    if args.self_test:
        print("check-required-contexts: self-test")
        if not self_test():
            print("check-required-contexts: SELF-TEST FAILED -- a check cannot see the "
                  "state it looks for, so a green from it means nothing", file=sys.stderr)
            return 1
        print("check-required-contexts: self-test passed")
        return 0

    tiers = load()
    if args.report:
        report(tiers)
        return 0
    if args.check_live:
        return check_live(tiers)

    check(tiers)
    if findings:
        print("check-required-contexts: FAIL", file=sys.stderr)
        for f in findings:
            print(f"  {f}", file=sys.stderr)
        print("\n  tiers.yaml's required_contexts block is the in-tree record of what "
              "blocks a merge.\n  Fix the entry, or if branch protection changed, update "
              "the block to match.", file=sys.stderr)
        return 1
    n = len(tiers["required_contexts"]["contexts"])
    print(f"check-required-contexts: OK, {n} required contexts declared and consistent "
          f"with tiers.yaml and .github/workflows/.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
