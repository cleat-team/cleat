"""The Python script guest for the fault-injection harness (cleat#4169): the same operations as testdata/chaosscript (Go), interpreted from a
list given as the input, so a scenario is data and runs unchanged on either language.

Every effect on the outside world goes through the recording service "chaos", under an operation name taken from the script.
"""

import time
from typing import Any

from cleat_sdk import HostCalls, RetryPolicy, cleat_entry


def _request(
    run: str, name: str, held: str = "", fail: int = 0, delay: int = 0
) -> dict[str, Any]:
    return {"run": run, "name": name, "held": held, "fail": fail, "delay": delay}


@cleat_entry("chaos_script")
def chaos_script(h: HostCalls, run: str, ops: list) -> str:
    seen: dict[str, str] = {}
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
        elif kind == "await_promise":
            pid = h.create_promise("p-" + o["name"])
            res = h.await_promise(pid, int(o["ms"]) / 1000.0)
            branch = o["miss"] if res.timed_out else o["hit"]
            h.call("chaos", branch, _request(run, branch))
            seen[branch] = "1"
        elif kind == "continue_as_new":
            h.continue_as_new(
                {
                    "run": run,
                    "ops": [{"op": "call", "name": o["name"]}],
                    "__entry_point": "run",
                }
            )
            return "{}"
        else:
            raise ValueError("chaosscript: unknown op " + repr(kind))
    return str(seen)
