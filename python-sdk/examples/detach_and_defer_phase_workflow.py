"""run_detached and defer_phase — two host calls a Python guest makes on a worker.

This is the fixture for engine/python_detach_and_defer_phase_e2e_test.go
(cleat#3428). Both calls were registered with the component linker and linked to
a fallback that wrote a u64 zero and called nothing:

* ``run_detached`` returns a u64 in the WIT, so the zero type-checked and the
  call looked like a success while no workflow was ever started.
* ``defer_phase`` takes a bool and returns nothing, so the fallback had nothing
  to write at all. The host never learned a defer body was running, and the
  durable calls it made were recorded as the workflow body's (cleat#1155,
  cleat#1237).

Neither raises, which is why a component that merely builds and runs passes.
The tests read what the HOST recorded.
"""

from cleat_sdk import HostCalls, cleat_entry


@cleat_entry("detach_probe")
def detach_probe(h: HostCalls) -> str:
    """Start a detached workflow by name and return."""
    h.run_detached("child-wf", '{"k": 1}')
    return "started"


@cleat_entry("defer_phase_probe")
def defer_phase_probe(h: HostCalls) -> str:
    """One call from the body and one from a defer, in that order.

    The defer runs when the entry returns. The host marks events recorded while
    the guest says it is draining its defer table, so the two calls must differ
    in that mark and in nothing else.
    """
    h.defer_func(lambda: h.call("cleanup", "release", {}))
    h.call("work", "body", {})
    return "done"
