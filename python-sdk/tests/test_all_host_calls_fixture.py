"""The compile fixture must exercise every public HostCalls method.

A host call the fixture never mentions is a host call nobody compiles. This is
the guard the Go side has as TestEveryHostCallIsExercisedByTheCompileFixture,
and its absence here is what made the Python end-to-end test's coverage
one method wide: it compiled durable_call_workflow.py, which calls h.call().

Deliberately cheap -- it reads two files and never invokes componentize-py --
so it runs everywhere the Python SDK tests run, including where the WASM
toolchain is absent. The expensive half is
TestPythonAllHostCallsWorkflowCompiles in engine/, which builds the fixture
for real under the tier-1 gate.

See IMPROVEMENT-PLAN.md 3.205.
"""

from __future__ import annotations

import ast
from pathlib import Path

_ROOT = Path(__file__).resolve().parents[1]
_HOST_CALLS = _ROOT / "cleat_sdk" / "host_calls.py"
_FIXTURE = _ROOT / "examples" / "all_host_calls_workflow.py"


def _public_host_call_methods() -> list[str]:
    """Every public method on class HostCalls, by AST rather than by grep.

    A regex over `def ` here would also pick up module-level helpers and the
    nested defs inside methods, and would miss nothing useful in exchange.
    """
    tree = ast.parse(_HOST_CALLS.read_text())
    for node in tree.body:
        if isinstance(node, ast.ClassDef) and node.name == "HostCalls":
            return sorted(
                f.name
                for f in node.body
                if isinstance(f, (ast.FunctionDef, ast.AsyncFunctionDef))
                and not f.name.startswith("_")
            )
    raise AssertionError(
        "class HostCalls not found in host_calls.py -- this test is scanning "
        "nothing, which would pass vacuously"
    )


def _methods_called_in_fixture() -> set[str]:
    """Method names invoked on the `h` parameter, by AST.

    Matching `h.<name>(` textually would count a name inside a comment or a
    docstring, and this file's docstrings name host calls.
    """
    tree = ast.parse(_FIXTURE.read_text())
    called: set[str] = set()
    for node in ast.walk(tree):
        if not isinstance(node, ast.Call):
            continue
        fn = node.func
        if (
            isinstance(fn, ast.Attribute)
            and isinstance(fn.value, ast.Name)
            and fn.value.id == "h"
        ):
            called.add(fn.attr)
    return called


def test_fixture_calls_every_public_host_call() -> None:
    expected = _public_host_call_methods()
    assert len(expected) > 40, (
        f"only {len(expected)} public HostCalls methods found; the scan is "
        "broken, not the SDK"
    )

    called = _methods_called_in_fixture()
    missing = sorted(set(expected) - called)
    assert not missing, (
        f"examples/all_host_calls_workflow.py does not call "
        f"{len(missing)} host method(s): {missing}\n"
        "Every public HostCalls method must be exercised, or the componentize-py "
        "compile test does not cover it. Add a call inside "
        "_exercise_every_host_call."
    )


def test_fixture_calls_nothing_that_is_not_a_host_call() -> None:
    """A call to a method that does not exist would compile here and fail there.

    The fixture is only meaningful if every `h.<name>` is real; a typo would
    otherwise sit in the file looking like coverage.
    """
    expected = set(_public_host_call_methods())
    called = _methods_called_in_fixture()
    unknown = sorted(called - expected)
    assert not unknown, (
        f"the fixture calls {unknown}, which are not public methods of "
        "HostCalls. Either they were renamed or these are typos -- both make "
        "the coverage count wrong."
    )


def test_fixture_calls_bind_to_the_real_signatures() -> None:
    """Every call in the fixture must match the method's actual signature.

    componentize-py will not catch this: Python binds arguments at call time, so
    a wrong arity compiles into the component happily and fails only when the
    workflow runs. The fixture is never run -- it exists to be compiled -- so
    without this check a call with the wrong number of arguments would sit in
    the file looking like coverage.

    inspect.signature().bind is the same machinery the interpreter uses, so this
    is the real rule rather than a re-implementation of it.
    """
    import inspect

    from cleat_sdk import HostCalls

    tree = ast.parse(_FIXTURE.read_text())
    problems: list[str] = []

    for node in ast.walk(tree):
        if not isinstance(node, ast.Call):
            continue
        fn = node.func
        if not (
            isinstance(fn, ast.Attribute)
            and isinstance(fn.value, ast.Name)
            and fn.value.id == "h"
        ):
            continue

        method = getattr(HostCalls, fn.attr, None)
        if method is None:
            continue  # covered by test_fixture_calls_nothing_that_is_not_a_host_call

        # Placeholder values: bind() checks arity and names, not types.
        args = [None] * len(node.args)
        kwargs = {kw.arg: None for kw in node.keywords if kw.arg is not None}
        has_starstar = any(kw.arg is None for kw in node.keywords)
        try:
            # `self` is unbound here because HostCalls.<name> is a plain
            # function, so supply a placeholder for it.
            inspect.signature(method).bind(None, *args, **kwargs)
        except TypeError as exc:
            if has_starstar:
                continue  # **kwargs spread; bind cannot check it
            problems.append(f"h.{fn.attr}(...) at line {node.lineno}: {exc}")

    assert not problems, (
        "calls in examples/all_host_calls_workflow.py do not match their "
        "HostCalls signatures:\n  " + "\n  ".join(problems)
    )
