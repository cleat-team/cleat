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


def wit_name(binding: str) -> str:
    """The WIT function name a `_import_*` SDK binding corresponds to.

    `sdk_bindings` and `wit_funcs` read ONE surface in TWO naming schemes, so a
    set comparison between their raw output is a comparison of disjoint sets --
    and two disjoint sets are consistent whatever the tree does. This is the
    correspondence that makes them comparable at all (cleat#2569). Measured on
    today's tree: all 48 SDK bindings normalise onto 48 of the 49 WIT functions.
    """
    n = binding[len("_import_"):]
    if n.startswith("cleat_"):
        n = "durable_" + n[len("cleat_"):]
    return n.replace("_", "-")


# WIT functions the Python SDK deliberately does not bind. Keyed on the NAME,
# never on a count: `n_sdk == n_wit - 1` would re-encode 48 as a constant and rot
# exactly the way a census does. Each entry is a decision, and a STALE one is a
# defect (checked in main), so this list cannot become a rubber stamp that hides
# the next removal.
DELIBERATELY_UNBOUND = {
    # Removed from every SDK on 2026-08-09; see the AssemblyScript twin recorded
    # in CLAUDE.md. The WIT interface still declares it.
    "durable-register-query-handler",
}


def shadowed_imports(src: str) -> list[str]:
    """Names bound from the WIT bindings and then re-bound at MODULE level.

    cleat#1432. Six stubs sat unguarded at module level, so they ran whatever
    the import did and rebound the name ~3,400 lines below it. Python binds in
    file order, so the stub won and six host calls raised NotImplementedError
    inside the WASM runtime.

    MODULE LEVEL is the whole predicate. The file has 39 stubs under
    `if not _USING_WASM:` and those are correct -- they bind only when the
    import failed. `tree.body` sees the unguarded ones and not the guarded
    ones, which is exactly the distinction, and is why this walks the top level
    rather than ast.walk.
    """
    tree = ast.parse(src)
    aliased = {
        a.asname
        for n in ast.walk(tree)
        if isinstance(n, ast.ImportFrom)
        for a in n.names
        if a.asname and a.asname.startswith("_import_")
    }
    top_level = {
        n.name
        for n in tree.body
        if isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef))
        and n.name.startswith("_import_")
    }
    return sorted(aliased & top_level)


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

    # KNOWN-POSITIVE for cleat#1432: an import re-bound at module level.
    shadow_src = (
        "try:\n"
        "    from wit_world.imports.a import set_scope as _import_set_scope\n"
        "except ImportError:\n"
        "    pass\n"
        "def _import_set_scope(a, b):\n"
        "    raise NotImplementedError\n"
    )
    if shadowed_imports(shadow_src) != ["_import_set_scope"]:
        failures.append(
            "shadowed_imports missed a module-level def rebinding an import: "
            f"{shadowed_imports(shadow_src)}"
        )

    # KNOWN-NEGATIVE, and the one that makes the check usable: the SAME stub
    # under `if not _USING_WASM:` is the correct pattern, used 39 times in the
    # real file. A guard that flagged those would be deleted the day it landed.
    guarded_src = (
        "try:\n"
        "    from wit_world.imports.a import set_scope as _import_set_scope\n"
        "except ImportError:\n"
        "    pass\n"
        "if not _USING_WASM:\n"
        "    def _import_set_scope(a, b):\n"
        "        raise NotImplementedError\n"
    )
    if shadowed_imports(guarded_src):
        failures.append(
            "shadowed_imports flagged a correctly guarded stub: "
            f"{shadowed_imports(guarded_src)}"
        )

    # KNOWN-NEGATIVE: a module-level stub for a name that is NOT imported
    # shadows nothing -- _import_cleat_extend_timeout is exactly this.
    unimported_src = "def _import_cleat_extend_timeout(ms):\n    raise NotImplementedError\n"
    if shadowed_imports(unimported_src):
        failures.append("shadowed_imports flagged a stub that shadows no import")

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

    # cleat#2569: the two extractors read one surface in two naming schemes, so
    # comparing their raw output compares DISJOINT sets -- and disjoint sets agree
    # with any tree, which is the defect this check exists to close.
    for binding, want in (
        ("_import_cleat_poll_work", "durable-poll-work"),
        ("_import_plugin_call", "plugin-call"),
        ("_import_set_query_state", "set-query-state"),
    ):
        if wit_name(binding) != want:
            failures.append(f"wit_name({binding!r}) -> {wit_name(binding)!r}, want {want!r}")

    # The control: if the raw names ever DID intersect, wit_name() would be
    # unnecessary here and the self-test above would prove nothing.
    pair_src = "from w import (\n    call as _import_cleat_call,\n)\n"
    pair_wit = "interface x {\n  durable-call: func();\n}\n"
    raw_sdk, raw_wit = sdk_bindings(pair_src), wit_funcs(pair_wit)
    if raw_sdk & raw_wit:
        failures.append(
            "the raw SDK and WIT names intersect -- the schemes are not disjoint, so "
            "this self-test no longer shows wit_name() is load-bearing"
        )
    if {wit_name(b) for b in raw_sdk} != raw_wit:
        failures.append(
            f"normalised SDK names {sorted({wit_name(b) for b in raw_sdk})} do not match "
            f"the WIT declaration {sorted(raw_wit)}"
        )

    if failures:
        for f in failures:
            print(f"SELF-TEST FAIL: {f}", file=sys.stderr)
        return 1
    print("self-test passed: 13 cases (three known-positive, three known-negative, two "
          "vacuity, five for the wit_name correspondence)")
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

    # cleat#2569: each number was checked against its OWN document and never
    # against the other, so dropping a binding read as a pass once the document
    # it is checked against was updated to match. Compare the SETS, in both
    # directions -- a count cannot say which function went missing.
    bound = {wit_name(b) for b in sdk_bindings(HOST_CALLS.read_text())}
    declared = wit_funcs(WIT.read_text())

    unbound = sorted(declared - bound - DELIBERATELY_UNBOUND)
    if unbound:
        print("ERROR: wit/cleat.wit declares these functions and the Python SDK does "
              "not bind them:", file=sys.stderr)
        for n in unbound:
            print(f"    {n}", file=sys.stderr)
        print("\nRestore the binding, or add the name to DELIBERATELY_UNBOUND in this "
              "script if the removal is intended.", file=sys.stderr)
        fail = 1

    undeclared = sorted(bound - declared)
    if undeclared:
        print("ERROR: the Python SDK binds these and wit/cleat.wit declares no such "
              "function:", file=sys.stderr)
        for n in undeclared:
            print(f"    {n}", file=sys.stderr)
        fail = 1

    # An exemption that no longer exempts anything is the mechanism by which an
    # exemption list becomes a rubber stamp -- so a stale one is itself a failure.
    stale = sorted(DELIBERATELY_UNBOUND & bound)
    if stale:
        print("ERROR: DELIBERATELY_UNBOUND lists functions the SDK does bind, so the "
              "exemption is stale and would hide the next removal:", file=sys.stderr)
        for n in stale:
            print(f"    {n}", file=sys.stderr)
        fail = 1

    # cleat#1432, checked before the documented numbers because a shadowed
    # import is a live defect while a stale number is a wrong sentence.
    shadowed = shadowed_imports(HOST_CALLS.read_text())
    if shadowed:
        print("ERROR: these names are imported from the WIT bindings and then re-bound "
              "by a MODULE-LEVEL stub, which wins because Python binds in file order. "
              "They will raise NotImplementedError inside the WASM runtime:",
              file=sys.stderr)
        for n in shadowed:
            print(f"    {n}", file=sys.stderr)
        print("\nPut the stub under `if not _USING_WASM:`, as the other 39 in that file "
              "are. Do not delete it -- the name would be unbound when the import fails, "
              "turning NotImplementedError into NameError on the path the fallback exists "
              "for. cleat#1432.", file=sys.stderr)
        fail = 1

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

    exc = ", ".join(sorted(DELIBERATELY_UNBOUND)) or "none"
    print(f"OK: the Python SDK binds {n_sdk} of the {n_wit} functions wit/cleat.wit "
          f"declares, and every documented number agrees.")
    print(f"    deliberate exceptions (declared, not bound): {exc}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
