"""A defer, then a pure-CPU loop that never calls the host. For the kill-then-defers tests.

The shape of ``spin_workflow.py`` with one defer registered first. The loop makes no host call after the registration, so
nothing the host does at a call boundary can stop it: only the epoch fence can, and a fence kill traps the instance. A
component instance cannot be re-entered after a trap, so what the engine does AFTER the kill is the thing under test.
"""

from cleat_sdk import HostCalls, cleat_entry


@cleat_entry("spin_defer_workflow")
def spin_defer_workflow(h: HostCalls, request: dict) -> str:
    h.defer_func(lambda: h.call("cleanup", "first", {}))
    n = 0
    while True:
        n = (n + 1) % 1000003
