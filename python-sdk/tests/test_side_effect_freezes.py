"""``HostCalls.side_effect`` freezes a value (cleat#4113).

The function form asks the host whether the value is recorded BEFORE it runs the function, so the function runs once across a first run and its
replays. These are the guest half; the host half (``side-effect-recorded`` returning the recorded value, the stop, the divergence) is
``engine`` tests, and the end to end one is the Python component test that builds a real component.

The fake host below is the contract in four lines: it holds a value once recorded, answers ``recorded`` from then on, and a lookup before
that answers ``not-recorded``.
"""

from __future__ import annotations

import json

import pytest

from cleat_sdk import host_calls as hc_module
from cleat_sdk.host_calls import CleatCallError, HostCalls, SuspendSentinel


class _Host:
    def __init__(self) -> None:
        self.recorded: str | None = None
        self.lookups = 0
        self.records: list[str] = []

    def lookup(self):
        self.lookups += 1
        if self.recorded is None:
            return object()  # not-recorded: anything that is not the recorded case
        return _recorded(self.recorded)

    def record(self, value: str) -> str:
        self.records.append(value)
        # replay semantics of cleat_side_effect after cleat#4113: the recorded value wins.
        if self.recorded is None:
            self.recorded = value
        return self.recorded


def _recorded(value: str):
    """``side-effect-lookup.recorded(value)``: the generated dataclass inside a component, the SDK's stand-in outside one."""
    try:
        return hc_module._SideEffectLookupRecorded(value)
    except TypeError:
        r = hc_module._SideEffectLookupRecorded()
        r.value = value
        return r


@pytest.fixture
def host(monkeypatch: pytest.MonkeyPatch) -> _Host:
    h = _Host()
    monkeypatch.setattr(hc_module, "_import_side_effect_recorded", h.lookup)
    monkeypatch.setattr(hc_module, "_import_side_effect", h.record)
    return h


class TestTheFunctionRunsOnceAcrossFirstRunAndReplay:
    def test_first_run_calls_the_function_and_records_its_value(self, host: _Host) -> None:
        calls: list[int] = []

        def fn() -> str:
            calls.append(1)
            return '"v1"'

        assert HostCalls().side_effect(fn) == '"v1"'
        assert calls == [1]
        assert host.records == ['"v1"']

    def test_a_replay_returns_the_recorded_value_without_calling_the_function(
        self, host: _Host
    ) -> None:
        host.recorded = '"frozen"'
        calls: list[int] = []

        def fn() -> str:
            calls.append(1)
            return '"different"'

        assert HostCalls().side_effect(fn) == '"frozen"'
        assert calls == [], "the function ran on a replay"
        assert host.records == [], "a replay must not record again"

    def test_count_across_a_first_run_and_a_replay(self, host: _Host) -> None:
        calls: list[int] = []

        def fn() -> str:
            calls.append(1)
            return f'"run-{len(calls)}"'

        first = HostCalls().side_effect(fn)
        second = HostCalls().side_effect(fn)  # the replay: the host now holds the value
        assert len(calls) == 1
        assert first == second == '"run-1"'


class TestTheStringFormStillWorksAndSaysWhatItCannotDo:
    def test_a_string_is_recorded_without_asking(self, host: _Host) -> None:
        assert HostCalls().side_effect('"computed"') == '"computed"'
        assert host.lookups == 0
        assert host.records == ['"computed"']

    def test_a_replay_of_the_string_form_gets_the_recorded_value(self, host: _Host) -> None:
        host.recorded = '"frozen"'
        assert HostCalls().side_effect('"recomputed"') == '"frozen"'

    def test_a_non_string_result_is_marshalled(self, host: _Host) -> None:
        HostCalls().side_effect(lambda: {"a": 1})  # type: ignore[arg-type,return-value]
        assert json.loads(host.records[0]) == {"a": 1}


class TestAStopOrAFailureOfTheLookup:
    def test_a_stop_unwinds_before_the_function_runs(
        self, host: _Host, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        class _Suspended(hc_module._CallFailureSuspended):
            pass

        # The SDK dispatches on its own stand-in outside a component, as test_call_outcomes.py does.
        monkeypatch.setattr(hc_module, "_CallFailureSuspended", _Suspended)

        def refuse():
            err = hc_module._WitErr()
            err.value = _Suspended()
            raise err

        monkeypatch.setattr(hc_module, "_import_side_effect_recorded", refuse)
        calls: list[int] = []
        with pytest.raises(SuspendSentinel):
            HostCalls().side_effect(lambda: calls.append(1) or "x")
        assert calls == [], "the function ran for a value the host refused to record"

    def test_a_divergence_raises_and_the_function_does_not_run(
        self, host: _Host, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        class _Err:
            code = 0
            message = "replay divergence at step 3: expected side_effect event, got durable_log"

        class _Failed:
            value = _Err()

        def diverge():
            err = hc_module._WitErr()
            err.value = _Failed()
            raise err

        monkeypatch.setattr(hc_module, "_import_side_effect_recorded", diverge)
        calls: list[int] = []
        with pytest.raises(CleatCallError, match="replay divergence"):
            HostCalls().side_effect(lambda: calls.append(1) or "x")
        assert calls == []
