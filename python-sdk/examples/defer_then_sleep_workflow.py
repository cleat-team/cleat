"""A defer registered before a sleep (cleat#4194).

The sleep suspends the run: it is still alive and WAITING, so the registered cleanup must not run then. Before the fix the component path drained a
guest's defer table at every suspension, so this workflow's cleanup ran at its first sleep and the resume diverged at the step the cleanup had taken.
"""

from cleat_sdk import HostCalls, cleat_entry


@cleat_entry("defer_then_sleep")
def defer_then_sleep(h: HostCalls) -> str:
    h.defer_func(lambda: h.call("svc", "cleanup", {}))
    h.call("svc", "before", {})
    h.sleep(5)
    h.call("svc", "after", {})
    return "done"
