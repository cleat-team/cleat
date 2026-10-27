"""A suspending ``await_promise`` is a stop, not a timeout or a rejection.

``AwaitPromise`` answers a fresh await that has to wait with the stop sentinel
(``engine/promises.go``, ``suspendingAwaitResult``), so the guest cannot take
its timeout branch before the run suspends. cleat#3383; the same change
cleat#3378 made for ``await_signals``.

The sentinel is bit 31. This SDK's ``decode_await_promise_result`` reads bits
24-31 as a ``rejected`` flag, so a decoder that runs before the stop test turns
a suspend into ``rejected=True, timed_out=False, err_code=0`` -- an empty
rejection -- and the workflow takes its rejection branch. (The Go SDK reads the
same bit as ``timed_out``; each decoder misreads it differently, which is why
the test is on the stop and not on any one field.)
"""

import ast
import inspect
import textwrap

import pytest

from cleat_sdk.host_calls import HostCalls, SuspendSentinel
from cleat_sdk.memory import CALL_SUSPEND_SENTINEL, decode_await_promise_result


class TestTheSentinelWouldOtherwiseReadAsAnOrdinaryResult:
    def test_the_stop_sentinel_decodes_as_an_empty_rejection(self) -> None:
        result_len, timed_out, rejected, err_code = decode_await_promise_result(
            CALL_SUSPEND_SENTINEL
        )
        assert result_len == 0
        assert err_code == 0
        assert timed_out is False
        assert rejected is True


class TestAwaitPromiseObeysAStop:
    def test_a_stop_unwinds_rather_than_returning_a_result(self) -> None:
        hc = HostCalls()
        with (
            pytest.MonkeyPatch.context() as mp,
            pytest.raises(SuspendSentinel),
        ):
            mp.setattr(
                "cleat_sdk.host_calls._import_cleat_await_promise",
                lambda *a, **k: CALL_SUSPEND_SENTINEL,
            )
            hc.await_promise_ms("prom-1", 60_000)

    def test_a_real_timeout_still_returns_a_timed_out_result(self) -> None:
        """The check must not swallow the case it resembles."""
        hc = HostCalls()
        real_timeout = 1 << 16  # the timed-out flag, sentinel clear
        assert real_timeout & CALL_SUSPEND_SENTINEL == 0
        with pytest.MonkeyPatch.context() as mp:
            mp.setattr(
                "cleat_sdk.host_calls._import_cleat_await_promise",
                lambda *a, **k: real_timeout,
            )
            result = hc.await_promise_ms("prom-1", 60_000)
        assert result.timed_out is True
        assert result.result == ""


class TestTheSentinelIsTestedBeforeTheWordIsDecoded:
    """Order is the contract, so assert on order, not merely on presence."""

    def test_raise_if_stopped_precedes_the_decode(self) -> None:
        src = inspect.getsource(HostCalls.await_promise_ms)
        tree = ast.parse(textwrap.dedent(src))
        calls = [
            n.func.id
            for n in ast.walk(tree)
            if isinstance(n, ast.Call) and isinstance(n.func, ast.Name)
        ]
        assert "_raise_if_stopped" in calls, "await_promise_ms does not test the stop sentinel"
        assert "decode_await_promise_result" in calls
        assert calls.index("_raise_if_stopped") < calls.index("decode_await_promise_result"), (
            "the stop sentinel must be tested before the result word is decoded"
        )
