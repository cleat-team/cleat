"""Calls durable-await-signals and get-scope in the shape they had before cleat#3786.

Both imports took pointers into the guest's linear memory and returned a packed
u64 of lengths. This file calls the generated binding directly, with that
arity, because the SDK's own wrappers now speak the new shape.

It is built only by engine/python_await_signals_stale_component_test.go, against
a copy of the SDK whose WIT has the old declarations put back -- never against
python-sdk/ itself, where this call would not type-check.
"""

from cleat_sdk import cleat_entry
from cleat_sdk.host_calls import HostCalls


@cleat_entry(name="old_await_signals")
def old_await_signals(h: HostCalls) -> dict:
    from wit_world.imports import durable_signals

    word = durable_signals.durable_await_signals('["go"]', 60000, 0, 65536, 65536, 65536)
    # Not reached against a host that refuses the old shape.
    return {"reached": True, "word": word}


@cleat_entry(name="old_get_scope")
def old_get_scope(h: HostCalls) -> dict:
    from wit_world.imports import durable_scope

    word = durable_scope.get_scope(0, 65536, 65536, 65536)
    return {"reached": True, "word": word}
