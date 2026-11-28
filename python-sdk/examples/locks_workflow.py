"""Takes and releases a lock, and reports what the host said each time.

For cleat#4081. ``acquire_lock``, ``acquire_lock_ms`` and ``release_lock`` passed
a core-module argument list -- a scratch-memory pointer and a length -- to the
component binding, which takes the key as a string. On a component build every
one of the three raised ``TypeError`` before reaching the host, so no Python
workflow could hold a lock at all.

The workflow reports the host's answer rather than asserting anything itself,
so the test that runs it can compare each answer with what the store was asked.
"""

from cleat_sdk import HostCalls, cleat_entry


@cleat_entry("locks")
def locks(h: HostCalls) -> dict:
    first = h.acquire_lock("k", 30)
    # Same key, held: the host must say no, which is what makes the first
    # answer mean something -- a binding that returned True for everything
    # would pass the line above.
    second = h.acquire_lock("k", 30)
    ms = h.acquire_lock_ms("k-ms", 45000)
    h.release_lock("k")
    third = h.acquire_lock("k", 30)
    return {"first": first, "second": second, "ms": ms, "third": third}
