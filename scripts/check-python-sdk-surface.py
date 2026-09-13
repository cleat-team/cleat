#!/usr/bin/env python3
"""The Python SDK's documented host-call surface must match a derivation.

cleat#1418. Five sites stated a number for "the Python SDK's host-call
surface" and nothing checked any of them:

    python-sdk/README.md       36  (x2)
    python-sdk/TEST_PLAN.md    36  (x2, one of them the ENGINE's number
                                    reached through internal/host/, a path
                                    deleted 2026-06-01)
    python-sdk/wit/README.md   31
    host_calls.py docstring    29  ("module-level _import_* functions")

Derived on 2026-09-13: the SDK binds 48 and the WIT world declares 49.

WHY A SIBLING OF check-host-call-counts.py RATHER THAN A SECTION IN IT. That
guard states, deliberately, that SDK trees are out of its scope because an
SDK's surface is a different denominator from the engine's and legitimately
differs. It is right, and forcing them to agree would be a worse error. This
checks the SDK against ITS OWN derivation instead.

DO NOT TREAT THE FOUR 36s AS CORROBORATION. They are one number copied. This
is the trap CLAUDE.md records for the engine count -- two derivations agreeing
while differing on six members -- and #1414 met it one layer up.

PARSE, DO NOT GREP. A regex for `_import_*` over host_calls.py returns 52.
Three of those are names inside comments stating the import does NOT exist:

    # There is no _import_cleat_register_query_handler here (removed 2026-08-09).

A text search cannot tell a binding from a sentence denying one -- the exact
trap CLAUDE.md records for the AssemblyScript SDK, on one of the same names.

Usage:
  scripts/check-python-sdk-surface.py
  scripts/check-python-sdk-surface.py --self-test
"""

from __future__ import annotations

import argparse
import ast
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
HOST_CALLS = ROOT / "python-sdk" / "cleat_sdk" / "host_calls.py"
WIT = ROOT / "python-sdk" / "wit" / "cleat.wit"


def sdk_bindings(src: str) -> set[str]:
    """Names bound as `_import_*` from the WIT bindings, by AST.

    Pure so --self-test can drive it. An `ast.ImportFrom` alias is a binding; a
    comment mentioning the same name is not, and only a parse can tell them
    apart.
    """
    tree = ast.parse(src)
    return {
        a.asname
        for n in ast.walk(tree)
        if isinstance(n, ast.ImportFrom)
        for a in n.names
        if a.asname and a.asname.startswith("_import_")
    }


def wit_funcs(src: str) -> set[str]:
    """Function names declared in a .wit file, comments stripped first.

    Stripping matters for the same reason as above: a comment naming a func is
    not a declaration. It makes no difference to today's file, which is checked
    rather than assumed -- see --self-test.
    """
    clean = "\n".join(re.sub(r"//.*$", "", line) for line in src.split("\n"))
    return set(re.findall(r"^\s*([a-z0-9-]+)\s*:\s*func", clean, re.M))


def stated(src: str, pattern: str) -> list[int]:
    """Every number a document states for a claim matching `pattern`."""
    return [int(m) for m in re.findall(pattern, src)]


def self_test() -> int:
    failures: list[str] = []

    # KNOWN-POSITIVE: a comment denying an import must not count as one. This
    # is the whole reason the derivation is a parse.
    src = (
        "from wit_world.imports.a import (\n"
        "    real_one as _import_cleat_real_one,\n"
        ")\n"
        "# There is no _import_cleat_ghost here (removed 2026-08-09).\n"
        "def _import_cleat_stub():\n"
        "    raise NotImplementedError\n"
    )
    got = sdk_bindings(src)
    if got != {"_import_cleat_real_one"}:
        failures.append(f"sdk_bindings should see only the aliased import, got {sorted(got)}")
    loose = set(re.findall(r"\b(_import_[a-z0-9_]+)\b", src))
    if got == loose:
        failures.append(
            "the parse agrees with a loose regex on a file containing a denial and a "
            "stub -- the parse is inert"
        )

    # KNOWN-POSITIVE for the WIT side: a commented-out func is not declared.
    wsrc = "interface x {\n  real-func: func() -> string;\n  // ghost-func: func();\n}\n"
    wgot = wit_funcs(wsrc)
    if wgot != {"real-func"}:
        failures.append(f"wit_funcs should see only real-func, got {sorted(wgot)}")

    # Vacuity: an extractor that returns nothing agrees with everything.
    if sdk_bindings("x = 1\n"):
        failures.append("sdk_bindings invented a binding in a file with none")
    if wit_funcs("// nothing here\n"):
        failures.append("wit_funcs invented a declaration in a file with none")

    if failures:
        for f in failures:
            print(f"SELF-TEST FAIL: {f}", file=sys.stderr)
        return 1
    print("self-test passed: 5 cases (two known-positive, one denial, two vacuity)")
    return 0


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--self-test", action="store_true")
    args = ap.parse_args()
    if args.self_test:
        return self_test()

    for p in (HOST_CALLS, WIT):
        if not p.exists():
            print(f"ERROR: {p} is missing -- this guard is reading the wrong tree, "
                  "which is not the same as a clean one.", file=sys.stderr)
            return 2

    n_sdk = len(sdk_bindings(HOST_CALLS.read_text()))
    n_wit = len(wit_funcs(WIT.read_text()))

    if n_sdk == 0 or n_wit == 0:
        print(f"ERROR: derived {n_sdk} SDK bindings and {n_wit} WIT funcs; a zero here "
              "means the extractor broke, not that the surface vanished.", file=sys.stderr)
        return 2

    fail = 0
    readme = (ROOT / "python-sdk" / "README.md").read_text()
    witmd = (ROOT / "python-sdk" / "wit" / "README.md").read_text()

    # \s+ between every token, never a literal space. #1414's census exists
    # because a claim wrapped across two lines and a line-oriented sweep
    # returned EMPTY -- which reads as "no claims to check" rather than "the
    # pattern cannot see this". This guard hit the same thing on its first run,
    # against a sentence written minutes earlier in this very PR.
    claims = [
        ("python-sdk/README.md", readme,
         r"binds\s+(\d+)\s+WASM\s+host\s+function\s+imports"
         r"|binding\s+the\s+(\d+)\s+WASM\s+host\s+function\s+imports",
         n_sdk),
        ("python-sdk/wit/README.md", witmd,
         r"declares\s+\*\*(\d+)\*\*\s+functions", n_wit),
    ]
    for name, src, pat, want in claims:
        found = [int(g) for m in re.findall(pat, src) for g in (m if isinstance(m, tuple) else (m,)) if g]
        if not found:
            print(f"ERROR: {name} states no host-call number in the expected form. "
                  f"Either the sentence was reworded (update this pattern) or the claim "
                  f"was dropped -- both need a human, and 'no claims found' must not "
                  f"read as 'all claims correct'.", file=sys.stderr)
            fail = 1
            continue
        for got in found:
            if got != want:
                print(f"ERROR: {name} says {got}, derived {want}.", file=sys.stderr)
                fail = 1

    if fail:
        print("\nDerive, do not count by hand and do not copy from another document:",
              file=sys.stderr)
        print("  scripts/check-python-sdk-surface.py   (this script prints both numbers)",
              file=sys.stderr)
        return 1

    print(f"OK: the Python SDK binds {n_sdk} host calls and wit/cleat.wit declares "
          f"{n_wit}; every documented number agrees.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
