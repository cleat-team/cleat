"""Minimal typed Python workflow: one required integer parameter.

cleat#1981's Python fixture. Built by `cleat build --target python`, which
emits a .schema.json sidecar that the start path validates against.

Uses the PREFERRED decorator form, `@cleat_entry("count_workflow")`, not the
bare `@cleat_entry`. The bare form is broken -- cleat_sdk/entry.py's
dual-form branch passes the decorated function into `_make_entry`'s `name`
slot, so the registry is keyed by the function object rather than by a string
-- and the emitter then dies in json.dumps. Found here while building this
fixture; reported separately rather than worked around silently.
"""

from cleat_sdk import HostCalls, cleat_entry


@cleat_entry("count_workflow")
def count_workflow(h: HostCalls, count: int) -> str:
    h.log(f"count={count}")
    return str(count)
