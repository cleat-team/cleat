#!/usr/bin/env python3
"""One structural rule for the C# guest's tier-2 scaffolding (cleat#4080): C# Go-tests live in ONE tier-2 package.

A test function whose name starts with TestCSharp, TestDotNet or TestDotnet, and any test file that names the C#
toolchain (componentize-dotnet, ILCompiler, dotnet-experimental, nuget, mcr.microsoft.com/dotnet), must be under
tests/csharp/. That package must be in tiers.yaml's tier2.packages and must NOT be selected by any tier-1 pattern --
decided by asking `go list`, not by comparing strings, because a pattern can reach the package without containing its name
(`./tests/csharp`, `./tests/csharp/`, `./tests/cshar...`). A tier-2 test in a tier-1 package has leaked three times in this
repo; the exclusion is structural, so there is no pattern list to rot. The rule also refuses to pass over zero tests, and
over a `go list` that cannot show engine in tier 1.

This is the TypeScript guard's placement rule (scripts/check-typescript-tier.py, cleat#3825) for the C# package, with the
same fail-closed behaviours. It is a separate script because the TypeScript guard is also about that language's doc claims
and README, which C# does not need: LANGUAGE_SUPPORT.md's language table is already checked against tiers.yaml for EVERY row,
C# included, by check-typescript-tier.py. NOT covered: the SDK itself and its managed unit tests, which live in
packages/cleat-csharp/ and run in the pinned SDK container, not under `go test`.

It ALSO checks the workflow that runs those tests in CI (.github/workflows/csharp-tier2.yml, C# PR 7): that its two jobs declare the
toolchain to the tests (CLEAT_REQUIRE_TOOLCHAINS names csharp in both, and csharp-arm64 in the arm64 one and NOT in the x86_64 one),
that the arm64 job runs on an arm runner and the x86_64 one does not, that neither is a REQUIRED context in tiers.yaml (they sit behind
a workflow-level paths filter, which a required context must not: it would never report on a PR that touches no C#), that the workflow
has no merge_group trigger, and that each job's skip budget has a ledger fragment. A declaration like CLEAT_REQUIRE_TOOLCHAINS is the
kind that rots silently: delete it and every test that was a failure becomes a skip, and the job stays green.

Exit statuses (CLAUDE.md, "give 'I could not look' its own exit status"):
  0  passed
  1  a finding: a C# test is misplaced, or the package is missing from tier2.packages or selected by tier 1
  2  UNMEASURED: the check could not establish what it measures (missing file, no tests, `go` or PyYAML absent). This is a
     failure of the check, not a finding about the tree.

  usage: scripts/check-csharp-tier.py             check the repository
         scripts/check-csharp-tier.py --self-test known-positive / negative-control pairs
"""
import copy
import os
import re
import subprocess
import sys

try:
    import yaml
except ImportError:
    print("UNMEASURED: PyYAML is not installed (pip install pyyaml); the check cannot read tiers.yaml.\n"
          "(this is a failure of the check, not a finding about the tree)", file=sys.stderr)
    sys.exit(2)

CS_PKG = "tests/csharp"
CS_PATTERN = "./tests/csharp/..."

NAME_PREFIX = re.compile(r"^func (Test(?:CSharp|DotNet|Dotnet)\w*)\(", re.M)
TOOLCHAIN = re.compile(r"componentize[-._]?dotnet|ilcompiler|dotnet-experimental|\bnuget\b|mcr\.microsoft\.com/dotnet", re.I)

# Paths allowed to sit outside tests/csharp/ although they are about C#, with the reason. It applies to BOTH checks (a TestCSharp*
# name outside the package, and a mention of the toolchain outside it). A path->reason map rather than a looser regex: it makes a
# person state why, once, where the next reader can find it.
#
# The reason that fits is a test that needs NO toolchain and CANNOT skip: it reads a checked-in prebuilt fixture (a .wasm in
# testdata/), so it runs in Tier 1 on every machine. The rule exists to keep tests that SKIP for want of a toolchain out of Tier 1,
# where a skip is a failure; such a test does not need it. State in the reason which file it reads, so the claim can be checked.
#
# Empty at the time this guard was written (2026-11-18), because every C# test on develop was in tests/csharp/. C# PR 3 (engine)
# adds tests of the prebuilt-fixture kind and is where its entries belong: a stale or missing path is UNMEASURED
# (check_exemptions), so an entry cannot be written before its file exists.
EXEMPT = {
    "engine/csharp_defer_segment_e2e_test.go":
        "reads the checked-in engine/testdata/csharp_defer_order/defer_order.wasm (a prebuilt C# component), needs no toolchain and "
        "has no t.Skip, so it runs in Tier 1 on every machine; it tests the ENGINE's defer-segment path, which belongs in engine/ "
        "(cleat#4080, C# PR 3)",
    "engine/a_component_gets_the_durable_clock_and_entropy_at_every_wasi_minor_test.go":
        "reads the checked-in engine/testdata/wasi_minor/probe-*.wasm (tiny hand-written components), needs no toolchain and has no "
        "t.Skip; it names componentize-dotnet only in a comment explaining why the WASI minor matters (cleat#4080, C# PR 3)",
    "engine/a_real_csharp_component_is_held_through_a_sleep_and_fenced_test.go":
        "reads the checked-in engine/testdata/csharp_defer_order/defer_order.wasm (a prebuilt C# component) and stamps it as csharp in "
        "the test, needs no toolchain and has no t.Skip (the file is separate from the Python rows precisely so that this is true), so it "
        "runs in Tier 1 on every machine; it tests the ENGINE's residency, fence and charge accounting on a real component, which belongs "
        "in engine/ (cleat#4162 part A, cleat#4278)",
    "cmd/cleat/csharp_vet_test.go":
        "tests the determinism VET, a Go lexer and rule table over .cs source text; it reads no component and runs no toolchain, and has "
        "no t.Skip, so it runs in Tier 1 on every machine. Its TestCSharp* names say what the vet is about, not what it needs "
        "(cleat#4080, C# PR 6)",
}


class Unmeasured(Exception):
    pass


def check_exemptions(test_files, exempt):
    """Raise when an exemption names a path the scan did not see: a renamed path leaves the reason behind and refuses the new
    path, which is worse than no exemption."""
    missing = [q for q in exempt if q not in test_files]
    if missing:
        raise Unmeasured("EXEMPT names path(s) the scan did not see: " + ", ".join(sorted(missing)))


def check_placement(tiers, test_files, resolve, cs_pkg_path, exempt=None):
    """test_files: {path: source text} for every tracked *_test.go.
    resolve(patterns) -> set of package import paths, as `go list` would answer.
    cs_pkg_path: the import path of tests/csharp."""
    findings = []
    t2 = (tiers.get("tier2") or {}).get("packages") or []
    t1 = (tiers.get("tier1") or {}).get("packages") or []
    if CS_PATTERN not in t2:
        findings.append(f"tiers.yaml tier2.packages does not list {CS_PATTERN}")
    else:
        got = resolve([CS_PATTERN])
        if got != {cs_pkg_path}:
            findings.append(f"tier2 pattern {CS_PATTERN} resolves to {sorted(got)}, want exactly {cs_pkg_path}")
    t1_set = resolve(t1)
    # The positive control: a resolver that returns nothing, or cannot see engine, would make "the C# package is not in tier 1"
    # true of everything.
    if not any(p.endswith("/engine") for p in t1_set):
        raise Unmeasured("resolving tier1.packages did not include engine, so the resolver cannot be trusted to say the C# "
                         "package is absent")
    if cs_pkg_path in t1_set:
        via = [p for p in t1 if cs_pkg_path in resolve([p])]
        findings.append(f"tier1.packages selects the C# package (via {via}); its tests would run in the Tier 1 gate, where a "
                        f"skip is a failure")
    inside = 0
    ex = EXEMPT if exempt is None else exempt
    for path, src in sorted(test_files.items()):
        in_pkg = path.startswith(CS_PKG + "/")
        for m in NAME_PREFIX.finditer(src):
            if in_pkg:
                inside += 1
            elif path not in ex:
                findings.append(f"{path}: {m.group(1)} is outside {CS_PKG}/ (a C# test in another package can run in Tier 1, "
                                f"where a skip is a failure)")
        if not in_pkg and TOOLCHAIN.search(src) and path not in ex:
            findings.append(f"{path} names the C# toolchain (componentize-dotnet, ILCompiler, dotnet-experimental, nuget or the "
                            f".NET SDK image) but is outside {CS_PKG}/; a test that needs it cannot run in Tier 1")
    if inside == 0:
        raise Unmeasured(f"{CS_PKG}/ holds no TestCSharp* function, so the placement rule examined nothing and 'must run' "
                         f"would be satisfied by an empty package")
    return findings


WORKFLOW = ".github/workflows/csharp-tier2.yml"
WF_X86 = "csharp-tier2"
WF_ARM = "csharp-tier2-arm64"
LEDGER_DIR = "scripts/skip-ledger.d"


def _declared(job):
    """The toolchains a job declares to its tests, from its job-level env (the declaration this workflow uses)."""
    env = (job or {}).get("env") or {}
    return {t.strip() for t in str(env.get("CLEAT_REQUIRE_TOOLCHAINS", "")).split(",") if t.strip()}


def check_workflow(wf, tiers, ledger_keys, budget_calls=None):
    """Findings about csharp-tier2.yml. `wf` is the parsed workflow (None if the file is absent), `ledger_keys` the set of job keys
    that have a line in scripts/skip-ledger.d/, `budget_calls` the job keys its run steps pass to check-skip-budget.sh."""
    if wf is None:
        return [f"{WORKFLOW} is missing: nothing in CI declares the C# toolchain to tests/csharp, so a runner without docker would "
                f"skip every build test and stay green"]
    findings = []
    jobs = wf.get("jobs") or {}
    for key in (WF_X86, WF_ARM):
        if key not in jobs:
            findings.append(f"{WORKFLOW} has no job {key!r}")
    x86, arm = jobs.get(WF_X86), jobs.get(WF_ARM)
    if x86 is not None:
        if "csharp" not in _declared(x86):
            findings.append(f"job {WF_X86} does not declare csharp in CLEAT_REQUIRE_TOOLCHAINS, so a missing docker is a skip there, not a failure")
        if "csharp-arm64" in _declared(x86):
            findings.append(f"job {WF_X86} declares csharp-arm64, but an x86_64 runner cannot execute the arm64 image: the leg would FAIL every run")
        if "arm" in str(x86.get("runs-on", "")).lower():
            findings.append(f"job {WF_X86} runs on {x86.get('runs-on')!r}, an arm runner: the amd64 lock leg would be the one skipped")
    if arm is not None:
        if not {"csharp", "csharp-arm64"} <= _declared(arm):
            findings.append(f"job {WF_ARM} must declare csharp AND csharp-arm64 in CLEAT_REQUIRE_TOOLCHAINS, so an arm64 leg that cannot run FAILS "
                            f"instead of skipping; it declares {sorted(_declared(arm))}")
        if "arm" not in str(arm.get("runs-on", "")).lower():
            findings.append(f"job {WF_ARM} runs on {arm.get('runs-on')!r}, which is not an arm runner: its arm64 claim would be emulated or false")
    on = wf.get(True, wf.get("on")) or {}
    if "merge_group" in on:
        findings.append(f"{WORKFLOW} has a merge_group trigger: its `paths:` filter does not apply there, so both jobs would run in the "
                        f"queue for every PR (the decision, recorded in its header, is PR only)")
    names = {j.get("name") for j in (x86, arm) if j}
    for c in (tiers.get("required_contexts") or {}).get("contexts") or []:
        if c.get("workflow") == "csharp-tier2.yml" or c.get("context") in names:
            findings.append(f"tiers.yaml makes {c.get('context')!r} a required context, but {WORKFLOW} is behind a workflow-level paths filter: "
                            f"a required context that never reports blocks every PR that touches no C#")
    for key in (WF_X86, WF_ARM):
        if key in jobs and key not in ledger_keys:
            findings.append(f"no {LEDGER_DIR}/*.tsv line has the job key {key!r}: its one expected skip is not declared, so the budget check fails")
        if budget_calls is not None and key in jobs and key not in budget_calls:
            findings.append(f"job {key} never runs scripts/check-skip-budget.sh {key}: a skip that should not happen would pass")
    return findings


def workflow_inputs(root):
    """(parsed workflow or None, ledger job keys, job keys passed to check-skip-budget.sh)."""
    path = os.path.join(root, WORKFLOW)
    wf = None
    if os.path.exists(path):
        try:
            wf = yaml.safe_load(open(path))
        except yaml.YAMLError as e:
            raise Unmeasured(f"{WORKFLOW} does not parse as YAML: {str(e)[:120]}")
    keys = set()
    d = os.path.join(root, LEDGER_DIR)
    if not os.path.isdir(d):
        raise Unmeasured(f"{LEDGER_DIR}/ does not exist, so the skip budgets cannot be read")
    for name in sorted(os.listdir(d)):
        if name.endswith(".tsv"):
            for line in open(os.path.join(d, name)):
                f = line.rstrip("\n").split("\t")
                if len(f) == 4 and not line.startswith("#"):
                    keys.add(f[0])
    calls = set()
    for job in ((wf or {}).get("jobs") or {}).values():
        for step in job.get("steps") or []:
            for m in re.finditer(r"check-skip-budget\.sh\s+(\S+)", str(step.get("run", ""))):
                calls.add(m.group(1))
    return wf, keys, calls


def go_resolver(root):
    def resolve(patterns):
        if not patterns:
            return set()
        try:
            out = subprocess.run(["go", "list"] + list(patterns), cwd=root, capture_output=True, text=True)
        except FileNotFoundError:
            raise Unmeasured("`go` is not on PATH; the check cannot ask which packages a tier-1 pattern selects")
        if out.returncode != 0:
            raise Unmeasured(f"`go list` failed: {out.stderr.strip()[:200]}")
        return {l for l in out.stdout.split("\n") if l}
    # The package's own import path, from go itself. `go list -m` is not usable here: in this repo's workspace it prints EVERY
    # module, one per line.
    own = resolve(["./" + CS_PKG])
    if len(own) != 1:
        raise Unmeasured(f"`go list ./{CS_PKG}` returned {sorted(own)}, want exactly one package")
    return resolve, next(iter(own))


def repo_inputs(root):
    tiers = yaml.safe_load(open(os.path.join(root, "tiers.yaml")))
    # git ls-files, not a directory walk: a walk descends into .claude/worktrees/ (a second copy of the repo) and attributes a
    # test to a package that exists only in a scratch checkout.
    out = subprocess.run(["git", "ls-files", "*_test.go"], cwd=root, capture_output=True, text=True)
    if out.returncode != 0:
        raise Unmeasured(f"git ls-files failed: {out.stderr.strip()}")
    files = {}
    for p in out.stdout.split("\n"):
        if p:
            try:
                files[p] = open(os.path.join(root, p)).read()
            except OSError:
                pass  # a tracked file deleted in the working tree: nothing to read
    if not files:
        raise Unmeasured("git ls-files found no *_test.go files")
    return tiers, files


# ---------------------------------------------------------------------------------------------
# --self-test: the rule against a case that must be refused and one that must pass.

_MOD = "example.com/m"
_UNIVERSE = ["engine", "cmd/cleat", "plugins/x", "tests/integrity", "tests/csharp", "tests/csharpish"]


def _fake_resolve(patterns):
    """Mimics `go list` for the pattern spellings that matter: `./a/...` (a and below), `./a` or `./a/` (exactly a), and
    `./a/pre...` (a wildcard over the final element)."""
    out = set()
    for p in patterns:
        q = p[2:] if p.startswith("./") else p
        if q.endswith("/..."):
            base = q[:-4]
            out |= {f"{_MOD}/{d}" for d in _UNIVERSE if d == base or d.startswith(base + "/")}
        elif q.endswith("..."):
            pre = q[:-3]
            out |= {f"{_MOD}/{d}" for d in _UNIVERSE if d.startswith(pre)}
        else:
            q = q.rstrip("/")
            out |= {f"{_MOD}/{d}" for d in _UNIVERSE if d == q}
    return out


_CS = _MOD + "/" + CS_PKG
_TIERS = {
    "tier1": {"languages": ["go", "python"], "packages": ["./engine/...", "./cmd/..."]},
    "tier2": {"packages": ["./plugins/...", CS_PATTERN]},
}
_TESTS = {"tests/csharp/a_test.go": "package csharp\nfunc TestCSharpLabel(t *testing.T) {}\n",
          "engine/x_test.go": "package engine\nfunc TestSomething(t *testing.T) {}\n"}


def self_test():
    bad = 0

    def expect(name, findings, want):
        nonlocal bad
        ok = (want is None and not findings) or (want is not None and any(want in f for f in findings))
        print(f"  {'ok  ' if ok else 'FAIL'}  {name}" + ("" if ok else f"   got: {findings}"))
        bad += 0 if ok else 1

    def expect_unmeasured(name, fn):
        nonlocal bad
        try:
            fn()
        except Unmeasured as e:
            print(f"  ok    {name}   ({str(e)[:90]})")
            return
        print(f"  FAIL  {name}   did not raise Unmeasured")
        bad += 1

    def place(t=_TIERS, f=_TESTS, exempt=None):
        return check_placement(t, f, _fake_resolve, _CS, exempt)

    print("-- negative control")
    expect("a consistent tree passes", place(), None)

    print("-- placement")
    for nm in ("TestCSharpThing", "TestDotNetThing", "TestDotnetThing"):
        f = dict(_TESTS); f["engine/zz_test.go"] = f"package engine\nfunc {nm}(t *testing.T) {{}}\n"
        expect(f"known-positive: {nm} outside tests/csharp/", place(f=f), "engine/zz_test.go")
    for word in ("componentize-dotnet", "componentizedotnet", "Componentize.DotNet", "BytecodeAlliance.Componentize.DotNet.Wasm.SDK",
                 "componentize_dotnet", "ILCompiler", "dotnet-experimental", "NuGet",
                 "mcr.microsoft.com/dotnet/sdk:10.0"):
        f = dict(_TESTS); f["engine/zz_test.go"] = f"package engine\n// builds with {word}\nfunc TestBuilds(t *testing.T) {{}}\n"
        expect(f"known-positive: an unprefixed test that names the toolchain ('{word}')", place(f=f), "names the C# toolchain")
    f = dict(_TESTS); f["engine/unexempted_test.go"] = "package engine\n// names componentize-dotnet\nfunc TestX(t *testing.T) {}\n"
    expect("known-positive: the SAME text at a path with no exemption is refused", place(f=f), "names the C# toolchain")
    expect("negative control: the same text at an exempted path passes", place(f=f, exempt={"engine/unexempted_test.go": "r"}), None)
    f = dict(_TESTS); f["engine/prebuilt_test.go"] = "package engine\nfunc TestCSharpReadsAPrebuiltFixture(t *testing.T) {}\n"
    expect("known-positive: a TestCSharp* name outside tests/csharp/ with no exemption", place(f=f), "engine/prebuilt_test.go")
    expect("negative control: the same name at a path exempted FOR THE NAME check passes",
           place(f=f, exempt={"engine/prebuilt_test.go": "reads a checked-in .wasm; cannot skip"}), None)
    f2 = dict(f); f2["engine/other_test.go"] = "package engine\nfunc TestCSharpAnother(t *testing.T) {}\n"
    expect("known-positive: exempting one path does not exempt its neighbour", place(f=f2, exempt={"engine/prebuilt_test.go": "r"}),
           "engine/other_test.go")
    expect_unmeasured("UNMEASURED: an exemption names a path the scan did not see",
                      lambda: check_exemptions(_TESTS, {"engine/gone_test.go": "stale"}))
    f = dict(_TESTS); f["internal/other/dotnet_test.go"] = "package other\n// dotnet is a word here\nfunc TestParsesRuntimeNames(t *testing.T) {}\n"
    expect("negative control: the bare word 'dotnet' in a comment is not the toolchain", place(f=f), None)
    f = dict(_TESTS); f["cmd/cleat/csharp_target_test.go"] = "package main\nfunc TestRunBuild_CSharpTarget_Refused(t *testing.T) {}\n"
    expect("negative control: a name with CSharp NOT at the start is not a C# guest test", place(f=f), None)
    t = copy.deepcopy(_TIERS); t["tier2"]["packages"].remove(CS_PATTERN)
    expect("known-positive: package missing from tier2.packages", place(t=t), "does not list")
    for pat in ("./tests/...", "./tests/csharp", "./tests/csharp/", "./tests/cshar...", "./tests/csharp/..."):
        t = copy.deepcopy(_TIERS); t["tier1"]["packages"].append(pat)
        expect(f"known-positive: tier 1 gains '{pat}'", place(t=t), "selects the C# package")
    for pat in ("./tests/integrity/...",):
        t = copy.deepcopy(_TIERS); t["tier1"]["packages"].append(pat)
        expect(f"negative control: tier 1 gains '{pat}', which does not reach it", place(t=t), None)
    t = copy.deepcopy(_TIERS); t["tier2"]["packages"] = ["./plugins/...", "./tests/csharp...", CS_PATTERN]
    expect("negative control: an extra tier-2 pattern that over-selects does not change what the exact pattern resolves to",
           place(t=t), None)
    expect_unmeasured("UNMEASURED: tests/csharp/ holds no TestCSharp* test",
                      lambda: place(f={"engine/x_test.go": _TESTS["engine/x_test.go"]}))
    t = copy.deepcopy(_TIERS); t["tier1"]["packages"] = ["./nothing/..."]
    expect_unmeasured("UNMEASURED: tier-1 resolution cannot show engine (the positive control)", lambda: place(t=t))

    print("-- the CI workflow")
    good = {True: {"pull_request": {}, "workflow_dispatch": None},
            "jobs": {WF_X86: {"runs-on": "ubuntu-latest", "name": "C# Tier 2 (Linux x86_64)", "env": {"CLEAT_REQUIRE_TOOLCHAINS": "csharp"}},
                     WF_ARM: {"runs-on": "ubuntu-24.04-arm", "name": "C# Tier 2 (Linux arm64)",
                              "env": {"CLEAT_REQUIRE_TOOLCHAINS": "csharp,csharp-arm64"}}}}
    keys = {WF_X86, WF_ARM}
    calls = {WF_X86, WF_ARM}
    tiers0 = {"required_contexts": {"contexts": [{"context": "Tier 2 Gate", "workflow": "tier2-gate.yml"}]}}

    def wfc(wf=good, tiers=tiers0, keys=keys, calls=calls):
        return check_workflow(wf, tiers, keys, calls)

    def mut(fn):
        w = copy.deepcopy(good)
        fn(w)
        return w

    expect("negative control: a consistent workflow passes", wfc(), None)
    expect("known-positive: the workflow file is missing", wfc(wf=None), "is missing")
    expect("known-positive: the x86_64 job stops declaring csharp",
           wfc(wf=mut(lambda w: w["jobs"][WF_X86]["env"].update(CLEAT_REQUIRE_TOOLCHAINS=""))), f"job {WF_X86} does not declare csharp")
    expect("known-positive: the declaration is deleted from the x86_64 job altogether",
           wfc(wf=mut(lambda w: w["jobs"][WF_X86].pop("env"))), f"job {WF_X86} does not declare csharp")
    expect("known-positive: the arm64 job stops declaring csharp-arm64",
           wfc(wf=mut(lambda w: w["jobs"][WF_ARM]["env"].update(CLEAT_REQUIRE_TOOLCHAINS="csharp"))), "csharp-arm64")
    expect("known-positive: the x86_64 job declares csharp-arm64, which it cannot execute",
           wfc(wf=mut(lambda w: w["jobs"][WF_X86]["env"].update(CLEAT_REQUIRE_TOOLCHAINS="csharp,csharp-arm64"))), "would FAIL every run")
    expect("known-positive: the arm64 job moves to an x86_64 runner",
           wfc(wf=mut(lambda w: w["jobs"][WF_ARM].update({"runs-on": "ubuntu-latest"}))), "not an arm runner")
    expect("known-positive: the x86_64 job moves to an arm runner",
           wfc(wf=mut(lambda w: w["jobs"][WF_X86].update({"runs-on": "ubuntu-24.04-arm"}))), "an arm runner")
    expect("known-positive: a job is removed", wfc(wf=mut(lambda w: w["jobs"].pop(WF_ARM))), f"no job {WF_ARM!r}")
    expect("known-positive: a merge_group trigger is added",
           wfc(wf=mut(lambda w: w[True].update({"merge_group": {}}))), "merge_group")
    for ctx in ("C# Tier 2 (Linux arm64)", "C# Tier 2 (Linux x86_64)"):
        t = {"required_contexts": {"contexts": [{"context": ctx, "workflow": "other.yml"}]}}
        expect(f"known-positive: {ctx!r} becomes a required context", wfc(tiers=t), "required context")
    t = {"required_contexts": {"contexts": [{"context": "Whatever", "workflow": "csharp-tier2.yml"}]}}
    expect("known-positive: a required context from this workflow file under another name", wfc(tiers=t), "required context")
    expect("known-positive: a job's skip budget has no ledger fragment", wfc(keys={WF_X86}), f"job key {WF_ARM!r}")
    expect("known-positive: a job never runs the skip-budget check", wfc(calls={WF_X86}), f"never runs scripts/check-skip-budget.sh {WF_ARM}")

    print(f"self-test: {'FAILED' if bad else 'every known-positive refused, every negative control passed'}")
    return 1 if bad else 0


def main():
    if len(sys.argv) > 2 or (len(sys.argv) == 2 and sys.argv[1] != "--self-test"):
        print(__doc__, file=sys.stderr)
        return 2  # a usage error is not a pass and not a finding
    if len(sys.argv) == 2:
        return self_test()
    root = subprocess.run(["git", "rev-parse", "--show-toplevel"], capture_output=True, text=True).stdout.strip() \
        or os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    try:
        tiers, files = repo_inputs(root)
        resolve, cs_pkg_path = go_resolver(root)
        check_exemptions(files, EXEMPT)
        findings = check_placement(tiers, files, resolve, cs_pkg_path)
        wf, keys, calls = workflow_inputs(root)
        findings += check_workflow(wf, tiers, keys, calls)
    except Unmeasured as e:
        print(f"UNMEASURED: {e}\n(this is a failure of the check, not a finding about the tree)", file=sys.stderr)
        return 2
    if findings:
        for f in findings:
            print(f"FAIL: {f}")
        return 1
    print(f"OK: every TestCSharp* function is under {CS_PKG}/, which is in tier2.packages and in no tier-1 pattern "
          f"({sum(len(NAME_PREFIX.findall(s)) for p, s in files.items() if p.startswith(CS_PKG + '/'))} test(s) there); "
          f"{WORKFLOW} declares the toolchain in both jobs")
    return 0


if __name__ == "__main__":
    sys.exit(main())
