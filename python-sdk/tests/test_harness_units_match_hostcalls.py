"""cleat#3820: a workflow tested under CleatTestHarness must mean what it means on a worker.

The harness overrode ``sleep``, ``await_signals`` and ``await_promise`` with MILLISECOND
signatures, while ``HostCalls`` takes seconds. The same ``h.sleep(5)`` slept 5 s on a worker
and advanced the harness clock 5 ms, so a test whose assertions depended on elapsed time was
off by 1000x. The harness also had no ``_ms`` forms, which raised NotImplementedError.

It now overrides only the ``_ms`` primitives and inherits the seconds forms, which convert
and delegate exactly as they do on a worker.
"""

from __future__ import annotations

import inspect
import re

import pytest

from cleat_sdk import HostCalls
from cleat_sdk.test_harness import CleatTestHarness

UNIT_SUFFIX = re.compile(r"_(ms|seconds|ns)$")


def unit_params(fn) -> dict[str, str]:
    """{parameter name: unit} for the parameters whose NAME carries a unit."""
    out = {}
    for name in inspect.signature(fn).parameters:
        m = UNIT_SUFFIX.search(name)
        if m:
            out[name] = m.group(1)
    return out


def fresh() -> CleatTestHarness:
    h = CleatTestHarness()
    h.reset()
    return h


# ---- the behaviour the issue names -------------------------------------------------


def test_sleep_takes_seconds():
    h = fresh()
    before = h.now_ms
    h.sleep(5)
    assert h.now_ms - before == 5000, "h.sleep(5) must advance the clock 5 s, as on a worker"
    h.sleep(0.25)
    assert h.now_ms - before == 5250


def test_a_signal_due_in_three_seconds_arrives_inside_a_five_second_wait_and_not_a_two_second_one():
    """An elapsed-time assertion, the kind the 1000x error corrupted: with the old
    millisecond reading both waits were a few ms long and the signal arrived in neither."""
    for wait_s, want_timed_out in ((5, False), (2, True)):
        h = fresh()
        h.stub_signal("approved", "yes")
        h._pending_signals[-1].deliver_at_ms = h.now_ms + 3000
        got = h.await_signals(["approved"], wait_s)
        assert got.timed_out is want_timed_out, f"a {wait_s} s wait for a signal due in 3 s"


def test_await_signals_timeout_advances_the_clock_by_seconds():
    h = fresh()
    before = h.now_ms
    assert h.await_signals(["never"], 2).timed_out
    assert h.now_ms - before == 2000


def test_await_promise_takes_seconds():
    h = fresh()
    pid = h.create_promise("p")
    before = h.now_ms
    assert h.await_promise(pid, 3).timed_out
    assert h.now_ms - before == 3000


def test_zero_still_waits_forever_for_a_signal():
    """The real await_signals converts 0 s to 0 ms, which the host reads as no timeout."""
    from cleat_sdk import SuspendSentinel

    with pytest.raises(SuspendSentinel):
        fresh().await_signals(["anything"], 0)


def test_a_call_that_passes_timeout_ms_runs_under_the_harness():
    """HostCalls.call takes timeout_ms; the harness raised TypeError on it."""
    h = fresh()
    h.stub_call("svc", "op", '"ok"')
    assert h.call("svc", "op", {}, timeout_ms=5000) == '"ok"'


# ---- the _ms forms exist and are milliseconds --------------------------------------


def test_the_millisecond_forms_run_under_the_harness():
    h = fresh()
    before = h.now_ms
    assert h.sleep_ms(1500) is False
    assert h.now_ms - before == 1500
    assert h.await_signals_ms(["never"], 700).timed_out
    assert h.now_ms - before == 2200
    pid = h.create_promise("q")
    assert h.await_promise_ms(pid, 300).timed_out
    assert h.now_ms - before == 2500


# ---- the old form is refused where it can be -----------------------------------------


@pytest.mark.parametrize(
    "call",
    [
        lambda h: h.sleep(duration_ms=5),
        lambda h: h.await_signals(["x"], timeout_ms=5),
        lambda h: h.await_promise("p", timeout_ms=5),
    ],
    ids=["sleep(duration_ms=)", "await_signals(timeout_ms=)", "await_promise(timeout_ms=)"],
)
def test_the_old_millisecond_keyword_names_are_refused_by_the_seconds_forms(call):
    """A caller who passed the old keyword gets a TypeError rather than a silent change
    of meaning. A POSITIONAL millisecond value cannot be told from seconds, and is the
    breaking change the CHANGELOG states."""
    with pytest.raises(TypeError):
        call(fresh())


# ---- the structural guard: no override may change a unit -----------------------------


def test_no_harness_override_changes_the_unit_of_a_parameter():
    """For every method the harness overrides, each parameter whose NAME carries a unit
    suffix must carry the same unit as HostCalls'. This is what the three overrides broke,
    and it catches the next one without anyone having to remember the three."""
    problems = []
    checked = 0
    for name, member in vars(CleatTestHarness).items():
        if name.startswith("_") or not callable(member) or not hasattr(HostCalls, name):
            continue
        real, fake = unit_params(getattr(HostCalls, name)), unit_params(member)
        checked += 1
        if real != fake:
            problems.append(
                f"{name}: HostCalls takes {real or 'no unit-named params'}, harness takes {fake or 'none'}"
            )
    assert checked > 20, f"compared only {checked} overrides; the scan stopped seeing the class"
    assert not problems, "harness overrides disagree with HostCalls on units:\n  " + "\n  ".join(
        problems
    )


def test_the_structural_guard_can_fail():
    """A known-positive: the defect as it was, one override in the wrong unit."""

    class Broken(CleatTestHarness):
        def sleep(self, duration_ms: int) -> bool:  # the shape cleat#3820 removed
            return False

    assert unit_params(Broken.sleep) != unit_params(HostCalls.sleep)
    assert unit_params(CleatTestHarness.sleep) == unit_params(HostCalls.sleep)
