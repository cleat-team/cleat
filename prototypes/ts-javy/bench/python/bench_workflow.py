"""Phase 4: the Python twin of workflows/bench (n recorded side-effect host calls, then an optional sleep)."""
from cleat_sdk import HostCalls, cleat_entry


@cleat_entry("bench_steps")
def bench_steps(h: HostCalls, n: int = 1, sleep_ms: int = 0) -> dict:
    for i in range(n):
        h.side_effect(str(i))
    if sleep_ms > 0:
        h.sleep_ms(sleep_ms)
    return {"sum": n}
