"""MEASUREMENT guest (cleat#4377, residency memory budget): `calls` durable calls, `mb` MiB of touched live data, then EITHER a durable sleep of `sleep_ms` (real residency) OR one blocking call named park."""

from cleat_sdk import HostCalls, cleat_entry


@cleat_entry("held_mem")
def held_mem(h: HostCalls, calls: int, mb: int, sleep_ms: int) -> dict:
    for _ in range(calls):
        h.call("work", "step", {"stage": "step"})
    keep = [bytearray(b"\x01") * (1 << 20) for _ in range(mb)]
    if sleep_ms > 0:
        h.sleep_ms(sleep_ms)
    else:
        h.call("work", "park", {"stage": "park"})
    h.call("work", "done", {"stage": "done"})
    return {"kept": len(keep)}
