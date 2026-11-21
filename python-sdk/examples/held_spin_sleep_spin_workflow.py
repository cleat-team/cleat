"""Two calls, a busy loop between them, an optional durable sleep, then a second busy loop.

For cleat#4162 (residency, the execution fence on the component path). The execution fence (--wasm-instance-timeout) is charged for GUEST execution and starts again
when a run is held and wakes, as the next segment of a suspended run's would. A guest that computes most of its fence BEFORE the hold and again AFTER it can only
finish if that restart happens: without it the second loop is killed for the work of both. A guest that spins at once after the wake cannot tell a restart from a
plain re-arm, which is what the earlier row (held_sleep_then_spin_workflow.py) could not.

The first host call gives the run an anchor, and a call after each loop lets a test read how long each took (the gap between two calls is the loop and nothing
else: compiling the component is not in it). The two are read SEPARATELY because they differ: the first loop of a fresh instance pays cold-start costs the second does
not, measured at about twice as long on one machine, so a fence sized from the first alone is loose for the second. The loop lengths and the sleep are INPUTS, because the work is sized on the machine it runs on and the
SDK binds input keys to parameters BY NAME. A sleep of 0 skips the hold, so the same guest is the control that two loops in ONE segment are killed.
"""

from cleat_sdk import HostCalls, cleat_entry


def _spin(n: int) -> None:
    for _ in range(n):
        pass


@cleat_entry("held_spin_sleep_spin")
def held_spin_sleep_spin(h: HostCalls, spin1: int, spin2: int, sleep_ms: int) -> str:
    h.call("payments", "Charge", {"stage": "start"})
    _spin(spin1)
    h.call("payments", "Charge", {"stage": "spun"})
    if sleep_ms > 0:
        h.sleep_ms(sleep_ms)
    _spin(spin2)
    h.call("payments", "Charge", {"stage": "end"})
    return "done"
