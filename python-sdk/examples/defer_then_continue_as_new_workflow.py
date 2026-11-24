"""A defer registered before continue_as_new, in the SDK docstring's own pattern (cleat#4197).

continue_as_new ENDS the run, so its cleanup belongs to it and must run exactly once. The workflow raises SuspendSentinel after it, as the SDK says to,
and a component guest does not drain on that path by itself: the host drain is the only thing that runs the cleanup.
"""

from cleat_sdk import HostCalls, SuspendSentinel, cleat_entry


@cleat_entry("defer_then_continue_as_new")
def defer_then_continue_as_new(h: HostCalls) -> str:
    h.defer_func(lambda: h.call("svc", "cleanup", {}))
    h.call("svc", "before", {})
    h.continue_as_new({})
    raise SuspendSentinel()
