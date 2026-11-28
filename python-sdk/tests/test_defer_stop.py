"""``durable-defer`` and ``fetch`` can answer ``err(suspended)``; the guest must unwind and keep its cleanup.

cleat#4079. ``durable-defer`` returned a bare string until then, so a defer registration could not carry a stop. It is now
``result<string, call-failure>`` and a stop at a registration is how a run that was killed mid-flight is cut short so its
defers run. Two things must hold on the guest side, and both are asserted here:

* the closure is in the table when the stop arrives. The stop carries no id, the host has already recorded the registration,
  and the drain that follows the unwind is the whole point; a closure stored only after the host answered would be missing;
* a stop is a stop and a failure is a failure: a stop unwinds without discarding the closure, a failure discards it (the host
  recorded nothing for it) and raises the error class.

``fetch`` is covered too: its stop used to escape as the raw ``Err`` and surface as an ordinary guest failure that user code
could catch and swallow.
"""

from __future__ import annotations

import pytest

from cleat_sdk import defer as defer_mod
from cleat_sdk.host_calls import CleatCallError, HostCalls, SuspendSentinel, _WitErr


class _Suspended:
    """Stands in for ``call-failure.suspended``; see test_call_outcomes.py."""


class _CallError:
    def __init__(self, code: int, message: str) -> None:
        self.code = code
        self.message = message


class _Failed:
    def __init__(self, code: int, message: str) -> None:
        self.value = _CallError(code, message)


@pytest.fixture(autouse=True)
def _fresh_table_and_stand_in_class(monkeypatch):
    import cleat_sdk.host_calls as hc

    monkeypatch.setattr(hc, "_CallFailureSuspended", _Suspended)
    monkeypatch.setattr(defer_mod, "_DEFERS", [])
    yield


def _import_raising(monkeypatch, name: str, failure) -> None:
    def boom(*_a, **_k):
        # Outside WASM ``_WitErr`` is a stand-in that does not store its argument; componentize-py's Err keeps it in ``.value``.
        err = _WitErr()
        err.value = failure
        raise err

    monkeypatch.setattr(f"cleat_sdk.host_calls.{name}", boom)


class TestDeferFuncAtAStop:
    def test_a_stop_unwinds_and_the_closure_is_still_in_the_table(self, monkeypatch):
        ran: list[str] = []
        _import_raising(monkeypatch, "_import_cleat_defer", _Suspended())
        with pytest.raises(SuspendSentinel):
            HostCalls().defer_func(lambda: ran.append("cleanup"))
        # The drain that follows the unwind runs it.
        assert len(defer_mod._DEFERS) == 1
        defer_mod.run_deferred(propagate_suspend=False)
        assert ran == ["cleanup"]

    def test_the_closure_is_stored_BEFORE_the_host_call(self, monkeypatch):
        """Order is the contract: a closure stored after the answer is missing on a stop."""
        seen: list[int] = []

        def host(_desc):
            seen.append(len(defer_mod._DEFERS))
            return "defer-7"

        monkeypatch.setattr("cleat_sdk.host_calls._import_cleat_defer", host)
        HostCalls().defer_func(lambda: None)
        assert seen == [1], "the table was empty when the host was called"

    def test_ok_gives_the_entry_the_hosts_id_and_keeps_its_place(self, monkeypatch):
        monkeypatch.setattr("cleat_sdk.host_calls._import_cleat_defer", lambda _d: "defer-3")
        hc = HostCalls()
        first = hc.defer_func(lambda: None)
        second = hc.defer_func(lambda: None)
        assert (first, second) == ("defer-3", "defer-3")
        assert [i for i, _ in defer_mod._DEFERS] == ["defer-3", "defer-3"]

    def test_a_failure_discards_the_closure_and_raises_the_error_class(self, monkeypatch):
        _import_raising(monkeypatch, "_import_cleat_defer", _Failed(2, "unavailable"))
        with pytest.raises(CleatCallError):
            HostCalls().defer_func(lambda: None)
        assert defer_mod._DEFERS == [], "the host recorded nothing, so the closure must not run"

    def test_CONTROL_a_stop_is_not_the_same_as_a_failure(self, monkeypatch):
        """Both go through the same except clause; this pins that they come out different."""
        _import_raising(monkeypatch, "_import_cleat_defer", _Suspended())
        with pytest.raises(SuspendSentinel):
            HostCalls().defer_func(lambda: None)
        stop_left = len(defer_mod._DEFERS)
        monkeypatch.setattr(defer_mod, "_DEFERS", [])
        _import_raising(monkeypatch, "_import_cleat_defer", _Failed(2, "x"))
        with pytest.raises(CleatCallError):
            HostCalls().defer_func(lambda: None)
        assert (stop_left, len(defer_mod._DEFERS)) == (1, 0)


class TestDeferWithoutABody:
    def test_a_stop_unwinds(self, monkeypatch):
        _import_raising(monkeypatch, "_import_cleat_defer", _Suspended())
        with pytest.raises(SuspendSentinel):
            HostCalls().defer("release the lock")

    def test_a_failure_raises_the_error_class(self, monkeypatch):
        _import_raising(monkeypatch, "_import_cleat_defer", _Failed(2, "x"))
        with pytest.raises(CleatCallError):
            HostCalls().defer("release the lock")

    def test_ok_returns_the_id(self, monkeypatch):
        monkeypatch.setattr("cleat_sdk.host_calls._import_cleat_defer", lambda _d: "defer-0")
        assert HostCalls().defer("release the lock") == "defer-0"


class TestHostFetchAtAStop:
    def test_a_stop_unwinds_instead_of_escaping_as_a_raw_err(self, monkeypatch):
        _import_raising(monkeypatch, "_import_fetch", _Suspended())
        with pytest.raises(SuspendSentinel):
            HostCalls().host_fetch("GET", "https://example.test/")

    def test_a_failure_raises_the_error_class(self, monkeypatch):
        _import_raising(monkeypatch, "_import_fetch", _Failed(1, "timed out"))
        with pytest.raises(CleatCallError):
            HostCalls().host_fetch("GET", "https://example.test/")

    def test_ok_still_returns_the_response(self, monkeypatch):
        monkeypatch.setattr("cleat_sdk.host_calls._import_fetch", lambda *a: '{"status":200}')
        assert HostCalls().host_fetch("GET", "https://example.test/") == '{"status":200}'
