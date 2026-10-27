"""A workflow that raises an exception whose ``str()`` is empty, for cleat#3433.

``raise KeyError()`` (and others) has no message, so a failure that carried only
``str(exc)`` would be a failed run with nothing to read. The failure's message
therefore names the exception's type as well.
"""

from cleat_sdk import HostCalls, cleat_entry


@cleat_entry("raises_empty")
def raises_empty(h: HostCalls) -> dict:
    raise KeyError()
