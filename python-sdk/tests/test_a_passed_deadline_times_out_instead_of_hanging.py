"""A deadline that has already passed times out; it never becomes a wait without a limit (cleat#4144).

A timeout of 0 or less is "do not wait" in every SDK, and waiting without a limit is explicit (``None`` / ``INFINITE_TIMEOUT_MS``).
The quorum loop computes a shrinking budget, which reaches 0 when the deadline passes; before the rule was uniform, a 0 reaching
``await_signals_ms`` would have been read as "wait for ever", so the loop guards ``wait_ms <= 0`` first. These tests pin that the
guard fires and that the budget never reaches the await.
"""

from __future__ import annotations

import pytest

from cleat_sdk.host_calls import INFINITE_TIMEOUT_MS, SignalResult, SuspendSentinel, _quorum_over
from cleat_sdk.test_harness import CleatTestHarness


def _never_called(names: list[str], wait_ms: int) -> SignalResult:
    raise AssertionError(f"a passed deadline reached the await with a wait of {wait_ms} ms")


@pytest.mark.parametrize("left_ms", [0, -1, -60_000])
def test_a_quorum_whose_deadline_has_passed_times_out_without_waiting(left_ms: int) -> None:
    with pytest.raises(RuntimeError, match="quorum timeout"):
        _quorum_over(["a", "b"], 1, 0, lambda: left_ms, _never_called)


def test_the_real_quorum_with_a_zero_timeout_times_out_rather_than_suspending() -> None:
    """Through the real method and the harness: 0 ms is a deadline of now. Under the old reading this reached
    ``await_signals_ms(names, 0)`` and the harness raised SuspendSentinel (wait for ever)."""
    h = CleatTestHarness()
    with pytest.raises(RuntimeError, match="quorum timeout"):
        h.await_signals_with_quorum(["a", "b"], 1, 0, 0)


def test_control_waiting_without_a_limit_is_still_possible_and_explicit() -> None:
    """The rows above would pass if nothing could ever wait; this one shows the explicit forms do."""
    h = CleatTestHarness()
    with pytest.raises(SuspendSentinel):
        h.await_signals_ms(["a"], None)
    with pytest.raises(SuspendSentinel):
        h.await_signals_ms(["a"], INFINITE_TIMEOUT_MS)


class TestWhatTheHostIsGivenForZeroAndNone:
    """Read the argument, not the outcome: the harness above simulates the host, and a simulation can agree with a wrong SDK."""

    @staticmethod
    def _signals_arg(timeout_ms) -> int:
        from cleat_sdk.host_calls import HostCalls

        seen: list[int] = []

        def fake(_names_json, ms):
            seen.append(ms)
            raise RuntimeError("stop: only the argument is being read")

        with pytest.MonkeyPatch.context() as mp:
            mp.setattr("cleat_sdk.host_calls._import_cleat_await_signals", fake)
            with pytest.raises(RuntimeError):
                HostCalls().await_signals_ms(["a"], timeout_ms)
        assert len(seen) == 1
        return seen[0]

    @staticmethod
    def _promise_arg(timeout_ms) -> int:
        from cleat_sdk.host_calls import HostCalls

        seen: list[int] = []

        def fake(_promise_id, ms):
            seen.append(ms)
            return 1 << 16  # timed out, stop sentinel clear

        with pytest.MonkeyPatch.context() as mp:
            mp.setattr("cleat_sdk.host_calls._import_cleat_await_promise", fake)
            HostCalls().await_promise_ms("p", timeout_ms)
        assert len(seen) == 1
        return seen[0]

    @pytest.mark.parametrize("ms", [0, -1, -5, -60_000])
    def test_zero_or_negative_reaches_the_host_as_zero_so_the_host_does_not_wait(
        self, ms: int
    ) -> None:
        # EXACTLY 0, never the caller's negative number: the WIT's timeout-ms is a u64, and a real componentize-py guest raises OverflowError
        # lowering a negative int into it, which is an uncatchable wasm trap (found in review of cleat#4189). A mock that only checked
        # "the host is not given the wait-forever value" passed on the negative and would have shipped it.
        assert self._signals_arg(ms) == 0
        assert self._promise_arg(ms) == 0

    @pytest.mark.parametrize("seconds", [0, -0.5, -3])
    def test_a_zero_or_negative_seconds_timeout_reaches_the_host_as_zero(
        self, seconds: float
    ) -> None:
        from cleat_sdk.host_calls import HostCalls

        seen: list[int] = []

        def fake(_promise_id: str, ms: int) -> int:
            seen.append(ms)
            return 1 << 16  # timed out, stop sentinel clear

        with pytest.MonkeyPatch.context() as mp:
            mp.setattr("cleat_sdk.host_calls._import_cleat_await_promise", fake)
            HostCalls().await_promise("p", timeout_seconds=seconds)
        assert seen == [0]

    def test_none_reaches_the_host_as_the_wait_without_a_limit(self) -> None:
        assert self._signals_arg(None) == INFINITE_TIMEOUT_MS
        assert self._promise_arg(None) == INFINITE_TIMEOUT_MS

    def test_an_ordinary_timeout_is_unchanged(self) -> None:
        assert self._signals_arg(1500) == 1500
        assert self._promise_arg(1500) == 1500
