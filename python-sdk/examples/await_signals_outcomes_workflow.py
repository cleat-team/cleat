"""await_signals and get_scope -- what a guest receives, byte for byte.

Fixture for engine/python_await_signals_e2e_test.go and the worker test beside it (cleat#3786).

``durable-await-signals`` and ``get-scope`` used to return a packed ``u64`` of LENGTHS and ask the guest to read the
strings from memory the host had been told to write. A component host cannot dereference a guest pointer, so the host
wrote into a scratch slice of its own and dropped it: the guest got the right lengths and NUL bytes
(``name="\\x00\\x00"``, a 17-byte payload of ``\\x00``). Both imports now return lifted values, and these entries report
exactly what the SDK handed back -- as hex as well as text, so a test can tell NULs from the real thing without a JSON
encoder or a database in the way -- then make a durable call, so a test can tell a guest that STOPPED at the wait from
one that ran on past it.
"""

import json

from cleat_sdk import HostCalls, cleat_entry
from cleat_sdk.host_calls import SuspendSentinel


@cleat_entry("await_signal")
def await_signal(h: HostCalls, signal_name: str, timeout_ms: int) -> str:
    """Wait for ``signal_name`` and report what the SDK handed back.

    A host failure is caught and reported by class and message so a test can assert on it; a stop is NOT caught
    (``SuspendSentinel`` must unwind).
    """
    try:
        got = h.await_signals_ms([signal_name], timeout_ms)
    except SuspendSentinel:
        raise
    except Exception as exc:
        return json.dumps({"raised": type(exc).__name__, "message": str(exc)})
    h.call("after", "side_effect", {})
    return json.dumps(
        {
            "name": got.name,
            "payload": got.payload,
            "name_hex": got.name.encode("utf-8").hex(),
            "payload_hex": got.payload.encode("utf-8").hex(),
            "timed_out": got.timed_out,
        }
    )


@cleat_entry("scope_roundtrip")
def scope_roundtrip(h: HostCalls, object_type: str, instance_key: str) -> str:
    """Read the scope, set one, read it again. A host failure is reported, not raised."""
    try:
        before = list(h.get_scope())
        h.set_scope(object_type, instance_key)
        after = list(h.get_scope())
    except Exception as exc:
        return json.dumps({"raised": type(exc).__name__, "message": str(exc)})
    return json.dumps({"before": before, "after": after})


@cleat_entry("signal_echo")
def signal_echo(h: HostCalls, signal_name: str) -> str:
    """The worker-level fixture: wait (no timeout) for a signal and return its name and payload as the result.

    Returned as ordinary text on purpose. Before the fix the NUL bytes in this result made PostgreSQL refuse to
    finalize the run (``unsupported Unicode escape sequence (22P05)``), so the run FAILED instead of finishing with
    a wrong answer; both outcomes are the same bug.
    """
    got = h.await_signals_ms([signal_name], 0)
    return json.dumps({"name": got.name, "payload": got.payload, "timed_out": got.timed_out})


@cleat_entry("signal_payload_result")
def signal_payload_result(h: HostCalls, signal_name: str) -> str:
    """The worker-level fixture for the finalize failure: the payload itself IS the result.

    Before the fix the result was a run of NUL bytes, which PostgreSQL refuses to store; this entry exists so that case
    is measured through a worker rather than inferred from the byte test.
    """
    return h.await_signals_ms([signal_name], 0).payload
