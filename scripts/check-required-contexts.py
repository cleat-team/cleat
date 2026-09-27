#!/usr/bin/env python3
"""Guard the required-status-context block in tiers.yaml.

Branch protection on develop lists 33 required contexts. That list lives in GitHub,
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
  5. Disagreement with `.github/required-checks.txt`, in either direction. That file
     predates this block and `scripts/check-workflow-guards.py` reads it, so the two
     are copies of one list. `.golangci.yml` records what happens to two copies of one
     fact -- "two mechanisms with two baselines, which is the shape that let the
     routing tables in 2.72 drift apart" -- and this block was shipped in #729 without
     noticing the file already existed. They were identical when the check was added;
     the check is what keeps them so.

  6. Disagreement with branch protection itself, in either direction, WHEN THE CALLER
     CAN READ IT. Reading it needs admin scope and GITHUB_TOKEN does not have it, so
     this is the one check that does not always run -- and the gap that left is the
     whole of cleat#1937. Until then the comparison lived only in `--check-live`, a
     mode nobody invoked, and the default run ended with

         check-required-contexts: OK, 32 required contexts declared and consistent ...

     on a tree where GitHub required 33. Every word of that is true, and it is
     unreadable as anything but "32 is the number": a numerator whose denominator the
     default path never fetched. `Web Dashboard` gated every merge into develop for
     three days with no `covers:` and no `why_required` -- exactly the state this
     block exists to end -- and the guard over the block printed OK each time.

     So the default run now ATTEMPTS the read and says which of the two happened. It
     fails on a disagreement and NEVER on an inability to read: a guard that goes red
     because the network was slow teaches people to re-run rather than to read. When
     it cannot read it prints NOT CHECKED, names why, and prints the date the two
     lists were last compared (`required_contexts.measured`) -- so the count is never
     handed over unqualified again.

Check 3 only reaches `Test Go (...)` contexts, because the test-go matrix is the only
place a context's packages are written down mechanically. `covers:` on the other 22 --
`Tier 2 Gate`, `Cluster Integration Tests`, `Java Tests` and so on -- is a hand claim
that nothing verifies. The self-test found that limit rather than being told it: its
first version asked check 3 to catch a relabelled `Tier 2 Gate` and printed MISSED.

Usage:
    scripts/check-required-contexts.py              enforce (exit 1 on a finding); the
                                                    live diff runs if the caller can
    scripts/check-required-contexts.py --no-live    enforce without the live attempt
    scripts/check-required-contexts.py --report     print the mapping and re-derivations
    scripts/check-required-contexts.py --self-test  negative control; see below
    scripts/check-required-contexts.py --check-live only the live diff, and here being
                                                    unable to read it IS an error (2)

--self-test is not optional decoration. CLAUDE.md: "a verification script needs its own
negative control ... a loop that cannot see the state it looks for does not fail -- it
prints a confident green." It runs each check against a deliberately broken in-memory
manifest and fails if any of them passes it -- plus a positive control for each half,
since a check that rejects everything would also print its own "caught" line.
"""

import argparse
import copy
import os
import re
import subprocess
import sys

import yaml

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
WORKFLOW_DIR = os.path.join(REPO_ROOT, ".github", "workflows")
REQUIRED_CHECKS_FILE = os.path.join(REPO_ROOT, ".github", "required-checks.txt")
NEEDS_REASON = ("tier2", "undeclared")
# The default run's live attempt is best-effort, so it must be bounded. A guard
# that hangs on a slow API is a guard nobody runs.
LIVE_TIMEOUT_SECONDS = 20

findings = []


def fail(msg):
    findings.append(msg)


def load(path=None):
    with open(path or os.path.join(REPO_ROOT, "tiers.yaml")) as f:
        return yaml.safe_load(f)


def read_required_checks_file():
    """The context list in .github/required-checks.txt, or None if absent.

    That file predates this block and scripts/check-workflow-guards.py reads it.
    Two hand-maintained copies of one fact is the shape .golangci.yml warns about
    ("one class of finding two mechanisms with two baselines, which is the shape
    that let the routing tables in 2.72 drift apart"), so check 5 below asserts
    they are equal rather than letting them drift.
    """
    if not os.path.isfile(REQUIRED_CHECKS_FILE):
        return None
    out = []
    with open(REQUIRED_CHECKS_FILE) as handle:
        for line in handle:
            line = line.strip()
            if line and not line.startswith("#"):
                out.append(line)
    return out


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

    # 5. this block and .github/required-checks.txt are two copies of one list
    from_file = read_required_checks_file()
    if from_file is None:
        fail(f"{REQUIRED_CHECKS_FILE} is missing; scripts/check-workflow-guards.py "
             f"reads it and this block is supposed to agree with it")
    else:
        here, there = set(names), set(from_file)
        for c in sorted(there - here):
            fail(f"{c!r} is in .github/required-checks.txt but not declared here. "
                 f"Two hand-maintained copies of one list drift; add it with a "
                 f"`covers:` and, if it is not tier 1, a `why_required:`.")
        for c in sorted(here - there):
            fail(f"{c!r} is declared here but not in .github/required-checks.txt. "
                 f"check-workflow-guards.py reads that file, so a context only in "
                 f"this block is not checked against the workflow jobs at all.")

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
            # A stale `undeclared` is the one that rots QUIETLY, because it rots by
            # being FIXED: someone assigns the missing package a tier and this label
            # goes on claiming a gap that is closed. Shipped without this check in
            # #729, and it passed silently the first time a tier was assigned --
            # which is exactly the "guard that cannot see the state it looks for"
            # this script's own docstring is about.
            if covers == "undeclared" and "undeclared" not in actual:
                fail(f"{name!r}: declared covers: undeclared, but every path now has a "
                     f"tier ({sorted(actual)}). Reclassify it -- the `why_required` "
                     f"below it is describing a gap that no longer exists.")
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
    print("\nThe live half, which needs admin scope. The default run attempts it and "
          "says\nso when it cannot; --check-live is the explicit mode. By hand:")
    print("  gh api repos/cleat-team/cleat/branches/develop/protection \\")
    print("    --jq '.required_status_checks.contexts[]' | sort")


def declared_contexts(tiers):
    return {c["context"] for c in tiers["required_contexts"]["contexts"]}


def fetch_live(branch):
    """What branch protection actually requires -> (set, None), or (None, why-not).

    Reading it needs admin scope, so "cannot read" is the ORDINARY case and is not
    an error here. What must never happen is a run that could not read printing a
    line a reader takes for one that did -- see check 6 in the module docstring.

    Every failure mode collapses to the same (None, reason) shape: gh absent, gh
    unauthenticated, no network, and a slow API all mean the same thing to a caller,
    and treating any of them as a finding would make a required check a function of
    the runner's connectivity.
    """
    try:
        proc = subprocess.run(
            ["gh", "api", f"repos/:owner/:repo/branches/{branch}/protection",
             "--jq", ".required_status_checks.contexts[]"],
            capture_output=True, text=True, cwd=REPO_ROOT,
            timeout=LIVE_TIMEOUT_SECONDS)
    except FileNotFoundError:
        return None, "gh is not on PATH"
    except subprocess.TimeoutExpired:
        return None, f"gh did not answer within {LIVE_TIMEOUT_SECONDS}s"
    if proc.returncode != 0:
        return None, gh_error_reason(proc.stderr) or f"gh exited {proc.returncode}, silently"
    live = {l.strip() for l in proc.stdout.splitlines() if l.strip()}
    if not live:
        # An empty answer is not "develop requires nothing"; it is a jq path that
        # matched nothing, which is the shape of a renamed field. Reporting it as
        # 33 deletions would be a confident wrong answer, so it reads as unreadable.
        return None, ("branch protection returned no contexts at all, which is not a "
                      "state this repo is in -- treating the read as failed")
    return live, None


def gh_error_reason(stderr):
    """The one line of gh's stderr worth printing, or "" if there is none.

    Shipped taking the LAST line, which was wrong in the first place it ran.
    In CI the Lint job sets no GH_TOKEN, so gh answers with a four-line hint
    whose last line is the YAML fragment `GH_TOKEN: ${{ github.token }}` -- and
    the guard printed

        this run did not read it:
              GH_TOKEN: ${{ github.token }}

    which names a variable rather than a reason. Prefer the line carrying an
    HTTP status, since that is the answer when there IS one; otherwise the
    first line, which is where gh puts the sentence.
    """
    lines = [l.strip() for l in (stderr or "").splitlines() if l.strip()]
    if not lines:
        return ""
    for line in lines:
        if re.search(r"HTTP \d{3}", line):
            return line
    return lines[0]


def diff_live(live, declared, branch="develop"):
    """Findings from comparing the live required set against the declared one.

    Pure, and separate from fetch_live, so --self-test can falsify it with no token
    and no network. Both directions are findings: a context required but not
    declared is cleat#1937's shape, and one declared but no longer required is this
    block claiming to govern something that has stopped gating anything.
    """
    out = []
    for missing in sorted(live - declared):
        out.append(f"required on {branch} but NOT declared in tiers.yaml: {missing!r}")
    for extra in sorted(declared - live):
        out.append(f"declared in tiers.yaml but NOT required on {branch}: {extra!r}")
    return out


def check_live(tiers):
    """--check-live: the explicit mode, where being unable to read IS an error."""
    branch = tiers["required_contexts"].get("branch", "develop")
    live, why = fetch_live(branch)
    if live is None:
        print(f"--check-live: cannot read branch protection for {branch} "
              f"(this usually means the token lacks admin scope):\n"
              f"  {why}", file=sys.stderr)
        return 2
    problems = diff_live(live, declared_contexts(tiers), branch)
    for p in problems:
        print(p)
    if problems:
        return 1
    print(f"--check-live: {len(live)} contexts, declared list matches exactly.")
    return 0


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
    case("a context still marked undeclared after its packages got a tier",
         lambda d: first_of(d, "tier2", go_matrix=True).update(covers="undeclared"))
    def drop_one(d):
        """Remove a context AND fix `total`, so ONLY check 5 fires.

        WORKSTREAM.md's verification protocol: "A falsification that fires two
        assertions proves neither." Dropping a context without adjusting `total`
        would trip check 4 as well and prove nothing about check 5.
        """
        d["required_contexts"]["contexts"].pop()
        d["required_contexts"]["total"] -= 1

    case("a context dropped here but still in .github/required-checks.txt", drop_one)
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

    # --- check 6, the live diff ------------------------------------------------------
    #
    # fetch_live is deliberately NOT exercised here. It needs admin scope this run may
    # not have, and a self-test that quietly skips when a credential is missing is the
    # exact shape the docstring above is about -- it would print the same thing whether
    # the comparison worked or could not run. So the comparison is a pure function over
    # two sets, and it is falsified with literals, on every machine, with no token.
    declared = declared_contexts(base)
    live_cases = [
        ("a context required on develop but not declared here (cleat#1937's own shape)",
         declared | {"Some Check Added In The GitHub UI"}),
        ("a context declared here but no longer required on develop",
         declared - {min(declared)}),
    ]
    for label, live in live_cases:
        caught = diff_live(live, declared)
        status = "caught" if caught else "MISSED"
        if not caught:
            ok = False
        print(f"  {status:6s} {label}")
        if caught:
            print(f"         -> {caught[0]}")

    # gh_error_reason, with the real CI stderr that made it necessary. Pure and
    # falsifiable for the same reason diff_live is: this runs where gh cannot.
    reason_cases = [
        ("gh unauthenticated in CI: the sentence, not the YAML fragment",
         "To use GitHub CLI in a GitHub Actions workflow, set the GH_TOKEN "
         "environment variable. Example:\n  env:\n    GH_TOKEN: "
         "${{ github.token }}\n",
         "To use GitHub CLI in a GitHub Actions workflow, set the GH_TOKEN "
         "environment variable. Example:"),
        ("a token without admin scope: the HTTP line, wherever it sits",
         "\ngh: Resource not accessible by integration (HTTP 403)\n",
         "gh: Resource not accessible by integration (HTTP 403)"),
        # A preamble ABOVE and a hint BELOW, so neither lines[0] nor lines[-1]
        # answers this one -- it is the case that pins the rule rather than a
        # position.
        ("an HTTP line between a preamble and a hint is still the one picked",
         "some preamble gh printed first\ngh: Bad credentials (HTTP 401)\n"
         "Try: gh auth login\n",
         "gh: Bad credentials (HTTP 401)"),
        ("nothing on stderr is reported as nothing, not as a blank reason",
         "   \n\n", ""),
    ]
    for label, stderr, want in reason_cases:
        got = gh_error_reason(stderr)
        status = "ok    " if got == want else "MISSED"
        if got != want:
            ok = False
        print(f"  {status} gh_error_reason: {label}")
        if got != want:
            print(f"         -> got {got!r}, want {want!r}")

    # And its positive control, for the same reason the one above exists: a diff that
    # reported on every input would have "caught" both cases too.
    residual = diff_live(declared, declared)
    if residual:
        ok = False
        print("  MISSED two identical lists should produce no findings, but:")
        for f in residual:
            print(f"         -> {f}")
    else:
        print("  passes the live diff over two identical lists (positive control)")

    return ok


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--report", action="store_true")
    ap.add_argument("--self-test", action="store_true")
    ap.add_argument("--check-live", action="store_true")
    ap.add_argument("--no-live", action="store_true",
                    help="skip the default run's live attempt (offline or hermetic runs)")
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

    # Check 6. Best-effort by construction: a disagreement is a finding, an inability
    # to read is a sentence. The distinction is the point -- see the docstring.
    branch = tiers["required_contexts"].get("branch", "develop")
    n = len(tiers["required_contexts"]["contexts"])
    if args.no_live:
        live, why = None, "--no-live was passed"
    else:
        live, why = fetch_live(branch)
    live_findings = diff_live(live, declared_contexts(tiers), branch) if live else []

    if findings or live_findings:
        print("check-required-contexts: FAIL", file=sys.stderr)
        for f in findings + live_findings:
            print(f"  {f}", file=sys.stderr)
        print("\n  tiers.yaml's required_contexts block is the in-tree record of what "
              "blocks a merge.\n  Fix the entry, or if branch protection changed, update "
              "the block to match.", file=sys.stderr)
        if live_findings:
            print(f"\n  The last {len(live_findings)} came from reading branch "
                  f"protection on {branch} itself, so\n  this tree and GitHub disagree "
                  f"about what blocks a merge RIGHT NOW. Whichever\n  is wrong, both "
                  f"halves move together -- tiers.yaml, .github/required-checks.txt,\n"
                  f"  and required_contexts.measured to the date you compared them.",
                  file=sys.stderr)
        return 1

    print(f"check-required-contexts: OK, {n} contexts declared, internally consistent "
          f"with tiers.yaml and .github/workflows/.")
    if live:
        print(f"check-required-contexts: and branch protection on {branch} requires "
              f"exactly these {n}.")
    else:
        # Never hand over the count unqualified. The line this replaces read
        # "OK, 32 required contexts declared and consistent" on a tree where GitHub
        # required 33, and nothing in it said the live list had not been opened.
        print(f"check-required-contexts: NOT CHECKED -- whether branch protection on "
              f"{branch} still\n  requires exactly these {n}. Reading it needs admin "
              f"scope, and this run did not read it:\n    {why}\n"
              f"  So {n} is what this tree DECLARES, not what GitHub "
              f"enforces. The two lists were\n  last compared on "
              f"{tiers['required_contexts'].get('measured')} "
              f"(required_contexts.measured).\n"
              f"  With the scope:  scripts/check-required-contexts.py --check-live")
    return 0


if __name__ == "__main__":
    sys.exit(main())
