"""Guest-side defer registry.

IMPROVEMENT-PLAN §3.70 for Go, §3.73 for the other SDKs.

``HostCalls.defer(description)`` sends a *description* across the boundary and
nothing else. The host records that a defer exists; no code anywhere can run it,
because there is no body to run. That was true of every Python workflow while
the SDK's own docstring said the action "runs on workflow exit" and is "executed
in LIFO order, analogous to Python's ``try/finally`` or Go's ``defer``".

``HostCalls.defer_func(fn)`` is the one with a body. The guest runs it itself, in
the instance that holds the closure, at the moment the entry point finishes --
which is what a defer is for.

The ``@cleat_entry`` wrapper drains the table on the success and error paths but
NOT on suspension: a suspended workflow has not exited, its defers are still
pending, and firing them at the first sleep would release locks a workflow that
is about to continue still holds.
"""

from __future__ import annotations

from collections.abc import Callable

from .host_calls import SuspendSentinel

# Registered defer bodies, in registration order. A module-level list is the
# right scope: a WASM guest is one instance running one workflow segment.
_DEFERS: list[tuple[str, Callable[[], None]]] = []


def register_defer(defer_id: str, fn: Callable[[], None]) -> None:
    """Record a defer body under the ID the host minted for it.

    The ID is not decorative -- it is the same one the host recorded in the
    workflow's deferrals map, so a body keyed by anything else would run but
    could never be correlated with what the host thinks it registered.
    """
    _DEFERS.append((defer_id, fn))


def run_deferred() -> int:
    """Run registered defer bodies in LIFO order; return how many ran.

    The table is drained BEFORE the first body runs, which makes this
    idempotent: a second call runs nothing. That matters because a caller
    cannot always tell whether the defers already ran, and cleanup that runs
    twice releases a lock twice or refunds a charge twice.

    LIFO is not cosmetic. A defer releases what the defer before it acquired,
    so running them in registration order unwinds the workflow inside-out.

    An exception in one body does not stop the others and does not disturb the
    workflow's result, which is already decided by the time this runs. The one
    exception is ``SuspendSentinel``, which is not an error: it propagates so
    the entry wrapper sees it and the segment suspends. Swallowing it would
    complete a workflow the host has already recorded as suspended.
    """
    global _DEFERS
    taken = _DEFERS
    _DEFERS = []

    ran = 0
    for _defer_id, fn in reversed(taken):
        ran += 1
        try:
            fn()
        except SuspendSentinel:
            raise
        except Exception:  # noqa: BLE001 - one bad cleanup must not stop the rest
            pass
    return ran
