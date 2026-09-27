"""The shared quorum cases, run against the Python SDK.

cleat#1132 was one defect written five times: every SDK passed the FULL name
set to ``await_signals_ms`` on every iteration, so a quorum counted deliveries
rather than distinct voters. It was fixed in Go and stayed broken in the other
four, because nothing compared them (cleat#1136).

``tests/conformance/quorum_cases.json`` is that comparison. This file is the
Python consumer of it; the table itself carries no language-specific text.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from cleat_sdk.host_calls import SignalResult, _quorum_over

CASES_PATH = Path(__file__).resolve().parents[2] / "tests" / "conformance" / "quorum_cases.json"


def _load_cases() -> list[dict]:
    doc = json.loads(CASES_PATH.read_text())
    cases = doc["cases"]
    assert cases, "an empty table would pass vacuously"
    return cases


# A semantic tag from the table mapped onto this SDK's wording. The table
# deliberately carries no message text -- five SDKs word these differently and
# always will -- so this dict is the only place the Python tests know a string.
KIND_NEEDLES = {
    "unsatisfiable": "unsatisfiable",
    "out_of_set": "which is not among them",
    "timeout": "quorum timeout",
    "rejections": "exceeded max rejections",
}


def _kind_of(message: str) -> str:
    for kind, needle in KIND_NEEDLES.items():
        if needle in message:
            return kind
    return f"UNCLASSIFIED({message})"


def _scripted_host(deliveries, payloads, polite, asked):
    """A host that reads from a queue, recording what it was asked for.

    ``polite`` hands over a queued name only while that name is still awaited,
    which is what the engine's own DurableAwaitSignals does -- it exercises the
    NARROWING. An impolite host hands over whatever is queued and exercises the
    OUT-OF-SET GUARD. A fix that rests on only the polite case is a fix that
    rests on the host's manners.
    """
    state = {"i": 0}

    def await_signals(names: list[str], _timeout_ms: int) -> SignalResult:
        asked.append(list(names))
        while state["i"] < len(deliveries):
            name = deliveries[state["i"]]
            state["i"] += 1
            if not polite or name in names:
                return SignalResult(
                    name=name,
                    payload=payloads.get(name, '{"ok":true}'),
                    timed_out=False,
                )
        return SignalResult(name="", payload="", timed_out=True)

    return await_signals


@pytest.mark.parametrize("case", _load_cases(), ids=lambda c: c["name"])
def test_a_shared_quorum_case_holds(case: dict) -> None:
    signal_names = list(case["signal_names"])
    caller_set = list(signal_names)
    asked: list[list[str]] = []

    host = _scripted_host(
        case["deliveries"], case.get("payloads", {}), case["host"] == "polite", asked
    )

    # remaining_ms is a constant: the deadline must never be what ends a case,
    # so the host's own timed_out is the only source of a timeout and the
    # assertions are about the loop rather than about the clock.
    def run():
        return _quorum_over(
            signal_names, case["min_count"], case["max_rejections"], lambda: 60_000, host
        )

    expect = case["expect"]
    if expect["outcome"] == "ok":
        got = run()
        assert [r.name for r in got] == expect["result_names"]
    else:
        with pytest.raises(RuntimeError) as excinfo:
            got = run()
            pytest.fail(f"expected an error, got {[r.name for r in got]}")
        assert _kind_of(str(excinfo.value)) == expect["error_kind"], (
            f"failed for the wrong reason: {excinfo.value}"
        )

    # The narrowing is the mechanism, and this is the assertion that sees it.
    # Checking only the outcome passes against an implementation that fails for
    # an unrelated reason.
    assert asked == [list(s) for s in expect["awaited_sets"]], "the sets it awaited"

    if expect.get("caller_set_unchanged"):
        assert signal_names == caller_set, "the caller's list was edited in place"
