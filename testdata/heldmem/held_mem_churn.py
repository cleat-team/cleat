"""MEASUREMENT guest: held_mem_workflow.py plus churn_mb (allocate, touch and DROP one MiB at a time before parking)."""

from cleat_sdk import HostCalls, cleat_entry


@cleat_entry("held_mem")
def held_mem(h: HostCalls, calls: int, mb: int, sleep_ms: int, churn_mb: int = 0, churn_small_mb: int = 0, churn_big_mb: int = 0) -> dict:
    for _ in range(calls):
        h.call("work", "step", {"stage": "step"})
    for _ in range(churn_mb):
        junk = bytearray(b"\x01") * (1 << 20)
        del junk
    for _ in range(churn_small_mb * 64):
        junk = bytearray(b"\x01") * (16 * 1024)
        del junk
    sink = None
    for _ in range(churn_big_mb // 4):
        sink = bytearray(b"\x01") * (4 << 20)
    sink = None
    keep = [bytearray(b"\x01") * (1 << 20) for _ in range(mb)]
    if sleep_ms > 0:
        h.sleep_ms(sleep_ms)
    else:
        h.call("work", "park", {"stage": "park"})
    h.call("work", "done", {"stage": "done"})
    return {"kept": len(keep)}
