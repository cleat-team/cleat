"""The Python script guest for the fault-injection harness (cleat#4169): the same operations as testdata/chaosscript (Go), interpreted from a
list given as the input, so a scenario is data and runs unchanged on either language.

Every effect on the outside world goes through the recording service "chaos", under an operation name taken from the script.
"""

import time
from typing import Any

from cleat_sdk import HostCalls, RetryPolicy, SuspendSentinel, cleat_entry


def _request(
    run: str, name: str, held: str = "", fail: int = 0, delay: int = 0
) -> dict[str, Any]:
    return {"run": run, "name": name, "held": held, "fail": fail, "delay": delay}


@cleat_entry("chaos_script")
def chaos_script(h: HostCalls, run: str, ops: list) -> str:
    seen: dict[str, str] = {}
    # The schedule ids the run's own cron_create ops got, by name, so a later cron_delete deletes the one it means.
    cron_ids: dict[str, str] = {}
    cron_workflow = "chaos-cron-" + run
    for o in ops:
        kind = o["op"]
        if kind == "call":
            seen[o["name"]] = str(
                h.call(
                    "chaos",
                    o["name"],
                    _request(run, o["name"], delay=int(o.get("delay", 0))),
                )
            )
        elif kind == "call_retry":
            policy = RetryPolicy(
                max_attempts=int(o.get("attempts", 3)),
                initial_interval_ms=100,
                backoff_coefficient=1.0,
                max_interval_ms=100,
            )
            seen[o["name"]] = str(
                h.call_with_retry(
                    "chaos",
                    o["name"],
                    _request(run, o["name"], fail=int(o.get("fail", 0))),
                    policy,
                )
            )
        elif kind == "side_effect":
            # The value a non-reproducible clock read gave the FIRST time; the service sees the one the guest HELD.
            held = h.side_effect(str(time.time_ns()))
            h.call("chaos", o["name"], _request(run, o["name"], held=held))
            seen[o["name"]] = held
        elif kind == "sleep":
            h.sleep_ms(int(o["ms"]))
        elif kind == "defer":
            name = o["name"]
            h.defer_func(
                lambda name=name: h.call(
                    "chaos", "defer_" + name, _request(run, "defer_" + name)
                )
            )
        elif kind == "await0":
            res = h.await_signals_ms([o["name"]], 0)
            branch = o["miss"] if res.timed_out else o["hit"]
            h.call("chaos", branch, _request(run, branch))
            seen[branch] = "1"
        elif kind == "await_signal":
            # Same op as the Go guest's of the name (cleat#4306): await0 with a real timeout, so a harness can measure the time from a signal's commit to the guest's next step.
            res = h.await_signals_ms([o["name"]], int(o["ms"]))
            branch = o["miss"] if res.timed_out else o["hit"]
            h.call("chaos", branch, _request(run, branch))
            seen[branch] = "1"
        elif kind == "await_promise":
            pid = h.create_promise("p-" + o["name"])
            res = h.await_promise(pid, int(o["ms"]) / 1000.0)
            branch = o["miss"] if res.timed_out else o["hit"]
            h.call("chaos", branch, _request(run, branch))
            seen[branch] = "1"
        elif kind in ("promise_create", "promise_resolve", "promise_reject"):
            # Same effect as the Go guest's op of the name: a promise "p-<name>" the harness reads from workflow_promises after the run.
            pid = h.create_promise("p-" + o["name"])
            if kind == "promise_resolve":
                h.resolve_promise(pid, '"ok"')
            elif kind == "promise_reject":
                h.reject_promise(pid, "rejected")
        elif kind == "send":
            # Same effect as the Go guest's op of the name: fire-and-forget to the service as "send_<name>", at most once.
            h.send("chaos", "send_" + o["name"], _request(run, "send_" + o["name"]))
        elif kind == "signal":
            # Same effect as the Go guest's op of the name: a signal "sig-<name>" to THIS run, whose only trace is a workflow_signals row.
            h.signal_workflow(run, "sig-" + o["name"], {"n": o["name"]})
        elif kind == "cron_create":
            # A yearly schedule for the workflow "chaos-cron-<run>": the effect lands in workflow_schedules.
            cron_ids[o["name"]] = h.schedule_cron(
                cron_workflow, "0 0 1 1 *", "UTC", '{"n":"' + o["name"] + '"}'
            )
            seen[o["name"]] = cron_ids[o["name"]]
        elif kind == "cron_delete":
            h.delete_cron(cron_ids[o["name"]])
        elif kind == "cron_list":
            # The count of this run's schedules the guest HELD, reported to the service: a replay that listed afresh reports another.
            n = sum(1 for c in h.list_crons() if c.get("workflow_name") == cron_workflow)
            held = "n=%d" % n
            # TWO calls carry the held count (see the Go guest): a replayed list is observable only through a request made after it.
            for name in (o["name"], o["name"] + "2"):
                h.call("chaos", name, _request(run, name, held=held))
            seen[o["name"]] = held
        elif kind == "continue_as_new":
            h.continue_as_new(
                {
                    "run": run,
                    "ops": [{"op": "call", "name": o["name"]}],
                    "__entry_point": "run",
                }
            )
            # As the SDK says to: raise the sentinel after continue_as_new. A component guest does not drain its defers on this path itself.
            raise SuspendSentinel()
        else:
            raise ValueError("chaosscript: unknown op " + repr(kind))
    return str(seen)
