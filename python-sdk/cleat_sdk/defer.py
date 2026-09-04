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


# True while ``run_deferred`` is draining the table.
#
# IMPROVEMENT-PLAN §3.35 phase 4. Two things a defer body must not do, both
# measured 2026-09-02 before they were blocked, and both producing a workflow
# that reported SUCCESS with a durable record that could not be honoured:
#
# * registering another defer -- the table is drained BEFORE the first body
#   runs, so the new entry lands in a table nobody walks again, while the host
#   has already written a durable ``defer`` event for it;
# * ``continue_as_new`` -- the host records the event AND the wrapper reports
#   the already-decided result, so the worker stores ``done`` and the
#   continuation is silently never taken.
#
# Both guards run BEFORE the host call. Checking after would leave the durable
# event behind, which is the defect rather than the fix.
_IN_DEFER_PHASE = False


def in_defer_phase() -> bool:
    """Report whether defer bodies are currently running."""
    return _IN_DEFER_PHASE


def defer_phase_refusal(what: str) -> str:
    """The message both refusals carry."""
    return (
        f"cleat: {what} is not allowed from inside a defer body: the defer "
        "table is drained before the first body runs and the workflow's result "
        "is already decided, so this would be recorded durably and never taken "
        "(IMPROVEMENT-PLAN 3.35 phase 4)"
    )


def register_defer(defer_id: str, fn: Callable[[], None]) -> None:
    """Record a defer body under the ID the host minted for it.

    The ID is not decorative -- it is the same one the host recorded in the
    workflow's deferrals map, so a body keyed by anything else would run but
    could never be correlated with what the host thinks it registered.
    """
    _DEFERS.append((defer_id, fn))


def run_deferred(*, propagate_suspend: bool = True) -> int:
    """Run registered defer bodies in LIFO order; return how many ran.

    The table is drained BEFORE the first body runs, which makes this
    idempotent: a second call runs nothing. That matters because a caller
    cannot always tell whether the defers already ran, and cleanup that runs
    twice releases a lock twice or refunds a charge twice.

    LIFO is not cosmetic. A defer releases what the defer before it acquired,
    so running them in registration order unwinds the workflow inside-out.

    An exception in one body does not stop the others and does not disturb the
    workflow's result, which is already decided by the time this runs.

    ``SuspendSentinel`` is the one that depends on WHO is draining, which is
    what *propagate_suspend* selects:

    * The **guest** drains at the end of a workflow that finished
      (``@cleat_entry`` step (e)). A body that suspends there has to win over
      the result: the host has recorded a suspension, and completing anyway
      reports a workflow that is still running as done. So it propagates --
      the default.
    * The **host** drains a workflow that has already suspended, through the
      ``run-deferred`` export. There is no result left to lose, and the
      remaining bodies have already been taken off the table -- propagating
      would consume them without running them, which is the destroyed cleanup
      IMPROVEMENT-PLAN 3.81 measured and 3.106 found again in the
      AssemblyScript SDK. So the drain carries on, and the caller passes
      ``propagate_suspend=False``.

    Rust's ``run_deferred`` makes the second choice unconditionally and has no
    flag, because its host calls return the stop as an ``Err`` a defer body
    can ignore. Python's arrives as an exception, so it has to be caught
    somewhere, and this is the only place that knows which drain is running.
    """
    global _DEFERS, _IN_DEFER_PHASE
    taken = _DEFERS
    _DEFERS = []

    # try/finally, not a pair of assignments: SuspendSentinel propagates out of
    # this function, and a flag left set would make the next segment's first
    # defer_func refuse.
    _IN_DEFER_PHASE = True
    try:
        ran = 0
        for _defer_id, fn in reversed(taken):
            ran += 1
            try:
                fn()
            except SuspendSentinel:
                if propagate_suspend:
                    raise
            except Exception:  # noqa: BLE001 - one bad cleanup must not stop the rest
                pass
        return ran
    finally:
        _IN_DEFER_PHASE = False
