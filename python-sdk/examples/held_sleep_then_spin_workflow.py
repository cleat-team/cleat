"""One outbound call, a durable sleep, then a busy loop that never ends.

For cleat#4162 (residency, the execution fence on the component path). A run held through the sleep wakes into a busy loop that only the execution fence can stop, so
a test can measure how long after the wake the fence stops it. The call before the sleep is what lets the run be held at all (the sleep's deadline is anchored at the
last recorded event).

This guest does NOT show that the fence is restarted at the wake as opposed to re-armed: it uses almost no guest time before the hold (componentize-py pre-initialises
the interpreter at build time), and the restart only differs from a re-arm for a guest that had already used part of its fence.

The sleep length is an input (the SDK binds input keys to parameters BY NAME).
"""

from cleat_sdk import HostCalls, cleat_entry


@cleat_entry("held_sleep_then_spin")
def held_sleep_then_spin(h: HostCalls, sleep_ms: int) -> str:
    h.call("payments", "Charge", {"stage": "before"})
    h.sleep_ms(sleep_ms)
    while True:
        pass
