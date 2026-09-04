"""Defer bodies actually run. IMPROVEMENT-PLAN 3.73.

``HostCalls.defer(description)`` sends a string and nothing else, so the host
records that a defer exists and no code anywhere runs it -- while the SDK's
docstring promised cleanup "executed in LIFO order, analogous to Python's
``try/finally`` or Go's ``defer``". ``defer_func`` is the one with a body.

These tests exercise the registry directly and through the ``@cleat_entry``
wrapper, because the two halves fail independently: a registry that works but
is never drained is exactly the state Python was already in.
"""

from __future__ import annotations

import pytest

from cleat_sdk import defer as defer_mod
from cleat_sdk.defer import register_defer, run_deferred
from cleat_sdk.host_calls import HostCalls, SuspendSentinel


@pytest.fixture(autouse=True)
def _drain_between_tests():
    """The registry is module-level, so a leftover body from one test would run
    inside the next and make its counts someone else's."""
    defer_mod._DEFERS = []
    yield
    defer_mod._DEFERS = []


def test_runs_bodies_in_lifo_order():
    order: list[str] = []
    for name in ("first", "second", "third"):
        register_defer(name, lambda n=name: order.append(n))

    assert run_deferred() == 3
    # LIFO, not registration order. A defer releases what the one before it
    # acquired, so FIFO unwinds the workflow inside-out.
    assert order == ["third", "second", "first"]


def test_draining_makes_a_second_run_a_no_op():
    runs: list[int] = []
    register_defer("x", lambda: runs.append(1))

    assert run_deferred() == 1
    assert run_deferred() == 0, "the table was not drained"
    assert len(runs) == 1, "cleanup ran twice; a lock would be released twice"


def test_a_raising_body_does_not_stop_the_others():
    ran: list[str] = []

    def boom():
        raise RuntimeError("cleanup blew up")

    register_defer("a", lambda: ran.append("a"))
    register_defer("boom", boom)
    register_defer("c", lambda: ran.append("c"))

    assert run_deferred() == 3
    # "c" registered last so it runs first; "a" must still run after the
    # raising one between them.
    assert ran == ["c", "a"]


def test_a_suspending_body_propagates():
    """SuspendSentinel is not an error and must reach the entry wrapper, or a
    workflow the host has already recorded as suspended is reported complete."""

    def suspends():
        raise SuspendSentinel()

    register_defer("suspends", suspends)

    with pytest.raises(SuspendSentinel):
        run_deferred()


def test_the_hosts_drain_is_not_stopped_by_a_suspending_body():
    """The other drain, and the opposite answer. IMPROVEMENT-PLAN 3.110.

    The test above is the GUEST draining a workflow that finished: a body that
    suspends there has to win over the result. This is the HOST draining one
    that has already suspended, through the ``run-deferred`` export. There is
    no result left to protect, and the remaining bodies were taken off the
    table before the first one ran -- so propagating would consume them
    without running them.

    That is not a hypothetical shape. It is 3.81's destroyed cleanup, and
    3.106 found it again in the AssemblyScript SDK, where the drain stopped on
    a flag the workflow BODY had set and exactly one of two cleanups ran.
    """
    ran: list[str] = []

    register_defer("first", lambda: ran.append("first"))
    register_defer("suspends", lambda: (_ for _ in ()).throw(SuspendSentinel()))
    register_defer("last", lambda: ran.append("last"))

    assert run_deferred(propagate_suspend=False) == 3
    # LIFO: last, then the suspending one, then first -- and "first" is the
    # one that proves the drain did not stop.
    assert ran == ["last", "first"], (
        "a defer body that suspended mid-drain took the cleanups after it "
        "with it. They are already off the table, so they do not run later "
        "either: this is the cleanup being consumed rather than performed."
    )


# ---------------------------------------------------------------------------
# Through the @cleat_entry wrapper -- the half that was missing entirely.
# ---------------------------------------------------------------------------


def test_the_entry_wrapper_runs_defers_on_the_success_path():
    from cleat_sdk.entry import cleat_entry

    ran: list[str] = []

    @cleat_entry(name="ok_flow")
    def ok_flow(h: HostCalls):
        register_defer("cleanup", lambda: ran.append("cleanup"))
        ran.append("body")
        return {"ok": True}

    ok_flow("{}")
    assert ran == ["body", "cleanup"], (
        "the entry wrapper did not drain the defer table; this is the state "
        "every Python workflow was in before 3.73"
    )


def test_the_entry_wrapper_runs_defers_on_the_error_path():
    """The case a defer is FOR. A defer that only runs on the happy path is
    close to useless -- cleanup exists for the run that went wrong."""
    from cleat_sdk.entry import cleat_entry

    ran: list[str] = []

    @cleat_entry(name="bad_flow")
    def bad_flow(h: HostCalls):
        register_defer("cleanup", lambda: ran.append("cleanup"))
        raise RuntimeError("workflow failed")

    out = bad_flow("{}")
    assert "error" in out
    assert ran == ["cleanup"], "cleanup did not run for a failed workflow"


def test_the_entry_wrapper_does_not_run_defers_on_suspension():
    """The control, and the half most likely to be wrong.

    "Run the defers when the entry point stops running" fires every cleanup at
    the first sleep -- releasing locks and refunding payments in the middle of a
    workflow that has not finished and is about to continue. The failure is
    silent: the workflow still completes, it just cleaned up too early.
    """
    from cleat_sdk.entry import cleat_entry

    ran: list[str] = []

    @cleat_entry(name="sleepy_flow")
    def sleepy_flow(h: HostCalls):
        register_defer("cleanup", lambda: ran.append("cleanup"))
        raise SuspendSentinel()

    sleepy_flow("{}")
    assert ran == [], (
        "the suspended segment ran its cleanup. The workflow has not exited -- "
        "it is asleep and will continue."
    )
    # And the body is still registered, so the segment that DOES finish runs it.
    assert run_deferred() == 1


class TestDeferPhaseRestriction:
    """IMPROVEMENT-PLAN §3.35 phase 4: what a defer body may not do."""

    def setup_method(self):
        defer_mod._DEFERS = []
        defer_mod._IN_DEFER_PHASE = False

    def test_in_defer_phase_is_true_while_a_body_runs(self):
        """Asserted from INSIDE a body.

        Checking from outside is exactly where the flag is always false, so a
        test written there would pass against a flag that is never set at all.
        """
        seen = []

        def body():
            seen.append(defer_mod.in_defer_phase())

        assert not defer_mod.in_defer_phase()
        defer_mod.register_defer("d1", body)
        assert defer_mod.run_deferred() == 1
        assert seen == [True]
        assert not defer_mod.in_defer_phase(), (
            "the flag must be clear after the drain, or the next segment's "
            "first defer_func would be refused"
        )

    def test_the_flag_is_cleared_when_a_body_suspends(self):
        """The case a pair of plain assignments would get wrong.

        SuspendSentinel propagates out of run_deferred, so only try/finally
        clears the flag on that path.
        """

        def body():
            raise SuspendSentinel()

        defer_mod.register_defer("d1", body)
        with pytest.raises(SuspendSentinel):
            defer_mod.run_deferred()
        assert not defer_mod.in_defer_phase()

    def test_the_flag_is_cleared_when_a_body_raises(self):
        def body():
            raise RuntimeError("cleanup blew up")

        defer_mod.register_defer("d1", body)
        assert defer_mod.run_deferred() == 1
        assert not defer_mod.in_defer_phase()
