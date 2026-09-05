#!/usr/bin/env python3
"""Report, per SDK, which host-call methods any workflow fixture actually calls.

A host call no fixture calls is a host call no test compiles and no test runs.
That is how four Go host calls shipped uncompilable (IMPROVEMENT-PLAN.md 3.204)
and how the Python end-to-end test's coverage stayed one method wide (3.205).

Written in Python rather than shell on purpose: the surface extraction needs
multi-line and non-ASCII-safe patterns, and CLAUDE.md records that ugrep, BSD
grep and GNU grep disagree on exactly those. `re` is the same everywhere.

Usage:
  scripts/sdk-host-call-coverage.py            # report
  scripts/sdk-host-call-coverage.py --check    # fail if coverage fell below the baseline
  scripts/sdk-host-call-coverage.py --update   # rewrite the baseline
"""

from __future__ import annotations

import ast
import json
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
BASELINE = ROOT / "scripts" / "sdk-host-call-coverage-baseline.json"


def _read(p: Path) -> str:
    return p.read_text(errors="replace")


def _files(globs: list[str]) -> list[Path]:
    out: list[Path] = []
    for g in globs:
        out.extend(p for p in ROOT.glob(g) if p.is_file() and "node_modules" not in p.parts)
    return out


def go_surface() -> set[str]:
    src = _read(ROOT / "wasm" / "adapter_metadata.go")
    return set(re.findall(r'FieldName:\s*"(\w+)"', src))


def python_surface() -> set[str]:
    tree = ast.parse(_read(ROOT / "python-sdk" / "cleat_sdk" / "host_calls.py"))
    for node in tree.body:
        if isinstance(node, ast.ClassDef) and node.name == "HostCalls":
            return {
                f.name
                for f in node.body
                if isinstance(f, (ast.FunctionDef, ast.AsyncFunctionDef))
                and not f.name.startswith("_")
            }
    return set()


def rust_surface() -> set[str]:
    src = _read(ROOT / "crates" / "cleat-sdk" / "src" / "host_calls.rs")
    # Methods on the HostCalls impl, not free functions or the raw externs.
    # host_calls.rs has exactly one `impl` block, so the four-space indent is
    # the whole of the qualification.
    #
    # NO `\(` ANCHOR after the name. A generic method is `pub fn name<T: ...>(`,
    # and requiring the paren excluded all ten of them -- including
    # cleat_call_with_retry and defer_func, which are the ONLY Rust bindings for
    # DurableCallWithRetry and DurableDeferFunc. Found by WS-3 2026-09-05, and
    # the direction is what made it worth fixing rather than noting: a smaller
    # denominator makes coverage look HIGHER, and a call that cannot be counted
    # reads as a call nobody needs to write. 61 -> 71.
    return set(re.findall(r"^\s{4}pub fn ([a-z_][a-z0-9_]*)", src, re.M))


def java_surface() -> set[str]:
    src = _read(ROOT / "crates" / "cleat-java" / "src" / "main" / "java" / "cleat" / "HostCalls.java")
    # The `.` in the return-type class is load-bearing: a method returning
    # CleatResult<java.util.List<AwaitSignalsResult>> was excluded without it,
    # taking awaitSignalsWithQuorum and awaitSignalsWithQuorumMs with it. 68 -> 70.
    # Same class as the Rust generics bug, found while checking whether it was.
    return set(
        re.findall(r"^    public (?:static )?[\w<>,\[\]\.\s]+? ([a-zA-Z][A-Za-z0-9]*)\s*\(", src, re.M)
    )


def as_surface() -> set[str]:
    src = _read(ROOT / "packages" / "cleat-as" / "assembly" / "host-calls.ts")
    m = re.search(r"^export class HostCalls\b.*?\{", src, re.M | re.S)
    if not m:
        return set()
    rest = src[m.end() :]
    end = re.search(r"^\}", rest, re.M)
    body = rest[: end.start()] if end else rest
    names = set(re.findall(r"^  ([a-zA-Z][A-Za-z0-9]*)\s*\(", body, re.M))
    return names - {"constructor", "if", "for", "while", "return", "switch", "catch"}


# The call pattern below ends `\s*\(`, so it does not match a call that carries
# an explicit type-argument list between the method name and the paren --
# `h.cleat_call_with_retry::<Value, Value>(..)` in Rust, `h.method<T>(..)` in
# AssemblyScript. Measured 2026-09-05 against a loosened pattern over every
# glob in EXECUTED: zero calls are missed today, so this costs no number. It is
# recorded because the zero is an accident. The one turbofish call site in the
# tree is credited anyway, by examples/rust-workflow calling the same method in
# a form the pattern does match; delete or rewrite that example and rust's
# executed count drops by one, reading as a regression in a fixture rather than
# a blind spot in this scanner.
#
# A known-positive, since "zero missed" is otherwise indistinguishable from a
# loosened pattern that matches nothing: over the hostcallsrust globs ALONE,
# strict finds 0 matches for cleat_call_with_retry and loose finds 1.
SDKS = {
    # name: (surface fn, fixture globs, call pattern builder)
    "go": (
        go_surface,
        ["testdata/allhostcalls/*.go", "examples/*/*.go", "tests/plugin-harness/testdata/goworkflow/*.go"],
        lambda n: rf"\bh\.{n}\s*\(",
    ),
    "python": (
        python_surface,
        ["python-sdk/examples/*.py", "tests/plugin-harness/testdata/pythonworkflow/*.py"],
        lambda n: rf"\bh\.{n}\s*\(",
    ),
    "rust": (
        rust_surface,
        ["examples/rust-workflow/src/*.rs", "examples/rust-all-host-calls/src/*.rs", "tests/plugin-harness/testdata/rustworkflow/src/*.rs", "crates/cleat-sdk/examples/*.rs"],
        lambda n: rf"\b(?:h|host|hc)\.{n}\s*\(",
    ),
    "java": (
        java_surface,
        ["examples/java-workflow/**/*.java", "examples/saga-java-port/**/*.java", "tests/plugin-harness/testdata/javaworkflow/**/*.java", "crates/cleat-java/src/test/java/cleat/AllHostCallsCompileTest.java"],
        lambda n: rf"\b(?:h|host|hc)\.{n}\s*\(",
    ),
    "assemblyscript": (
        as_surface,
        ["examples/as-workflow/assembly/*.ts", "examples/widget-store-as/assembly/*.ts", "tests/plugin-harness/testdata/asworkflow/assembly/*.ts", "packages/cleat-as/assembly/__compile__/*.ts"],
        lambda n: rf"\b(?:h|host|hc)\.{n}\s*\(",
    ),
}


# A call whose result goes nowhere. Matched per line: the call opens the
# statement, or its only targets are blanks.
#
# This is syntactic and it is NOT assertion coverage -- it cannot tell a bound
# result that is checked from one that is ignored two lines later. It separates
# exactly one thing: "the fixture compiles this call" from "the fixture does
# something with what came back". WS-3's suggestion, and the reason for it is in
# 3.206 -- the failure it points at is a metric that improves as the thing it
# measures degrades.
_DISCARD_PREFIX = re.compile(r"^\s*(?:let\s+)?(?:_\s*(?:,\s*_\s*)*=\s*)?$")


def _binding(blob: str, pattern: str) -> tuple[int, int]:
    """Return (bound, discarded) call-site counts for one method."""
    bound = discarded = 0
    for m in re.finditer(pattern, blob):
        line_start = blob.rfind("\n", 0, m.start()) + 1
        prefix = blob[line_start : m.start()]
        if _DISCARD_PREFIX.match(prefix):
            discarded += 1
        else:
            bound += 1
    return bound, discarded


def measure() -> dict[str, dict]:
    out: dict[str, dict] = {}
    for name, (surface_fn, globs, pat) in SDKS.items():
        surface = surface_fn()
        blob = "\n".join(_read(p) for p in _files(globs))
        called = set()
        bound_methods = set()
        for m in surface:
            b, d = _binding(blob, pat(m))
            if b or d:
                called.add(m)
            if b:
                bound_methods.add(m)
        out[name] = {
            "surface": len(surface),
            "covered": len(called),
            "bound": len(bound_methods),
            "uncovered": sorted(surface - called),
            "called_but_discarded": sorted(called - bound_methods),
        }
    return out


# Every mode this script accepts. An argument outside this set is an ERROR,
# not a fall-through.
#
# It used to fall through: `mode` was compared against the known names and
# anything else reached the report path and returned 0, so --chekc,
# --check-executd and --this-is-nonsense all printed a plausible table and
# exited successfully. Found by WS-3 2026-09-05, and the reason it matters is
# not the typo at a shell prompt -- it is that CI invokes this script BY NAME
# in a YAML file nobody re-reads. One transposed character there turns a guard
# into a report and the job stays green.
#
# That is this repo's central failure mode living in the argument parser of a
# script whose whole job is to be a guard, and it is the same shape as a -run
# pattern that matches nothing: a wrong invocation indistinguishable from a
# right one.
MODES = {
    "--report",
    "--check",
    "--update",
    "--executed",
    "--check-executed",
    "--update-executed",
}


def main() -> int:
    mode = sys.argv[1] if len(sys.argv) > 1 else "--report"

    if mode not in MODES:
        print(f"unknown mode {mode!r}", file=sys.stderr)
        print(f"usage: {sys.argv[0]} [{' | '.join(sorted(MODES))}]", file=sys.stderr)
        return 2

    # --executed and --check-executed are the A2 modes. They share the ratchet
    # shape of the compile modes above and a SEPARATE baseline key, because the
    # two numbers must not be able to substitute for each other: compile
    # coverage is at 100% on all five SDKs and executed coverage is not close,
    # and a single number would let the first stand in for the second.
    if mode in ("--executed", "--check-executed"):
        problems = (
            check_executed_wiring()
            + check_run_patterns_select_something()
            + check_surface_extraction()
        )
        for p in problems:
            print(f"FAIL {p}", file=sys.stderr)
        ex = measure_executed()
        width = max(len(n) for n in ex)
        for name, d in sorted(ex.items()):
            pct = 100.0 * d["executed"] / d["surface"] if d["surface"] else 0.0
            print(f"  {name:<{width}}  executed {d['executed']:>3}/{d['surface']:<3} {pct:5.1f}%")
        if problems:
            return 1
        if mode == "--executed":
            return 0
        base = json.loads(BASELINE.read_text()) if BASELINE.exists() else {}
        failed = False
        for name, d in sorted(ex.items()):
            want = (base.get(name) or {}).get("executed")
            if want is None:
                print(f"  {name}: no executed baseline yet; run --update-executed")
                continue
            if d["executed"] < want:
                print(
                    f"FAIL {name}: executed coverage fell {want} -> {d['executed']}. "
                    f"A host call that stopped being RUN is the failure this mode exists for; "
                    f"the compile number can stay at 100% while this falls.",
                    file=sys.stderr,
                )
                failed = True
            elif d["executed"] > want:
                # A RISE FAILS TOO, and that is deliberate.
                #
                # WS-3's point, and it is a real gap in a shrink-only ratchet:
                # such a ratchet cannot tell a stale baseline from an accurate
                # one. Once a PR raises executed coverage without recording it,
                # the file says 7 while the tree does 9 and nothing ever says
                # so -- the guard is silent about staleness BY CONSTRUCTION,
                # and an advisory line on a green run is read by nobody.
                #
                # Same discipline as scripts/skip-ledger.tsv, which fails when
                # a line matches FEWER skips than declared as well as more, on
                # identical reasoning: "a line that matches nothing is a grant
                # covering something that is not there".
                #
                # The cost is that a PR which raises coverage must run
                # --update-executed. That is one command, and it is the PR that
                # knows why the number moved.
                print(
                    f"FAIL {name}: executed coverage rose {want} -> {d['executed']}. "
                    f"That is good news and it has to be recorded, or the baseline "
                    f"goes stale and a later fall back to {want} passes unnoticed. "
                    f"Run: scripts/sdk-host-call-coverage.py --update-executed",
                    file=sys.stderr,
                )
                failed = True
        return 1 if failed else 0

    if mode == "--update-executed":
        if check_executed_wiring():
            print("refusing to update: fix the wiring problems above first", file=sys.stderr)
            for p in check_executed_wiring():
                print(f"  {p}", file=sys.stderr)
            return 1
        ex = measure_executed()
        base = json.loads(BASELINE.read_text()) if BASELINE.exists() else {}
        for name, d in ex.items():
            base.setdefault(name, {})["executed"] = d["executed"]
        BASELINE.write_text(json.dumps(base, indent=2) + "\n")
        print(f"wrote executed counts to {BASELINE.relative_to(ROOT)}")
        return 0

    got = measure()

    for name, d in got.items():
        if d["surface"] == 0:
            print(f"{name}: surface extraction found 0 methods -- the scan is broken", file=sys.stderr)
            return 2

    if mode == "--update":
        # Merge rather than rewrite: --update must not drop the executed
        # counts --update-executed wrote. A ratchet that another mode silently
        # resets is not a ratchet.
        base = json.loads(BASELINE.read_text()) if BASELINE.exists() else {}
        for k, v in got.items():
            row = base.setdefault(k, {})
            row["covered"] = v["covered"]
            row["surface"] = v["surface"]
        BASELINE.write_text(json.dumps(base, indent=2) + "\n")
        print(f"wrote {BASELINE.relative_to(ROOT)}")
        return 0

    width = max(len(n) for n in got)
    for name, d in sorted(got.items()):
        pct = 100.0 * d["covered"] / d["surface"]
        print(
            f"  {name:<{width}}  called {d['covered']:>3}/{d['surface']:<3} {pct:5.1f}%"
            f"   result bound {d['bound']:>3}"
        )
        if mode == "--report" and d["uncovered"]:
            print(f"      uncovered: {', '.join(d['uncovered'][:12])}"
                  + (f" … +{len(d['uncovered']) - 12} more" if len(d["uncovered"]) > 12 else ""))

    if mode != "--check":
        return 0

    if not BASELINE.exists():
        print(f"no baseline at {BASELINE}; run --update", file=sys.stderr)
        return 2
    base = json.loads(BASELINE.read_text())
    failed = False
    for name, d in sorted(got.items()):
        want = base.get(name)
        if want is None:
            print(f"FAIL {name}: not in the baseline -- add it with --update", file=sys.stderr)
            failed = True
            continue
        if d["covered"] < want["covered"]:
            print(
                f"FAIL {name}: coverage fell {want['covered']} -> {d['covered']} of {d['surface']}. "
                f"A host call that stopped being exercised is one nothing compiles or runs.",
                file=sys.stderr,
            )
            failed = True
        elif d["covered"] > want["covered"]:
            print(f"  {name}: coverage rose {want['covered']} -> {d['covered']}; run --update to lock it in")
    return 1 if failed else 0


# ---------------------------------------------------------------------------
# Executed coverage (plan item A2)
# ---------------------------------------------------------------------------
#
# Everything above measures whether a fixture CALLS a host method, which is a
# compile-time question answered by parsing source. All five SDKs read 100%
# there, and that number says nothing about whether the call works: every
# binding defect of 2026-09-04/05 compiles.
#
# This measures the subset of that surface reached by a fixture some test
# BUILDS AND RUNS. The compile-only fixtures of §3.204-§3.209 are excluded by
# construction -- they exist to be compiled and are never executed.
#
# THE DECLARATION IS THE HARD PART, and it is why each entry names a test
# rather than only a glob. "This fixture is executed" cannot be read off the
# fixture: it is a property of some test, and of whether any CI job selects
# that test. §3.211 is the case -- a harness sat in the tree, green, passing
# locally, and selected by no CI pattern at all, so it ran nowhere. A declared
# list with no check would have recorded its calls as executed.
#
# So check_executed_wiring() verifies both halves: the named test exists in the
# tree, and at least one workflow's -run pattern actually selects it, matched
# with Python's re rather than by grepping for the name. An alternation that
# mentions a test can still fail to select it.

# INCOMPLETENESS, stated rather than left to be discovered. This list is
# enumerated by hand from the tests that build fixtures, and a fixture nobody
# has declared here reads as unexecuted. That errs low, which is the safe
# direction for a ratchet -- a missing entry cannot manufacture coverage, it can
# only fail to credit some -- but it does mean these numbers are a FLOOR and not
# a census. Adding an entry requires naming the test that runs it, and the
# wiring check then has to agree.
EXECUTED = {
    "go": [
        {
            "globs": ["tests/plugin-harness/testdata/goworkflow/*.go"],
            "test": "TestPluginCalls_Wasm_Go",
            "why": "built by buildGoWorkflowWasm and executed by wasmtest; 17 plugin calls",
        },
        # The engine's own root-level testdata fixtures, built by
        # buildFixtureWasm (engine/host_test.go:1151) and executed.
        #
        # testdata/allhostcalls is deliberately NOT here: it is the
        # compile-only fixture of §3.204, built by
        # TestGeneratedAdapterCompilesForEveryHostCall and never run. Including
        # it would credit 37 executed calls to a fixture that executes none,
        # which is this whole mode's failure case.
        {
            "globs": ["testdata/basic/*.go"],
            "test": "TestGuestReturnedErrorIsNotLabelledATrap",
            "why": "engine/host_test.go:1151 builds testdata/basic and the test executes it",
        },
        {
            "globs": ["testdata/cancelpoll/*.go"],
            "test": "TestCancellationGuestAPIEndToEnd_NotCancelled",
            "why": "the PollCancellation fixture; built and executed",
        },
        {
            "globs": ["testdata/deferfunc/*.go"],
            "test": "TestHostDoesNotRerunDefersAGuestAlreadyRan",
            "why": "the DurableDeferFunc fixture; built and executed",
        },
        {
            "globs": ["testdata/spin/*.go"],
            "test": "TestRealTrapIsStillLabelledATrap",
            "why": "built and executed; the trap-labelling case",
        },
        {
            "globs": ["tests/plugin-harness/testdata/hostcallsgo/*.go"],
            "test": "TestHostCallsGo",
            "why": "the host-call execution harness (A1); 24 wave-1 calls, one invocation each",
        },
    ],
    "python": [
        {
            "globs": ["tests/plugin-harness/testdata/pythonworkflow/*.py"],
            "test": "TestPluginCalls_Wasm_Python",
            "why": "built by buildPythonWorkflowWasm and executed; the componentize-py path",
        },
        {
            # NOT python-sdk/examples/*.py. That glob sweeps in
            # all_host_calls_workflow.py, which is the COMPILE-ONLY fixture of
            # §3.205 -- its 73 calls sit behind `if request.exercise:` and no
            # test sets that flag. Counting it read python as 73/73 executed,
            # i.e. 100%, which would have been the flagship number of this
            # whole measurement and is false. The one glob that must never
            # appear in an executed list is the one added to satisfy a compile
            # guard.
            "globs": [
                "python-sdk/examples/cron_workflow.py",
                "python-sdk/examples/defer_order_workflow.py",
                "python-sdk/examples/durable_call_workflow.py",
                "python-sdk/examples/short_results_workflow.py",
                "python-sdk/examples/spin_workflow.py",
            ],
            "test": "TestPythonWasmEndToEnd",
            "why": "engine/python_wasm_e2e_test.go:426 builds from python-sdk/examples and executes it",
        },
    ],
    "rust": [
        {
            "globs": ["tests/plugin-harness/testdata/rustworkflow/src/*.rs"],
            "test": "TestPluginCalls_Wasm_Rust",
            "why": "built by buildRustWorkflowWasm and executed",
        },
        {
            "globs": ["examples/rust-workflow/src/*.rs"],
            "test": "TestRustWorkflowExecute",
            "why": "engine/rust_workflow_test.go:67 builds examples/rust-workflow and executes it",
        },
        # The host-call execution harness (C2, IMPROVEMENT-PLAN 3.406). 24
        # wave-1 calls, one workflow invocation each, outcomes checked against
        # a recorded table.
        #
        # Two arms make no host call at all: ScheduleCron and ListCrons return
        # `unsupported`, because crates/cleat-sdk/src/host_calls.rs declares
        # neither cleat_schedule_cron nor cleat_list_crons -- the string "cron"
        # appears in it zero times. Crediting them would be this mode's own
        # failure case in miniature: a call counted as executed by a fixture
        # that cannot make it.
        #
        # That leaves 22 arms making calls, and this entry credits 21, not 22.
        # ARMS AND SURFACE METHODS ARE DIFFERENT DENOMINATORS and the gap is in
        # the scanner, not the fixture: the DurableCallWithRetry arm calls
        # `h.cleat_call_with_retry::<Value, Value>(...)`, and the call pattern
        # in SDKS ends `\s*\(`, which a turbofish sits in the middle of. See
        # the note on SDKS. Measured 2026-09-05, this entry's globs alone:
        # 20 credited under the pre-#753 surface, 21 after it.
        {
            "globs": ["tests/plugin-harness/testdata/hostcallsrust/src/*.rs"],
            "test": "TestHostCallsRust",
            "why": "the host-call execution harness (C2); 22 of the 24 wave-1 arms make a call, of which 21 credit a surface method -- the two cron arms have no Rust binding, and the retry arm's turbofish defeats the call pattern",
        },
    ],
    "java": [
        {
            "globs": ["tests/plugin-harness/testdata/javaworkflow/**/*.java"],
            "test": "TestPluginCalls_Wasm_Java",
            "why": "built by buildJavaWorkflowWasm and executed",
        },
        {
            "globs": ["examples/saga-java-port/**/*.java"],
            "test": "TestJavaWorkflowExecute",
            "why": "engine/java_workflow_e2e_test.go:24 builds examples/saga-java-port and executes it",
        },
    ],
    "assemblyscript": [
        {
            "globs": ["tests/plugin-harness/testdata/asworkflow/assembly/*.ts"],
            "test": "TestPluginCalls_Wasm_AS",
            "why": "built by buildASWorkflowWasm and executed",
        },
        {
            "globs": ["examples/as-workflow/assembly/*.ts"],
            "test": "TestAssemblyScriptWorkflowExecute",
            "why": "engine/as_workflow_e2e_test.go:30 builds examples/as-workflow and executes it",
        },
        # The host-call execution harness (C2, IMPROVEMENT-PLAN 3.406). All 24
        # wave-1 calls execute here -- unlike Rust, the AssemblyScript SDK
        # binds both cron imports.
        {
            "globs": ["tests/plugin-harness/testdata/hostcallsas/assembly/*.ts"],
            "test": "TestHostCallsAS",
            "why": "the host-call execution harness (C2); all 24 wave-1 calls, one invocation each",
        },
    ],
}

_GO_TEST = re.compile(r"\bgo test\b([^\n]*)")
_RUN_IN = re.compile(r"-run\s+'([^']+)'")


def _module_of(test_file: Path) -> str:
    """Which Go module a test file belongs to, as a repo-relative dir."""
    d = test_file.parent
    while d != ROOT:
        if (d / "go.mod").exists():
            return str(d.relative_to(ROOT))
        d = d.parent
    return "."


def _invocations() -> list[tuple[str, str, str | None]]:
    """Every `go test` invocation in .github/workflows.

    Returns (workflow, argstring, run-pattern-or-None). Comment lines are
    dropped: a command inside a comment documents a command, it does not run
    one, and counting it would let a test be declared selected on the strength
    of an example.
    """
    out = []
    for wf in sorted((ROOT / ".github" / "workflows").glob("*.yml")):
        # Join shell line-continuations BEFORE scanning.
        #
        # Without this, a `go test` whose -run sits on a continuation line
        # parses as UNFILTERED, and an unfiltered invocation is read as
        # selecting everything -- the permissive direction, so the guard goes
        # quiet exactly where a filter is doing the excluding. That is not
        # hypothetical: the Layer 2 step this guard was written to protect is
        # itself line-continued, so the first version of this function reported
        # a known-unselected test as selected. Caught by a negative control,
        # which is the only thing that could have caught it.
        joined, buf = [], ""
        for line in _read(wf).splitlines():
            if line.lstrip().startswith("#"):
                continue
            stripped = line.rstrip()
            if stripped.endswith("\\"):
                buf += stripped[:-1] + " "
                continue
            joined.append(buf + stripped)
            buf = ""
        if buf:
            joined.append(buf)

        # `./...` is MODULE-RELATIVE, so which module an invocation covers
        # depends on the step's working-directory. Tracking the most recent one
        # is a line-order heuristic and not a YAML parse, but the alternative --
        # treating every `./...` as covering every module -- makes the guard
        # permissive in the direction that silences it: an unfiltered `./...`
        # in ANY workflow would then vouch for a test in a DIFFERENT module.
        # That is the bug this replaced, found by negative control.
        wd = "."
        for line in joined:
            mwd = re.search(r"working-directory:\s*(\S+)", line)
            if mwd:
                wd = mwd.group(1).strip().strip("\"'")
            for m in _GO_TEST.finditer(line):
                args = m.group(1)
                r = _RUN_IN.search(args)
                out.append((wf.name, args, r.group(1) if r else None, wd))
    return out


def _selects(module: str, test: str, invs) -> bool:
    """Is `test`, in Go module `module`, selected by some CI invocation?

    The rule is deliberately asymmetric, because the two axes are not equally
    knowable from a workflow file:

      * The -run FILTER is exact -- matched with Python's re against the test
        name, not grepped for. An alternation that mentions a test can still
        fail to select it.
      * The PATH is not resolvable: ci.yml passes ${{ matrix.package.path }},
        which is a matrix variable. So path coverage is approximated by module:
        an invocation is taken to reach a test if it runs inside that test's
        module.

    The approximation is in the safe direction for the defect this exists to
    catch. §3.211 is a test in tests/plugin-harness, a module whose every CI
    invocation carries a -run filter, so "does any filter match" is decidable
    there and is the whole question. For the root module, ci.yml has unfiltered
    invocations, so its tests are selected and this returns True without
    needing the path.
    """
    for wf, args, pattern, wd in invs:
        covers = False
        if module == ".":
            covers = wd == "." and ("./..." in args or "./" in args)
        else:
            # Either the step runs inside the module, or names its path.
            covers = wd.rstrip("/") == module or module in args
        if not covers:
            continue
        if pattern is None:
            return True
        try:
            if re.search(pattern, test):
                return True
        except re.error:
            continue
    return False


# A -run pattern that selects nothing, and the one case currently open.
#
# This is the OTHER direction of §3.211, and the two catch different defects:
#   * check_executed_wiring: a test exists and no pattern selects it.
#     (#744's harness; TestPluginCallStreaming_Cleattest.)
#   * check_run_patterns_select_something: a pattern names a test that does not
#     exist, so the job runs nothing and exits 0.
#     (CLAUDE.md's TestTenantIsolationAcrossDialects; TestBlobstore_S3.)
# Both are green. Neither the skip budget nor `go vet` nor `go test .` can see
# either, because a test that never runs never skips.
#
# Entries here are a RATCHET and may only shrink. Each needs the reason it is
# still open, so that removing one is a decision.
RUN_PATTERN_EXCEPTIONS = {
    "TestBlobstore_S3": (
        "plugin-harness-ci.yml Layer 4 selects a test that exists nowhere in the "
        "repo, so the job provisions a MinIO service and five CLEAT_TEST_S3_* env "
        "vars that no Go code reads, and has never run a test. Found by WS-2 "
        "2026-09-05. Left open deliberately: whether S3-against-real-MinIO "
        "coverage is wanted is a product question, and the honest options are "
        "'write the test' or 'delete the job', not 'quietly repoint the pattern'."
    ),
}


def check_run_patterns_select_something() -> list[str]:
    """Every -run pattern in CI must select at least one test that exists."""
    problems = []
    listed = subprocess.run(
        ["git", "ls-files", "*_test.go"], cwd=ROOT, capture_output=True, text=True, check=True
    ).stdout.split()
    names = set()
    for f in listed:
        names.update(re.findall(r"^func (Test[A-Za-z0-9_]*)\(", (ROOT / f).read_text(errors="ignore"), re.M))

    for wf, args, pattern, _wd in _invocations():
        if pattern is None:
            continue
        try:
            if any(re.search(pattern, n) for n in names):
                continue
        except re.error:
            problems.append(f"{wf}: -run '{pattern}' is not a valid regex")
            continue
        why = RUN_PATTERN_EXCEPTIONS.get(pattern)
        if why:
            print(f"  known-open: {wf} -run '{pattern}' selects nothing -- {why}")
            continue
        problems.append(
            f"{wf}: -run '{pattern}' selects NO test that exists. The job runs "
            f"nothing and exits 0 -- `go test` prints 'ok ... [no tests to run]'. "
            f"Either the test was renamed or it was never written."
        )
    return problems


# A surface extractor that silently under-counts is the worst failure this
# script has, and it has happened twice.
#
# Both were regexes that required something optional right after the method
# name -- Rust wanted `(` and missed every `pub fn name<T>(`, Java's return-type
# class lacked `.` and missed `java.util.List<...>` returns. Neither errored.
# Both made coverage look HIGHER, because the surface is the denominator, and a
# method that cannot be counted reads as one nobody needs to write.
#
# So each extractor is checked against a deliberately LOOSER parse of the same
# file. The loose parse is not more correct -- it over-matches on purpose. It
# exists to answer "is the strict one missing anything", which is the question
# a passing scan cannot answer about itself.
LOOSE = {
    "rust": (
        "crates/cleat-sdk/src/host_calls.rs",
        r"^\s{4}pub fn ([a-z_][a-z0-9_]*)",
    ),
    "java": (
        "crates/cleat-java/src/main/java/cleat/HostCalls.java",
        r"^    public\s+(?:static\s+)?[\w<>,\[\]\.\s]+?\s+([a-zA-Z][A-Za-z0-9]*)\s*\(",
    ),
    "assemblyscript": (
        "packages/cleat-as/assembly/host-calls.ts",
        r"^  ([a-zA-Z][A-Za-z0-9]*)\s*(?:<[^)]*>)?\s*\(",
    ),
}
_AS_KEYWORDS = {"constructor", "if", "for", "while", "return", "switch", "catch"}


def check_surface_extraction() -> list[str]:
    """Each strict extractor must see everything a looser parse of the same file sees."""
    problems = []
    for sdk, (path, pattern) in LOOSE.items():
        f = ROOT / path
        if not f.exists():
            problems.append(f"{sdk}: {path} is missing; the surface scan has nothing to read")
            continue
        loose = set(re.findall(pattern, _read(f), re.M)) - _AS_KEYWORDS
        strict = SDKS[sdk][0]()
        missed = loose - strict
        if missed:
            problems.append(
                f"{sdk}: the surface scan misses {len(missed)} method(s) a looser parse finds: "
                f"{', '.join(sorted(missed))}. A smaller surface is a smaller DENOMINATOR, so this "
                f"inflates coverage rather than failing loudly. Fix the extractor, do not widen "
                f"the baseline."
            )
        if not strict:
            problems.append(f"{sdk}: surface extraction found 0 methods -- the scan is broken")
    return problems


def check_executed_wiring() -> list[str]:
    """Verify each declared executed fixture is actually run. Returns problems."""
    problems = []
    invs = _invocations()
    # git ls-files, not rglob: the working tree can contain whole COPIES of the
    # repo that are not part of it -- .claude/worktrees/ is one -- and rglob
    # walks into them, so a test gets attributed to a module that exists only
    # in a scratch checkout. Measured: the first version resolved
    # TestPythonWasmEndToEnd to
    # .claude/worktrees/<id>/engine/python_wasm_e2e_test.go and reported it
    # unselected on that basis.
    listed = subprocess.run(
        ["git", "ls-files", "*_test.go"], cwd=ROOT, capture_output=True, text=True, check=True
    ).stdout.split()
    test_files = [ROOT / f for f in listed]
    where: dict[str, Path] = {}
    for f in test_files:
        for name in re.findall(r"^func (Test[A-Za-z0-9_]*)\(", _read(f), re.M):
            where.setdefault(name, f)

    for sdk, entries in EXECUTED.items():
        for e in entries:
            test = e["test"]
            f = where.get(test)
            if f is None:
                problems.append(
                    f"{sdk}: declared executed fixture names test {test}, which "
                    f"exists in no _test.go. A declaration naming a test that is "
                    f"not there is a grant covering nothing."
                )
                continue

            module = _module_of(f)
            if not _selects(module, test, invs):
                problems.append(
                    f"{sdk}: {test} ({f.relative_to(ROOT)}, module {module}) exists but no CI "
                    f"invocation selects it -- every `go test` reaching its module carries a "
                    f"-run filter and none match. This is §3.211: in the tree, green, and run "
                    f"nowhere. {len(invs)} invocations checked."
                )

            missing = [g for g in e["globs"] if not _files([g])]
            if missing:
                problems.append(f"{sdk}: declared executed globs match no files: {missing}")
    return problems


def measure_executed() -> dict[str, dict]:
    out: dict[str, dict] = {}
    for name, (surface_fn, _globs, pat) in SDKS.items():
        surface = surface_fn()
        globs = [g for e in EXECUTED.get(name, []) for g in e["globs"]]
        blob = "\n".join(_read(p) for p in _files(globs))
        called = {m for m in surface if _binding(blob, pat(m)) != (0, 0)}
        out[name] = {
            "surface": len(surface),
            "executed": len(called),
            "unexecuted": sorted(surface - called),
        }
    return out


if __name__ == "__main__":
    sys.exit(main())
