"""Awaiting a child that has not finished — what a Python guest does next.

Fixture for engine/python_pending_child_await_e2e_test.go (cleat#3429).

The host answers a wait on an unfinished child by recording the wait, setting
its suspension, and returning a word the guest is supposed to read as "stop".
The component world types these calls `-> string`, so the word is flattened to
text on the way across. Each entry here follows the await with a durable call
that has a side effect, because the question is not only what the await
returns but whether the workflow RUNS ON past it before the run suspends.
"""

import json

from cleat_sdk import HostCalls, cleat_entry


@cleat_entry("await_child_pending")
def await_child_pending(h: HostCalls, pending: str) -> str:
    got = h.await_child(pending)
    h.call("after", "side_effect", {})
    return json.dumps({"bytes": len(got), "nul_bytes": got.count("\x00")})


@cleat_entry("await_all_children_pending")
def await_all_children_pending(h: HostCalls, pending: str) -> str:
    got = h.await_all_children([pending])
    h.call("after", "side_effect", {})
    return json.dumps({"results": len(got)})


@cleat_entry("await_child_with_defer")
def await_child_with_defer(h: HostCalls, pending: str) -> str:
    """A cleanup, a wait on an unfinished child, then a side effect.

    Replayed as a DEFER SEGMENT (the workflow was terminated while it waited),
    the host must still run the cleanup and must not run the side effect.
    """
    h.defer_func(lambda: h.call("cleanup", "release", {}))
    h.await_child(pending)
    h.call("after", "side_effect", {})
    return "done"


@cleat_entry("defer_drain_survives_a_pending_await")
def defer_drain_survives_a_pending_await(h: HostCalls, pending: str) -> str:
    """The guest's OWN drain, with a defer body that waits on an unfinished child.

    The body fails, so the guest runs its defers itself. Defers run last to
    first: B waits on the pending child and then calls, and A was queued before
    it. If waiting in B stopped the guest outright, neither B's call nor A's
    would happen.
    """
    h.defer_func(lambda: h.call("cleanup", "A", {}))

    def b() -> None:
        h.await_child(pending)
        h.call("cleanup", "B", {})

    h.defer_func(b)
    h.call("work", "body", {})
    raise RuntimeError("boom")


@cleat_entry("defer_body_awaits_children")
def defer_body_awaits_children(h: HostCalls, pending: str) -> str:
    """What a defer body sees when it waits on children that have not finished.

    Reported through a durable call, because the defer body's return value goes
    nowhere. engine/python_pending_child_await_e2e_test.go pins the document.
    """

    def d() -> None:
        child = h.await_child(pending)
        all_results = h.await_all_children([pending])
        try:
            h.await_any_child([pending])
            any_raised = False
        except RuntimeError:
            any_raised = True
        h.call(
            "cleanup",
            "saw",
            {"child": child, "all": len(all_results), "any_raised": any_raised},
        )

    h.defer_func(d)
    return "done"
