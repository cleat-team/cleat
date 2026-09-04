"""A stop is not a timeout.

``DurableAwaitSignals`` is one of the host's nine stop sites: a fresh await
inside a defer segment would leave a terminated workflow waiting for a signal
instead of finishing its cleanup, so ``stopBeforeNewWork`` returns
``callSuspendSentinel`` (``engine/signaller.go``).

That sentinel is bit 31, and the await-signals layout puts its timed-out flag at
bits 16-31. Decoding the word before testing the sentinel therefore yields
``timed_out=True, err_code=0`` -- an ordinary, unremarkable result -- and the
workflow runs on past a stop it was told to obey. IMPROVEMENT-PLAN 3.202.
"""

import ast
import inspect
import pathlib

import pytest

from cleat_sdk.host_calls import HostCalls, SuspendSentinel
from cleat_sdk.memory import (
    CALL_SUSPEND_SENTINEL,
    SUSPEND_SENTINEL,
    decode_await_signals_result,
)


class TestTheSentinelWouldOtherwiseReadAsAnOrdinaryTimeout:
    """The arithmetic first, so the behavioural test below has a stated why."""

    def test_the_stop_sentinel_decodes_as_a_timeout(self) -> None:
        sig_name_len, payload_len, timed_out, err_code = decode_await_signals_result(
            CALL_SUSPEND_SENTINEL
        )
        # Every field reads as an ordinary "nothing arrived in time".
        assert (sig_name_len, payload_len) == (0, 0)
        assert timed_out is True
        assert err_code == 0

    def test_it_is_not_the_other_sentinel(self) -> None:
        """The two are easy to conflate; only one of them was being tested for."""
        assert CALL_SUSPEND_SENTINEL != SUSPEND_SENTINEL
        assert CALL_SUSPEND_SENTINEL == 1 << 31
        assert SUSPEND_SENTINEL == 1 << 62


class TestAwaitSignalsObeysAStop:
    def test_a_stop_unwinds_rather_than_returning_a_timeout(self) -> None:
        hc = HostCalls()
        with (
            pytest.MonkeyPatch.context() as mp,
            pytest.raises(SuspendSentinel),
        ):
            mp.setattr(
                "cleat_sdk.host_calls._import_cleat_await_signals",
                lambda *a, **k: CALL_SUSPEND_SENTINEL,
            )
            hc.await_signals_ms(["approve"], 1000)

    def test_a_real_timeout_still_returns_a_timed_out_result(self) -> None:
        """The guard must not swallow the case it resembles."""
        hc = HostCalls()
        # timed-out flag set at bit 16, sentinel clear.
        real_timeout = 1 << 16
        assert real_timeout & CALL_SUSPEND_SENTINEL == 0
        with pytest.MonkeyPatch.context() as mp:
            mp.setattr(
                "cleat_sdk.host_calls._import_cleat_await_signals",
                lambda *a, **k: real_timeout,
            )
            result = hc.await_signals_ms(["approve"], 1000)
        assert result.timed_out is True
        assert result.name == ""


class TestTheSentinelIsTestedBeforeTheWordIsDecoded:
    """Order is the contract, so assert on order, not merely on presence.

    A source-level check because the ordering cannot be observed from outside:
    both orders raise for a bare sentinel, and only a word carrying a sentinel
    AND a field distinguishes them at runtime -- which the host does not
    currently produce for this layout. Asserting on the source is honest about
    that rather than inventing a value to prove it with.
    """

    def test_raise_if_stopped_precedes_the_decode(self) -> None:
        src = inspect.getsource(HostCalls.await_signals_ms)
        tree = ast.parse(src.lstrip())
        calls = [
            n.func.id
            for n in ast.walk(tree)
            if isinstance(n, ast.Call) and isinstance(n.func, ast.Name)
        ]
        assert "_raise_if_stopped" in calls, "await_signals_ms does not test the stop sentinel"
        assert "decode_await_signals_result" in calls
        assert calls.index("_raise_if_stopped") < calls.index("decode_await_signals_result"), (
            "the stop sentinel must be tested before the result word is decoded: "
            "bit 31 lands inside this layout's timed-out field"
        )

    def test_the_sentinel_test_is_a_mask_not_an_equality(self) -> None:
        """``result == SENTINEL`` misses a sentinel packed beside a field."""
        src = pathlib.Path(
            inspect.getsourcefile(HostCalls) or ""
        ).read_text()
        fn = next(
            n
            for n in ast.parse(src).body
            if isinstance(n, ast.FunctionDef) and n.name == "_raise_if_stopped"
        )
        assert any(
            isinstance(n, ast.BinOp) and isinstance(n.op, ast.BitAnd) for n in ast.walk(fn)
        ), "_raise_if_stopped must test by mask (&), not by whole-word equality"
