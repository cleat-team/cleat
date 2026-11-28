"""One outbound call, a durable sleep, a second outbound call.

For cleat#4162 (residency, component admission). It exists so that a test can hold a REAL Python component through a durable sleep on the worker's own backend and
measure what is held and what it is charged. The sleep length is an INPUT, so that a test can ask for two lengths and show the wall time moves with the request:
a guest that ignores its input still completes and exits cleanly, which is how an earlier measurement guest went unnoticed (the SDK binds input keys to
parameters BY NAME, so a parameter that is not named in the input silently takes its default).

The two calls are what show the run was held and not replayed: replayed, the first call would be answered from history and a counting caller would see one call
per execution instead of both in one.
"""

from cleat_sdk import HostCalls, cleat_entry


@cleat_entry("held_sleep")
def held_sleep(h: HostCalls, sleep_ms: int) -> dict:
    before = h.call("payments", "Charge", {"stage": "before"})
    h.sleep_ms(sleep_ms)
    after = h.call("payments", "Charge", {"stage": "after"})
    return {"sleep_ms": sleep_ms, "before": before, "after": after}
