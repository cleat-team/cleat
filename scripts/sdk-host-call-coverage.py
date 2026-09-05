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
    return set(re.findall(r"^\s{4}pub fn ([a-z_][a-z0-9_]*)\s*\(", src, re.M))


def java_surface() -> set[str]:
    src = _read(ROOT / "crates" / "cleat-java" / "src" / "main" / "java" / "cleat" / "HostCalls.java")
    return set(
        re.findall(r"^    public (?:static )?[\w<>,\[\]\s]+? ([a-zA-Z][A-Za-z0-9]*)\s*\(", src, re.M)
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
        ["examples/rust-workflow/src/*.rs", "tests/plugin-harness/testdata/rustworkflow/src/*.rs", "crates/cleat-sdk/examples/*.rs"],
        lambda n: rf"\b(?:h|host|hc)\.{n}\s*\(",
    ),
    "java": (
        java_surface,
        ["examples/java-workflow/**/*.java", "examples/saga-java-port/**/*.java", "tests/plugin-harness/testdata/javaworkflow/**/*.java"],
        lambda n: rf"\b(?:h|host|hc)\.{n}\s*\(",
    ),
    "assemblyscript": (
        as_surface,
        ["examples/as-workflow/assembly/*.ts", "examples/widget-store-as/assembly/*.ts", "tests/plugin-harness/testdata/asworkflow/assembly/*.ts"],
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


def main() -> int:
    mode = sys.argv[1] if len(sys.argv) > 1 else "--report"
    got = measure()

    for name, d in got.items():
        if d["surface"] == 0:
            print(f"{name}: surface extraction found 0 methods -- the scan is broken", file=sys.stderr)
            return 2

    if mode == "--update":
        BASELINE.write_text(
            json.dumps({k: {"covered": v["covered"], "surface": v["surface"]} for k, v in got.items()}, indent=2) + "\n"
        )
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


if __name__ == "__main__":
    sys.exit(main())
