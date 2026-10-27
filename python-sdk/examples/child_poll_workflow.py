"""Child polling — poll_child and await_any_child against a host that answers.

This is the fixture for engine/python_child_poll_e2e_test.go (cleat#3390).

Both calls were registered with the component linker and linked to a fallback
that wrote a u64 zero and called nothing, so a Python guest on a worker never
reached PollChild or AwaitAnyChild. The SDK's own tests could not see it: they
run against LocalHost, which simulates the host and never crosses the
component boundary. A component that only builds is no better, since the
bindings exist whether or not the host answers them.

Each call here is therefore load-bearing, and each result is returned so the
test can compare it with what the host's child store was told to report.
"""

import json

from cleat_sdk import HostCalls, cleat_entry


@cleat_entry("child_poll")
def child_poll(h: HostCalls, done: str, pending: str) -> str:
    """Poll a finished child and a running one, then race them.

    ``done`` and ``pending`` are run IDs the test's child store reports as
    completed and as still running.
    """
    finished = h.poll_child(done)
    running = h.poll_child(pending)
    winner = h.await_any_child([pending, done])
    return json.dumps(
        {
            "polled_result": finished.result if finished is not None else None,
            "polled_error": finished.error if finished is not None else None,
            "running_is_none": running is None,
            "winner_run_id": winner.run_id,
            "winner_result": winner.result,
        }
    )


@cleat_entry("child_await_pending")
def child_await_pending(h: HostCalls, pending: str) -> str:
    """Race children none of which has finished.

    The host records the wait and suspends the run. The WIT function is typed
    ``-> string``, so the suspension reaches this guest as an empty answer and
    ``await_any_child`` raises; the host's suspension wins over that error, so
    the run waits instead of failing. The test asserts exactly that.
    """
    winner = h.await_any_child([pending])
    return winner.run_id
