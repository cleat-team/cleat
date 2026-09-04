"""The WIT outcome types, guest side.

IMPROVEMENT-PLAN 3.110. Every host call that the engine can refuse, or that can
fail, returns ``result<string, call-failure>`` rather than a bare ``string``,
and the world's ``run`` export returns ``run-outcome`` rather than a string
whose value might be the literal ``"__CLEAT_SUSPEND__"``.

These tests are the guest half. The end-to-end half is
engine.TestPythonDeferSegmentRunsOnlyTheDefers, which builds a real component
and runs it; these run in milliseconds and say which piece broke.
"""

from __future__ import annotations

import pytest

from cleat_sdk import HostCalls, cleat_entry
from cleat_sdk.host_calls import (
    CleatCallError,
    CleatCallPermanentError,
    CleatCallTimeoutError,
    SuspendSentinel,
    _call_or_raise,
    _raise_call_failure,
)


class _Suspended:
    """Stands in for ``call-failure.suspended``.

    The real one is ``wit_world.imports.outcomes.CallFailure_Suspended``, which
    only exists inside a built component. ``_raise_call_failure`` dispatches on
    the SDK's own stand-in for it outside WASM, which is what this subclasses,
    so the branch under test is the same branch a component takes.
    """


class _CallError:
    def __init__(self, code: int, message: str) -> None:
        self.code = code
        self.message = message


class _Failed:
    def __init__(self, code: int, message: str) -> None:
        self.value = _CallError(code, message)


@pytest.fixture(autouse=True)
def _suspended_is_the_sdks_own_class(monkeypatch):
    """Point the SDK's suspended-case class at the stand-in above."""
    import cleat_sdk.host_calls as hc

    monkeypatch.setattr(hc, "_CallFailureSuspended", _Suspended)


class TestCallFailure:
    def test_a_stop_unwinds_rather_than_becoming_an_error(self):
        """``suspended`` is not a failure and must not be reported as one.

        If it raised CleatCallError instead, a defer segment would report the
        terminated workflow as *failed* rather than suspended -- and the host
        would never drain the defer table, because it only does that on a
        suspension.
        """
        with pytest.raises(SuspendSentinel):
            _raise_call_failure("work", "body", _Suspended())

    def test_a_failure_carries_its_code_into_the_exception_class(self):
        """The code is what picks retryable from permanent.

        The component path used to drop it -- along with the failure itself --
        so a Python workflow could not have told a timeout from a bad request.
        """
        with pytest.raises(CleatCallTimeoutError) as exc:
            _raise_call_failure("svc", "op", _Failed(1, "deadline exceeded"))
        assert exc.value.call_error_code == 1
        assert "deadline exceeded" in str(exc.value)

        with pytest.raises(CleatCallPermanentError):
            _raise_call_failure("svc", "op", _Failed(4, "bad request"))

    def test_an_unclassified_failure_is_still_a_failure(self):
        with pytest.raises(CleatCallError):
            _raise_call_failure("svc", "op", _Failed(0, "something went wrong"))


class TestCallOrRaise:
    def test_an_ok_response_is_returned_unchanged(self):
        assert _call_or_raise("svc", "op", lambda a: a, "hello") == "hello"

    def test_the_err_case_cannot_arrive_as_a_response(self):
        """The point of the type, stated as a test.

        There is no argument to this that produces a *response* of "the call
        was stopped": the only way to say it is to raise, and the only thing
        that reaches the caller as a string is a string the service returned.
        """
        import cleat_sdk.host_calls as hc

        def stopped(_: str) -> str:
            err = hc._WitErr()
            err.value = _Suspended()
            raise err

        with pytest.raises(SuspendSentinel):
            _call_or_raise("work", "body", stopped, "{}")


class TestWitWorldExports:
    """The world exports two functions now, and the second one is the drain."""

    def test_run_returns_a_run_outcome_not_a_string(self):
        import sys

        @cleat_entry(name="outcome_flow")
        def outcome_flow(h: HostCalls) -> dict:
            return {"status": "ok"}

        world = sys.modules[outcome_flow.__module__].WitWorld
        out = world.run("{}")
        assert not isinstance(out, str), (
            "run returned a string. A suspension used to be the literal "
            '"__CLEAT_SUSPEND__" in that string; the host compared against it '
            "verbatim, and any workflow whose result was that text was "
            "indistinguishable from one that had stopped."
        )
        assert out.value == '{"status": "ok"}'

    def test_run_deferred_is_exported_and_drains(self):
        import sys

        from cleat_sdk import defer as defer_mod

        ran: list[str] = []

        @cleat_entry(name="drain_flow")
        def drain_flow(h: HostCalls) -> dict:
            return {}

        defer_mod._DEFERS = []
        defer_mod.register_defer("a", lambda: ran.append("a"))
        defer_mod.register_defer("b", lambda: ran.append("b"))

        world = sys.modules[drain_flow.__module__].WitWorld
        assert world.run_deferred() == 2, (
            "the host's drain export did not run the table. Without it a "
            "suspended defer segment leaves every cleanup unrun -- measured "
            "as `operations: []` before this export existed."
        )
        assert ran == ["b", "a"], "the drain is LIFO"
