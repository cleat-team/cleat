"""A stop is not a timeout, and a received signal is not its length.

``DurableAwaitSignals`` is one of the host's nine stop sites: a fresh await
inside a defer segment would leave a terminated workflow waiting for a signal
instead of finishing its cleanup, so ``stopBeforeNewWork`` returns
``callSuspendSentinel`` (``engine/signaller.go``).

That sentinel is bit 31, and the packed layout this import used to return put
its timed-out flag at bits 16-31, so decoding the word before testing the
sentinel yielded ``timed_out=True, err_code=0`` -- an ordinary result -- and the
workflow ran on past a stop it was told to obey (IMPROVEMENT-PLAN 3.202).

The import is now ``result<signal-outcome, call-failure>`` (cleat#3786): the
stop is ``err(call-failure.suspended)``, a case of the return type rather than a
bit inside a word, so there is nothing left to test in a particular order. These
tests pin what the guest sees for each case. What reaches the guest through a
REAL component is pinned by ``engine/python_await_signals_e2e_test.go``; this
file cannot, because outside a component the import is a stub.
"""

import pytest

import cleat_sdk.host_calls as hc
from cleat_sdk.host_calls import HostCalls, SuspendSentinel


class _Signal:
    def __init__(self, name: str, payload: str) -> None:
        self.name = name
        self.payload = payload


class _Failed:
    def __init__(self, code: int, message: str) -> None:
        self.value = type("CallError", (), {"code": code, "message": message})()


def _raising(failure: object):
    def call(*_a: object, **_k: object) -> object:
        err = hc._WitErr()
        err.value = failure
        raise err

    return call


def _await(monkeypatch: pytest.MonkeyPatch, fn) -> object:
    monkeypatch.setattr("cleat_sdk.host_calls._import_cleat_await_signals", fn)
    return HostCalls().await_signals_ms(["approve"], 1000)


class TestAwaitSignalsObeysAStop:
    def test_a_stop_unwinds_rather_than_returning_a_timeout(
        self, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        with pytest.raises(SuspendSentinel):
            _await(monkeypatch, _raising(hc._CallFailureSuspended()))

    def test_a_host_failure_raises_and_is_not_a_timeout(
        self, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        with pytest.raises(hc.CleatCallError) as exc:
            _await(monkeypatch, _raising(_Failed(0, "signal too large")))
        assert "signal too large" in str(exc.value)

    def test_a_real_timeout_still_returns_a_timed_out_result(
        self, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        """The stop must not swallow the case it resembles."""
        result = _await(monkeypatch, lambda *a, **k: hc._SignalOutcomeTimedOut())
        assert result.timed_out is True
        assert result.name == ""
        assert result.payload == ""


class TestAwaitSignalsReturnsWhatWasDelivered:
    def test_name_and_payload_come_through_exactly(self, monkeypatch: pytest.MonkeyPatch) -> None:
        sent = _Signal("go", '{"approved":true}')
        result = _await(monkeypatch, lambda *a, **k: hc._SignalOutcomeReceived(sent))
        assert result.timed_out is False
        assert (result.name, result.payload) == ("go", '{"approved":true}')

    def test_an_empty_payload_is_empty_not_missing(self, monkeypatch: pytest.MonkeyPatch) -> None:
        result = _await(monkeypatch, lambda *a, **k: hc._SignalOutcomeReceived(_Signal("ping", "")))
        assert (result.name, result.payload, result.timed_out) == ("ping", "", False)

    def test_the_import_is_called_with_the_names_and_a_timeout_and_nothing_else(
        self, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        """No pointers: a component host cannot write through one (cleat#3786)."""
        seen: list[tuple] = []

        def record(*args: object) -> object:
            seen.append(args)
            return hc._SignalOutcomeTimedOut()

        _await(monkeypatch, record)
        assert seen == [('["approve"]', 1000)]

    def test_an_unexpected_return_type_is_an_error_not_a_signal(
        self, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        """The packed word an old host would send must not be read as a signal."""
        with pytest.raises(TypeError, match="signal-outcome"):
            _await(monkeypatch, lambda *a, **k: 42)


class TestGetScopeReturnsTheScope:
    def test_the_record_is_returned_as_a_pair(self, monkeypatch: pytest.MonkeyPatch) -> None:
        scope = type("Scope", (), {"object_type": "order", "instance_key": "o-1"})()
        monkeypatch.setattr("cleat_sdk.host_calls._import_get_scope", lambda: scope)
        assert HostCalls().get_scope() == ("order", "o-1")

    def test_no_scope_is_a_pair_of_empty_strings(self, monkeypatch: pytest.MonkeyPatch) -> None:
        scope = type("Scope", (), {"object_type": "", "instance_key": ""})()
        monkeypatch.setattr("cleat_sdk.host_calls._import_get_scope", lambda: scope)
        assert HostCalls().get_scope() == ("", "")

    def test_the_import_takes_no_arguments(self, monkeypatch: pytest.MonkeyPatch) -> None:
        """The four output pointers were the defect: a component host cannot honour them."""
        scope = type("Scope", (), {"object_type": "t", "instance_key": "k"})()
        monkeypatch.setattr("cleat_sdk.host_calls._import_get_scope", lambda: scope)
        HostCalls().get_scope()  # a lambda taking none would raise TypeError on any argument
