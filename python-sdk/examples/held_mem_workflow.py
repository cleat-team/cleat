"""MEASUREMENT guest (cleat#4162, residency memory budget): `calls` durable calls, `mb` MiB of touched live data, then one blocking call named park."""

from cleat_sdk import HostCalls, cleat_entry


@cleat_entry("held_mem")
def held_mem(h: HostCalls, calls: int, mb: int) -> dict:
    for _ in range(calls):
        h.call("work", "step", {"stage": "step"})
    keep = [bytearray(b"\x01") * (1 << 20) for _ in range(mb)]
    h.call("work", "park", {"stage": "park"})
    return {"kept": len(keep)}
