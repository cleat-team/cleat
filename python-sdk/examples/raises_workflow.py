"""A workflow that registers one defer and then raises, for cleat#3433.

A workflow body that raises must FAIL the run, as it does in every other SDK.
Until cleat#3433 the entry wrapper caught the exception and returned
``{"error": "boom"}`` as an ordinary completed result, so the engine recorded a
success and nothing downstream (retry, dead-lettering, a parent awaiting this
run as a child) saw a failure.

The defer is here to pin that a failing run still does its cleanup, once. The
wrapper runs the registered defers on the error path, so they have run by the
time the host sees the failure.
"""

from cleat_sdk import HostCalls, cleat_entry


@cleat_entry("raises")
def raises(h: HostCalls) -> dict:
    h.defer_func(lambda: h.call("cleanup", "ran", {}))
    raise ValueError("boom")
