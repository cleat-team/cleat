"""Short-result workflow — proves host calls return their value, not "".

This is the regression fixture for a bug that made most string-returning host
calls come back EMPTY to a Component Model guest.

The wasmtime component bridge decoded every string result with
extractStringFromPacked, which reads the length from bits 40-63 — the layout
packDurableCallResult uses. But nineteen of the twenty-four dispatchers call
handlers that pack with packSimpleResult, whose length lives at bits 32-63.
Reading the wrong field shifts the length right by 8, so any result shorter
than 256 bytes decoded as length 0.

Every call below returns well under 256 bytes, which is exactly the range that
was broken. A workflow calling `h.call()` would NOT have caught it —
DurableCall is one of the five that packed the other way and always worked,
which is why the Python end-to-end test passed throughout.

The values are returned rather than logged so the host side can assert on them;
a log line would have been discarded and the bug preserved.
"""

from cleat_sdk import HostCalls, cleat_entry


@cleat_entry
def short_results_workflow(h: HostCalls, _unused: str) -> str:
    """Call host functions whose results are short, and return them."""
    # Both are short (a workflow ID and a run ID), and both are dispatched
    # through handlers that pack their length at bit 32 -- the side that was
    # decoded wrongly. uuid() would have been a third case but has no WIT
    # binding for Python, so calling it here would test the stub, not the
    # bridge.
    wf_id = h.current_workflow_id()
    run_id = h.current_run_id()

    h.log(f"workflow_id={wf_id} run_id={run_id}")

    # Pipe-separated so the host can split and assert each independently, and
    # so an empty field is visible as an empty field rather than merely a
    # shorter overall string.
    return f"{wf_id}|{run_id}"
