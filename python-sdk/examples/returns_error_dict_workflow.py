"""A workflow that RETURNS a dictionary with an ``error`` key, for cleat#3433.

This is a result, not a failure, and it must stay one. It is the control for
``raises_workflow.py``: a host that told a failure from a result by sniffing for
an ``{"error": ...}`` shape would fail this run, which is why that approach was
rejected. The run completes, and its output is this dictionary.
"""

from cleat_sdk import HostCalls, cleat_entry


@cleat_entry("returns_error_dict")
def returns_error_dict(h: HostCalls) -> dict:
    return {"error": "a result that happens to say error"}
