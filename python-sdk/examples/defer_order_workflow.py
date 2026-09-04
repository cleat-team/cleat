"""Two defers and one body call, for the defer-segment tests.

The Java and Rust SDKs have the same fixture (``defer_order`` in
examples/saga-java-port and examples/rust-workflow). Keeping the three
identical is the point: one host mechanism, one shape of proof.

Both defers are registered BEFORE the body call, deliberately. A stop that
arrives at the body call then has a full defer table behind it, so a segment
that consumes the table on its way out is distinguishable from one that leaves
it for the host to drain -- which is the difference IMPROVEMENT-PLAN 3.81
recorded as a destroyed cleanup.
"""

from cleat_sdk import HostCalls, cleat_entry


@cleat_entry("defer_order")
def defer_order(h: HostCalls) -> dict:
    h.defer_func(lambda: h.call("cleanup", "first", {}))
    h.defer_func(lambda: h.call("cleanup", "second", {}))
    h.call("work", "body", {})
    return {"status": "ok"}
