"""A workflow that never returns, for testing the execution fence.

The Python counterpart of testdata/spin, and it exists for the same reason that
one does: a pure arithmetic loop that never calls back into the host, so
nothing the host does at a call boundary can stop it. Only a backend-level
interrupt can, which is precisely what makes it a test of the fence rather than
of the host-call path.

Used by TestPythonComponentExecutionFence (engine/component_fence_test.go),
which gives it a budget far shorter than it would ever run and asserts that the
budget is what stops it.
"""

from cleat_sdk import HostCalls, cleat_entry


@cleat_entry
def spin_workflow(h: HostCalls, request: dict) -> str:
    # Modulo keeps the value small: the point is to burn wall-clock inside the
    # guest, not to allocate. A growing integer would eventually make this a
    # memory-limit test instead, which is a different bound.
    n = 0
    while True:
        n = (n + 1) % 1000003
