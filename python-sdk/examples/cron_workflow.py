"""Cron workflow — registers, lists and removes a recurring trigger.

This is the fixture for engine/python_cron_e2e_test.go, and it exists to
prove something a hello-world cannot: that the three durable-cron calls
reach the Go host from a componentized Python guest.

Until 2026-08-09 they could not. python-sdk/wit/cleat.wit had no cron
interface, so componentize-py generated no binding and the built component
never imported the calls — HostCalls.schedule_cron raised
NotImplementedError whatever the host supported. cleat_sdk.local_host DID
implement all three, so every test that exercised cron used the in-process
host and the gap stayed invisible.

Each call here is therefore load-bearing. A workflow that only logged would
build and pass with the binding completely broken, which is exactly how the
original defect survived.
"""

from cleat_sdk import HostCalls, cleat_entry


@cleat_entry
def cron_workflow(h: HostCalls, workflow_name: str) -> str:
    """Register a recurring trigger, read it back, then remove it.

    The round trip is the assertion. schedule_cron returns a schedule ID the
    host minted; list_crons must then report the schedule; delete_cron must
    accept that same ID. A binding that was declared but unwired fails at the
    first call rather than returning something plausible.
    """
    # Evaluated in the SCHEDULE's zone, not the worker's — the timezone
    # argument is part of what this checks reaches the host intact.
    schedule_id = h.schedule_cron(
        workflow_name,
        "0 3 * * *",
        "Europe/Berlin",
        '{"source": "python-cron-e2e"}',
    )
    h.log(f"scheduled {workflow_name} as {schedule_id}")

    # Read back BEFORE deleting: the count is the only evidence the host side
    # can get that list_crons saw the schedule, since the delete below removes
    # it again and the store is empty by the time the test inspects it.
    crons = h.list_crons()
    h.log(f"list_crons returned {len(crons)} schedule(s)")

    h.delete_cron(schedule_id)
    h.log(f"deleted {schedule_id}")

    # Both halves travel back to the host: the ID it minted, and what
    # list_crons observed while the schedule existed. The test asserts the
    # store is empty afterwards, which is what proves delete_cron ran.
    return f"{schedule_id}|{len(crons)}"
