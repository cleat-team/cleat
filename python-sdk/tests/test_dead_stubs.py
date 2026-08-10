"""Which host calls a Python workflow cannot make, pinned.

``host_calls.py`` defines ``_import_*`` functions in two kinds. Most are
placeholders for a binding componentize-py generates from ``wit/cleat.wit``
and are replaced by real WASM FFI at runtime. A few have no WIT interface at
all, so nothing is ever generated and the built component never imports them:
those calls cannot work, in or out of the runtime.

Nothing used to distinguish the two. Three cron stubs sat in the second group
looking exactly like the first, with a message claiming they only needed a
WASM runtime, and no test anywhere disagreed. Those three gained a
durable-cron interface on 2026-08-09 and moved to the first group -- this test
is what noticed, which is the behaviour it was written for.

This does not require the list to be empty. It requires it to be EXACTLY this,
so that a call which quietly loses its binding tomorrow fails here rather than
in a user's workflow. Shrinking it means adding the WIT interface; growing it
has to be deliberate.
"""

import re
from pathlib import Path

HOST_CALLS = Path(__file__).resolve().parents[1] / "cleat_sdk" / "host_calls.py"

# Measured 2026-08-08. Re-derive with the parsing below, or by hand:
#   grep -oE '^def (_import_\w+)\(' cleat_sdk/host_calls.py
#   grep -oE 'as (_import_\w+)'     cleat_sdk/host_calls.py
# Measured 2026-08-09, after durable-cron landed: the three cron calls left this
# set and Python can now make them.
EXPECTED_WITHOUT_WIT_BINDING = {
    # The engine registers cleat_extend_timeout, but no WIT interface exposes
    # it to Python. Same shape the cron calls had until durable-cron was added
    # to wit/cleat.wit -- so the remedy is the same one, if this is ever wanted
    # from a Python workflow.
    "_import_cleat_extend_timeout",
}


def _partition():
    src = HOST_CALLS.read_text()
    from_wit = set(re.findall(r"as (_import_\w+)", src))
    defined_here = set(re.findall(r"^def (_import_\w+)\(", src, re.M))
    return defined_here, from_wit


def test_stubs_without_a_wit_binding_are_exactly_the_known_set():
    defined_here, from_wit = _partition()
    assert from_wit, (
        "no 'as _import_*' aliases found; the parsing has stopped matching "
        "host_calls.py and this test is no longer checking anything"
    )

    without_binding = defined_here - from_wit
    assert without_binding == EXPECTED_WITHOUT_WIT_BINDING, (
        "the set of host calls with no WIT binding changed.\n"
        f"  newly unavailable: {sorted(without_binding - EXPECTED_WITHOUT_WIT_BINDING)}\n"
        f"  newly available:   {sorted(EXPECTED_WITHOUT_WIT_BINDING - without_binding)}\n"
        "A call appearing here can never work in a built component. If that is "
        "intended, add it above with the reason; if it is not, give it an "
        "interface in wit/cleat.wit."
    )


def test_dead_stubs_do_not_blame_the_runtime():
    """The message must not send the reader looking for a runtime problem."""
    src = HOST_CALLS.read_text()
    for name in sorted(EXPECTED_WITHOUT_WIT_BINDING):
        call = name.removeprefix("_import_")
        bad = f'"{call} can only be called within a cleat WASM runtime."'
        assert bad not in src, (
            f"{name} still claims it only needs a WASM runtime, but it has no "
            "WIT binding and cannot work inside one either"
        )
