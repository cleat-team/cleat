"""Side effect workflow: one frozen value, and a count of how often its function ran.

The fixture for engine/python_side_effect_e2e_test.go (cleat#4113). Every execution is a fresh component instance, so the module-level
counter starts at zero each time: a first run reports ``ran=1``, and a replay that returns the recorded value WITHOUT calling the function
reports ``ran=0``. A replay that called it again would report 1, and one the host failed would report nothing.
"""

import json

from cleat_sdk import HostCalls, cleat_entry

_ran = 0


@cleat_entry("side_effect_workflow")
def side_effect_workflow(h: HostCalls) -> str:
    def compute() -> str:
        global _ran
        _ran += 1
        return f"computed-in-run-{_ran}"

    value = h.side_effect(compute)
    return json.dumps({"value": value, "ran": _ran})
