"""The Python twin of workflows/bench.ts and gobench/gobench.go: n recorded host calls, then an optional sleep.

side_effect records one history event per call; a call that records nothing would give a 0-event history and make every
language look instantaneous, which typescript_numbers_test.go refuses to measure.
"""
from cleat_sdk import HostCalls, cleat_entry


@cleat_entry("bench_steps")
def bench_steps(h: HostCalls, n: int = 1, sleep_ms: int = 0) -> dict:
    for i in range(n):
        h.side_effect(str(i))
    if sleep_ms > 0:
        h.sleep_ms(sleep_ms)
    return {"sum": n}
