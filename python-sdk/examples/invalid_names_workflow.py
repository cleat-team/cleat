"""A Python guest sends a name the core-module host would refuse.

Fixture for engine/python_invalid_names_e2e_test.go (cleat#3436).

Both guest-facing host ABIs read names from the guest. The core-module paths run
``validServiceName`` on them and refuse an empty name or one outside
``[a-zA-Z0-9._-]+`` before any handler sees it. The component path read the same
strings with no check. The Python SDK does not validate them either, so a
Python guest can send anything.
"""

import json

from cleat_sdk import HostCalls, cleat_entry
from cleat_sdk.host_calls import SuspendSentinel


@cleat_entry("probe")
def probe(h: HostCalls, kind: str, name: str) -> str:
    """Make one call with ``name`` in the position the host validates.

    Reports what the SDK returned or raised, so a test can set that beside what
    the host's collaborators were handed.
    """
    try:
        # One entry per host function the component dispatchers validate, one
        # per result shape: result<string, call-failure> (call, plugin_call,
        # child_workflow, await_promise), u64 (signal_workflow, send,
        # resolve_promise, run_detached) and a bare string (create_promise,
        # poll_signal, await_child), which can only be refused by trapping.
        calls = {
            "call": lambda: h.call(name, "op", {}),
            "call_op": lambda: h.call("svc", name, {}),
            "plugin_call": lambda: h.plugin_call(name, "fn", {}),
            "child_workflow": lambda: h.child_workflow(name, {}),
            "await_promise": lambda: h.await_promise(name, 1.0),
            "signal_workflow": lambda: h.signal_workflow(name, "sig", {}),
            "send": lambda: h.send(name, "op", {}),
            "resolve_promise": lambda: h.resolve_promise(name, "v"),
            "run_detached": lambda: h.run_detached(name, "{}"),
            "create_promise": lambda: h.create_promise(name),
            "poll_signal": lambda: h.poll_signal(name),
            "await_child": lambda: h.await_child(name),
            "set_scope_ok": lambda: h.set_scope("", ""),
            "call_valid": lambda: h.call("svc", "op", {}),
        }
        if kind not in calls:
            raise ValueError(f"unknown kind {kind!r}")
        got = calls[kind]()
        if got is None or not isinstance(got, str):
            got = repr(got)
    except SuspendSentinel:
        raise
    except Exception as exc:
        return json.dumps({"raised": type(exc).__name__, "message": str(exc)})
    return json.dumps({"returned": got})


@cleat_entry("defer_probe")
def defer_probe(h: HostCalls) -> str:
    """A defer body sends an invalid name, then a second defer makes a real call.

    Defers run last-in-first-out, so the invalid name goes first. A refusal that
    trapped the guest there would end the drain and lose the cleanup registered
    before it -- the case the host answers ``""`` for instead of trapping.
    """
    h.defer_func(lambda: h.call("cleanup", "after", {}))
    h.defer_func(lambda: h.create_promise("bad name!"))
    return "body done"
