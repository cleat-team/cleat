"""One durable call to a service that takes a long time, and nothing else (cleat#4174).

What ends the run is what the host does while it waits for the service, so a run that is killed here is killed by a limit on that wait.
"""

from cleat_sdk import HostCalls, cleat_entry


@cleat_entry("slow_call")
def slow_call(h: HostCalls, request: dict) -> str:
    h.call("slow", "wait", {})
    return "completed"


@cleat_entry("slow_call_then_spin")
def slow_call_then_spin(h: HostCalls, request: dict) -> str:
    """NEGATIVE CONTROL: a host call, then a guest-side tight loop. The fence must still kill the loop after the call returns."""
    h.call("slow", "wait", {})
    while True:
        pass


@cleat_entry("slow_plugin_call")
def slow_plugin_call(h: HostCalls, request: dict) -> str:
    """One plugin call that takes a long time (an AI-call shape)."""
    h.plugin_call("slow", "wait", {})
    return "completed"


@cleat_entry("slow_plugin_stream")
def slow_plugin_stream(h: HostCalls, request: dict) -> str:
    """One STREAMING plugin call whose stream takes a long time to produce and to finish (the other AI-call shape)."""
    n = 0
    for _ in h.plugin_call_streaming("slowstream", "run", {}):
        n += 1
    return f"completed {n}"
