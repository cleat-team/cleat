"""Every call to a component binding must pass the arguments the binding takes.

cleat#4081. ``acquire_lock``, ``acquire_lock_ms`` and ``release_lock`` called
``durable_acquire_lock(SCRATCH_BASE, key_len, ttl_ms)`` -- the core-module ABI,
a pointer and a length -- where the generated binding is
``durable_acquire_lock(key: str, ttl_ms: int)``. On a component build each of
the three raised ``TypeError`` before reaching the host, and nothing noticed:
the compile fixture only COMPILES every host call, and Python binds arguments
at call time, so a wrong arity is invisible until the workflow runs.

This is the cheap half of the guard, in the same spirit as
``test_all_host_calls_fixture.py``: it reads the SDK and the generated binding
tree and compares the two, and needs no WASM toolchain. The expensive half is
``TestAPythonWorkflowCanHoldAndReleaseALock`` in ``engine/``, which runs the
lock calls through a real component and is what found the engine-side defect
the same fix exposed.

WHAT IT CHECKS, and what it cannot. It compares the NUMBER of positional
arguments at each call site with the number the binding declares. It does not
compare types, so a call that passes the right count of wrong-kinded values
passes here; ``test_no_binding_call_passes_a_scratch_pointer`` is the second,
differently-wrong reading that catches the specific shape this defect had. The
run in ``engine/`` is the check that sees types.

A call site is either direct, ``_import_x(a, b)``, or by reference through
``_call_or_raise(service, op, _import_x, a, b)``, where everything after the
function is passed to it. Both are counted; a count of direct calls alone would
have skipped sixteen of fifty-five sites.
"""

from __future__ import annotations

import ast
import re
from pathlib import Path

_ROOT = Path(__file__).resolve().parents[1]
_HOST_CALLS = _ROOT / "cleat_sdk" / "host_calls.py"
_BINDINGS = _ROOT / "cleat_sdk" / "_wit" / "wit_world" / "imports"

_PREFIX = "wit_world.imports."


def _binding_signatures() -> tuple[dict[tuple[str, str], int], dict[str, set[str]]]:
    """(module, function) -> positional parameter count, and module -> class names.

    Classes are recorded because an import of a VARIANT CASE
    (``AwaitOutcome_Resolved``) is a class used in ``isinstance``, not a call
    target, and must not read as a binding that went missing.
    """
    sigs: dict[tuple[str, str], int] = {}
    classes: dict[str, set[str]] = {}
    for path in sorted(_BINDINGS.glob("*.py")):
        module = path.stem
        classes[module] = set()
        for node in ast.parse(path.read_text()).body:
            if isinstance(node, ast.FunctionDef):
                sigs[(module, node.name)] = len(node.args.args)
            elif isinstance(node, ast.ClassDef):
                classes[module].add(node.name)
    return sigs, classes


def check_call_arguments(
    source: str,
    sigs: dict[tuple[str, str], int],
    classes: dict[str, set[str]],
) -> tuple[list[str], int, int]:
    """Return (problems, call sites checked, binding aliases found).

    Separate from the tests so a test can hand it a source known to be wrong
    and confirm it says so.
    """
    tree = ast.parse(source)
    problems: list[str] = []
    alias: dict[str, tuple[str, str]] = {}

    for node in ast.walk(tree):
        if not isinstance(node, ast.ImportFrom) or not node.module:
            continue
        if node.module == _PREFIX.rstrip("."):
            # `from wit_world.imports import durable_lock` would make every
            # call below read as an attribute access this scan does not model.
            problems.append(
                f"line {node.lineno}: an import of the form 'from {node.module} import ...' "
                "is not one this guard understands, so calls through it are unchecked"
            )
        elif node.module.startswith(_PREFIX):
            module = node.module[len(_PREFIX):]
            for a in node.names:
                alias[a.asname or a.name] = (module, a.name)

    for name, (module, fn) in sorted(alias.items()):
        if (module, fn) not in sigs and fn not in classes.get(module, set()):
            problems.append(
                f"{name} is imported from {module}.{fn}, which the generated bindings do "
                "not define -- a rename in the WIT that this file did not follow"
            )

    checked = 0
    for node in ast.walk(tree):
        if not isinstance(node, ast.Call):
            continue
        target: str | None = None
        given = 0
        if isinstance(node.func, ast.Name) and node.func.id in alias:
            target, given = node.func.id, len(node.args)
        else:
            for i, arg in enumerate(node.args):
                if isinstance(arg, ast.Name) and arg.id in alias:
                    target, given = arg.id, len(node.args) - i - 1
                    break
        if target is None:
            continue
        key = alias[target]
        if key not in sigs:
            continue  # a variant-case class; reported above if it is also missing
        checked += 1
        if given != sigs[key]:
            problems.append(
                f"line {node.lineno}: {target} -> {key[0]}.{key[1]} takes {sigs[key]} "
                f"positional argument(s), this call passes {given}"
            )
    return problems, checked, len(alias)


def test_every_binding_call_passes_the_arguments_the_binding_takes() -> None:
    sigs, classes = _binding_signatures()
    problems, checked, aliases = check_call_arguments(_HOST_CALLS.read_text(), sigs, classes)

    # THE THIRD OUTCOME. A scan that found no bindings or no call sites would
    # pass on an empty list, which is a green that compared nothing.
    assert len(sigs) > 40, (
        f"only {len(sigs)} binding functions found under {_BINDINGS}; the generated "
        "tree moved or was not committed, and nothing below was measured"
    )
    assert aliases > 40 and checked > 40, (
        f"{aliases} binding alias(es) and {checked} call site(s) found in host_calls.py; "
        "the scan is broken, not the SDK"
    )
    assert not problems, (
        "host_calls.py calls a component binding with the wrong arguments. Python binds "
        "at call time, so the compile fixture cannot see this and the workflow fails when "
        "it runs (cleat#4081):\n  " + "\n  ".join(problems)
    )


def test_the_check_reports_the_defect_it_was_written_for() -> None:
    """A KNOWN-POSITIVE. The rule above is satisfied by every broken guard.

    This is the call as it stood before cleat#4081, in both call forms, against
    the real bindings. If either stops being reported, the test above has
    stopped being able to fail.
    """
    sigs, classes = _binding_signatures()
    bad_direct = (
        "from wit_world.imports.durable_lock import durable_acquire_lock as _acquire\n"
        "def f(key, ttl_ms):\n"
        "    return _acquire(SCRATCH_BASE, len(key), ttl_ms)\n"
    )
    bad_by_reference = (
        "from wit_world.imports.durable_lock import durable_release_lock as _release\n"
        "def f(key):\n"
        "    return _call_or_raise('s', 'o', _release, SCRATCH_BASE, len(key))\n"
    )
    good = (
        "from wit_world.imports.durable_lock import durable_acquire_lock as _acquire\n"
        "def f(key, ttl_ms):\n"
        "    return _acquire(key, ttl_ms)\n"
    )
    for label, src in (("direct", bad_direct), ("by reference", bad_by_reference)):
        problems, checked, _ = check_call_arguments(src, sigs, classes)
        assert checked == 1 and len(problems) == 1, (label, checked, problems)
        assert "takes" in problems[0] and "passes" in problems[0], problems
    problems, checked, _ = check_call_arguments(good, sigs, classes)
    assert (problems, checked) == ([], 1), (problems, checked)


def test_a_missing_binding_is_reported_not_skipped() -> None:
    sigs, classes = _binding_signatures()
    src = "from wit_world.imports.durable_lock import durable_renamed as _x\n_x('k')\n"
    problems, checked, _ = check_call_arguments(src, sigs, classes)
    assert checked == 0 and any("do not define" in p for p in problems), problems


def test_no_binding_call_passes_a_scratch_pointer() -> None:
    """The second reading: the SHAPE of the original defect, ignoring arity.

    A wrapper that handed a binding the right number of core-ABI values would
    pass the count check. Deliberately loose -- it matches names, not meaning --
    so that its only job is to disagree with the strict reading.
    """
    tree = ast.parse(_HOST_CALLS.read_text())
    aliases = {
        (a.asname or a.name)
        for n in ast.walk(tree)
        if isinstance(n, ast.ImportFrom) and n.module and n.module.startswith(_PREFIX)
        for a in n.names
    }
    pointer_like = re.compile(r"SCRATCH|OUT_BUF|\b\w*_ptr\b|\b\w*_len\b|\bptr\b")
    found = [
        f"line {n.lineno}: {ast.unparse(n)[:100]}"
        for n in ast.walk(tree)
        if isinstance(n, ast.Call)
        and any(
            isinstance(x, ast.Name) and x.id in aliases
            for x in [n.func, *n.args]
        )
        and pointer_like.search(ast.unparse(n))
    ]
    assert not found, (
        "a binding call mentions a scratch pointer or a length -- the core-module ABI "
        "(cleat#4081):\n  " + "\n  ".join(found)
    )
