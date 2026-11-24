"""A positive timeout must never reach the host as 0 (cleat#4144).

``int(timeout_seconds * 1000)`` truncates, so ``await_signals(names, 0.0004)`` handed the host 0 ms, which this SDK maps to "wait
without a limit": a caller who asked for a tenth of a millisecond waited for ever. These tests read what the host is actually given.
"""

from __future__ import annotations

import math

import pytest

from cleat_sdk import INFINITE_TIMEOUT_MS, HostCalls
from cleat_sdk.host_calls import _seconds_to_timeout_ms

TIMED_OUT_WORD = 1 << 16  # the timed-out flag, stop sentinel clear


@pytest.mark.parametrize(
    ("seconds", "want"),
    [
        (0.0004, 1),  # positive and sub-millisecond: 1, never 0
        (0.001, 1),
        (1.5, 1500),
        (60, 60_000),
        (0, 0),  # the caller's own non-positive value, unchanged
        (-1, -1000),
        (math.inf, INFINITE_TIMEOUT_MS),  # saturates rather than raising OverflowError
        (1e300, INFINITE_TIMEOUT_MS),
    ],
)
def test_seconds_to_the_milliseconds_the_host_is_given(seconds: float, want: int) -> None:
    assert _seconds_to_timeout_ms(seconds) == want


class TestWhatTheHostIsGiven:
    def _given_to_await_promise(self, call) -> int:
        seen: list[int] = []

        def fake(_promise_id, timeout_ms):
            seen.append(timeout_ms)
            return TIMED_OUT_WORD

        with pytest.MonkeyPatch.context() as mp:
            mp.setattr("cleat_sdk.host_calls._import_cleat_await_promise", fake)
            call(HostCalls())
        assert len(seen) == 1, "the host call was not made exactly once"
        return seen[0]

    def test_await_promise_in_seconds_gives_a_positive_timeout_at_least_1_ms(self) -> None:
        assert self._given_to_await_promise(lambda h: h.await_promise("p", 0.0004)) == 1

    def test_await_promise_in_seconds_gives_an_ordinary_timeout_as_asked(self) -> None:
        assert self._given_to_await_promise(lambda h: h.await_promise("p", 2.5)) == 2500

    def test_await_promise_in_seconds_saturates_an_infinite_timeout(self) -> None:
        assert self._given_to_await_promise(lambda h: h.await_promise("p", math.inf)) == INFINITE_TIMEOUT_MS

    def test_await_signals_in_seconds_gives_a_positive_timeout_at_least_1_ms(self) -> None:
        seen: list[int] = []

        def fake(_names_json, timeout_ms):
            seen.append(timeout_ms)
            raise RuntimeError("stop here: only the argument is being read")

        with pytest.MonkeyPatch.context() as mp:
            mp.setattr("cleat_sdk.host_calls._import_cleat_await_signals", fake)
            with pytest.raises(RuntimeError):
                HostCalls().await_signals(["a"], 0.0004)
        assert seen == [1], f"the host was given {seen}; a positive timeout reached it as 0, which this SDK reads as wait for ever"
