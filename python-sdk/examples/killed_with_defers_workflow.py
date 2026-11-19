"""Workflows the engine KILLS (the execution fence) with a defer registered. For the cleat#4079 worker tests.

Each entry is one shape of "killed with cleanup owed", so a test can say which shape it is about:

* ``spin_after_call``   a defer, one durable call, then a pure-CPU loop. The stop point is the call.
* ``spin_after_defer``  a defer, then the loop. The stop point is the defer registration itself.
* ``spin_no_defer``     the loop, with nothing registered. Owes no cleanup, so it must fail at once.
* ``raise_with_defer``  a defer, then an ordinary exception. The guest's own wrapper runs the defer on the way out, so this is
                        NOT a kill and must not be routed through a second segment (that would run the cleanup twice).
* ``spin_in_defer``     a defer whose BODY loops forever, one call, then the loop. The segment is itself killed.
* ``exhaust_then_spin`` a defer, a call whose retry policy is exhausted (the service answers 500), then the loop. The engine's own
                        classification calls this run dead-lettered, so a kill here must still end dead_lettered.
* ``exhaust_then_raise`` the same, but ending in an ordinary exception: not a kill. It ends dead_lettered with the guest's own drain,
                        which is NOT a segment, so RetryWorkflow must still be allowed to retry it.
* ``resolve_then_spin`` / ``reject_then_spin`` / ``reply_then_spin``
                        a defer, a promise, one durable call, then the SETTLEMENT (resolve_promise, reject_promise,
                        reply_to_signal), then the loop. The cleanup reports what the guest knew when it ran ("saw-charged" only if
                        the replay got past the call, which it does only when the settlement is a stop point), so a stop placed
                        BEFORE the settlement is told apart from a stop AT it.
* ``charge_then_await_settled`` the control. The promise is settled FIRST, then a call, then an await of that already-settled promise
                        (a read: it records promise_resolved with no Op), then the loop. The await must NOT be the stop point.
"""

import contextlib

from cleat_sdk import HostCalls, RetryPolicy, cleat_entry
from cleat_sdk.host_calls import CleatCallError


def _spin() -> None:
    n = 0
    while True:
        n = (n + 1) % 1000003


@cleat_entry("spin_after_call")
def spin_after_call(h: HostCalls, request: dict) -> str:
    h.defer_func(lambda: h.call("cleanup", "refund", {}))
    h.call("work", "charge", {})
    _spin()
    return ""


@cleat_entry("spin_after_defer")
def spin_after_defer(h: HostCalls, request: dict) -> str:
    h.defer_func(lambda: h.call("cleanup", "first", {}))
    _spin()
    return ""


@cleat_entry("spin_no_defer")
def spin_no_defer(h: HostCalls, request: dict) -> str:
    h.call("work", "charge", {})
    _spin()
    return ""


@cleat_entry("raise_with_defer")
def raise_with_defer(h: HostCalls, request: dict) -> str:
    h.defer_func(lambda: h.call("cleanup", "refund", {}))
    h.call("work", "charge", {})
    raise RuntimeError("the workflow failed on purpose")


@cleat_entry("spin_in_defer")
def spin_in_defer(h: HostCalls, request: dict) -> str:
    h.defer_func(_spin)
    h.call("work", "charge", {})
    _spin()
    return ""


def _exhaust(h: HostCalls) -> None:
    # Narrow on purpose: SuspendSentinel is an Exception, and swallowing it here would hide the engine's stop.
    with contextlib.suppress(CleatCallError):
        h.call_with_retry("work", "flaky", {}, RetryPolicy(max_attempts=2, initial_interval_ms=10, max_interval_ms=10))


@cleat_entry("exhaust_then_spin")
def exhaust_then_spin(h: HostCalls, request: dict) -> str:
    h.defer_func(lambda: h.call("cleanup", "refund", {}))
    _exhaust(h)
    _spin()
    return ""


@cleat_entry("exhaust_then_raise")
def exhaust_then_raise(h: HostCalls, request: dict) -> str:
    h.defer_func(lambda: h.call("cleanup", "refund", {}))
    _exhaust(h)
    raise RuntimeError("the workflow failed on purpose")


def _report(h: HostCalls, effects: list[str]) -> None:
    # What the cleanup saw. The op name is the report, so a test reads it off the service's own log.
    h.call("cleanup", "saw-" + "-".join(effects), {})


@cleat_entry("resolve_then_spin")
def resolve_then_spin(h: HostCalls, request: dict) -> str:
    effects: list[str] = []
    h.defer_func(lambda: _report(h, effects))
    p = h.create_promise("order")
    h.call("work", "charge", {})
    effects.append("charged")
    h.resolve_promise(p, '"done"')
    effects.append("resolved")  # unreachable in a segment: the settlement is refused
    _spin()
    return ""


@cleat_entry("reject_then_spin")
def reject_then_spin(h: HostCalls, request: dict) -> str:
    effects: list[str] = []
    h.defer_func(lambda: _report(h, effects))
    p = h.create_promise("order")
    h.call("work", "charge", {})
    effects.append("charged")
    h.reject_promise(p, "declined")
    effects.append("rejected")
    _spin()
    return ""


@cleat_entry("reply_then_spin")
def reply_then_spin(h: HostCalls, request: dict) -> str:
    effects: list[str] = []
    h.defer_func(lambda: _report(h, effects))
    p = h.create_promise("order")
    h.call("work", "charge", {})
    effects.append("charged")
    h.reply_to_signal(p, '"answer"')  # an SDK method over resolve_promise
    effects.append("replied")
    _spin()
    return ""


@cleat_entry("charge_then_await_settled")
def charge_then_await_settled(h: HostCalls, request: dict) -> str:
    effects: list[str] = []
    h.defer_func(lambda: _report(h, effects))
    p = h.create_promise("order")
    h.resolve_promise(p, '"done"')  # an effect, and a stop point
    effects.append("settled")
    h.call("work", "charge", {})  # the last STOPPABLE event
    effects.append("charged")
    h.await_promise_ms(p, 1000)  # already settled: a read, recorded as promise_resolved with no Op
    _spin()
    return ""
