"""LocalHostCalls must expose everything HostCalls does.

A workflow written against ``HostCalls`` runs unchanged under ``LocalHostCalls``
in ``cleat run`` and in tests. A method on one and not the other therefore fails
at the worst possible moment -- the workflow compiles, runs under WASM, and
raises ``AttributeError`` the first time someone runs it locally.

Nothing checked this before. Adding ``await_any_child`` and ``poll_child`` to
``HostCalls`` (IMPROVEMENT-PLAN 3.252) introduced exactly that divergence and
the full test suite stayed green; it was caught by asking the question directly
rather than by anything failing.

The baseline is SHRINK-ONLY. Two methods predate this test and are recorded
rather than fixed, because fixing them is a separate change with its own
reasoning.
"""

import ast
from pathlib import Path

SDK = Path(__file__).resolve().parent.parent / "cleat_sdk"

# Present on HostCalls and absent from LocalHostCalls before this test existed.
# Remove an entry when the method lands; do not add one without a reason.
KNOWN_MISSING = {
    # defer_func takes a callable, and the local host has no segment boundary to
    # defer past -- it would have to run the function immediately, which is not
    # what the name promises.
    "defer_func",
    # host_fetch performs real network I/O. The local host deliberately does not,
    # so the honest options are "raise" or "return a canned response", and which
    # one is right is a decision about what `cleat run` means rather than an
    # omission.
    "host_fetch",
}


def _public_methods(path: Path, cls: str) -> set[str]:
    tree = ast.parse(path.read_text())
    for node in ast.walk(tree):
        if isinstance(node, ast.ClassDef) and node.name == cls:
            return {
                m.name
                for m in node.body
                if isinstance(m, (ast.FunctionDef, ast.AsyncFunctionDef))
                and not m.name.startswith("_")
            }
    raise AssertionError(f"class {cls} not found in {path}")


def test_local_host_mirrors_host_calls() -> None:
    host = _public_methods(SDK / "host_calls.py", "HostCalls")
    local = _public_methods(SDK / "local_host.py", "LocalHostCalls")

    # Guard the extractor: if the AST walk stops finding methods, every
    # assertion below passes vacuously.
    assert len(host) >= 50, (
        f"only {len(host)} public methods found on HostCalls, far below the 66 measured "
        "on 2026-09-07 -- the extractor has stopped matching and this test now checks nothing"
    )

    missing = host - local - KNOWN_MISSING
    assert not missing, (
        f"HostCalls exposes {sorted(missing)}, which LocalHostCalls does not.\n\n"
        "A workflow using one of these runs under WASM and raises AttributeError under "
        "`cleat run` and in tests. Add it to LocalHostCalls, or to KNOWN_MISSING with a "
        "reason if it genuinely cannot exist there."
    )

    # Shrink-only: a name that no longer diverges must leave the baseline, or it
    # stops measuring anything.
    stale = KNOWN_MISSING - (host - local)
    assert not stale, (
        f"KNOWN_MISSING names {sorted(stale)}, which no longer diverge. Remove them: a "
        "baseline that covers something already fixed hides the next regression."
    )
