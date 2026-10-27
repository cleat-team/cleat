"""await_promise outcomes -- what a guest receives for each way a wait can end.

Fixture for engine/python_await_promise_outcomes_e2e_test.go (cleat#3425).

``durable-await-promise`` was typed ``-> string``, which flattened every way a
wait can end into one value (see the test file for the measurement). It is now
``result<await-outcome, call-failure>``, and each ending has its own case. The
entry reports exactly what the SDK handed back, then makes a durable call, so a
test can tell a guest that STOPPED at the wait from one that ran on past it.
"""

import json

from cleat_sdk import HostCalls, cleat_entry
from cleat_sdk.host_calls import SuspendSentinel


@cleat_entry("await_outcome")
def await_outcome(h: HostCalls, promise_id: str, timeout_ms: int) -> str:
    """Wait on ``promise_id`` and report what the SDK handed back.

    A host failure is caught and reported by class and message so a test can
    assert on it; a stop is NOT caught (``SuspendSentinel`` must unwind).
    """
    try:
        got = h.await_promise_ms(promise_id, timeout_ms)
    except SuspendSentinel:
        raise
    except Exception as exc:
        return json.dumps({"raised": type(exc).__name__, "message": str(exc)})
    h.call("after", "side_effect", {})
    return json.dumps(
        {"result": got.result, "timed_out": got.timed_out, "rejected": got.rejected}
    )
