"""Workflow updates in the Python SDK.

An update is a request/reply call into a *running* workflow: the only external
interaction that both changes workflow state and returns a value to the caller.

Before updates were implemented end to end, none of it worked. Handlers were
registered into a map that only a test harness read, and a caller got a ``202``
with a ``promise_id`` that nothing ever settled (IMPROVEMENT-PLAN 3.239,
cleat#849).

These exercise ``LocalHostCalls``, which carries the same queue the engine keeps
in ``workflow_update_requests``.
"""

from __future__ import annotations

from cleat_sdk.local_host import LocalHostCalls


class TestUpdateDispatch:
    def test_an_update_runs_its_handler_and_settles_the_callers_promise(self) -> None:
        host = LocalHostCalls()
        seen: list[str] = []

        host.register_update_handler("add", lambda payload: (seen.append(payload), '{"ok":true}')[1])

        promise_id = host.create_promise("caller")
        host.enqueue_update("add", "12345", promise_id)
        host.dispatch_updates()

        assert seen == ["12345"], "the handler did not receive the payload"

        done = host.completed_updates()
        assert len(done) == 1
        name, _rid, result, error = done[0]
        assert name == "add"
        assert result == '{"ok":true}'
        assert error == ""

        res = host.await_promise(promise_id, 1.0)
        assert not res.timed_out, (
            "the caller's promise never settled -- the defect updates were built to fix"
        )
        assert res.result == '{"ok":true}'

    def test_a_validator_refusal_never_reaches_the_handler(self) -> None:
        # The validator is the half of the API that makes an update different
        # from a signal: it is read-only, so a refusal changes nothing.
        host = LocalHostCalls()
        ran: list[bool] = []

        host.register_update_handler(
            "approve",
            lambda payload: (ran.append(True), "approved")[1],
            lambda payload: False,
        )

        promise_id = host.create_promise("caller")
        host.enqueue_update("approve", '{"amount":-1}', promise_id)
        host.dispatch_updates()

        assert not ran, "the handler ran despite the validator refusing"

        done = host.completed_updates()
        assert len(done) == 1, "a refused update must still answer the caller"
        assert done[0][3], "a refused update completed with no error tells the caller it succeeded"

    def test_an_unregistered_update_answers_rather_than_hanging(self) -> None:
        # The name is chosen by the CALLER, so it can name a handler this
        # workflow does not have. That must be an answer, not silence.
        host = LocalHostCalls()
        promise_id = host.create_promise("caller")
        host.enqueue_update("no-such-handler", "{}", promise_id)
        host.dispatch_updates()

        done = host.completed_updates()
        assert len(done) == 1
        assert done[0][3], "an update naming no handler must be completed with an error"

    def test_a_handler_that_raises_still_answers(self) -> None:
        # An exception inside a handler is the caller's answer, not a reason to
        # leave them waiting.
        host = LocalHostCalls()

        def boom(payload: str) -> str:
            raise ValueError("handler blew up")

        host.register_update_handler("boom", boom)
        promise_id = host.create_promise("caller")
        host.enqueue_update("boom", "{}", promise_id)
        host.dispatch_updates()

        done = host.completed_updates()
        assert len(done) == 1
        assert "handler blew up" in done[0][3]

    def test_updates_are_not_dispatched_without_a_dispatch_point(self) -> None:
        # The negative control for the whole design. Dispatch happens at fixed
        # program positions, not on arrival. If this ever fails, dispatch has
        # become time- or host-driven, and the interleaving replay depends on is
        # no longer a property of the program.
        host = LocalHostCalls()
        ran: list[bool] = []
        host.register_update_handler("add", lambda payload: (ran.append(True), "")[1])
        host.enqueue_update("add", "x", "")

        assert not ran, "an update ran without the workflow reaching a dispatch point"
        assert host.completed_updates() == []

        host.dispatch_updates()
        assert ran, "dispatch_updates did not dispatch"

    def test_a_suspension_point_is_a_dispatch_point(self) -> None:
        # The property that makes updates usable at all: a workflow that never
        # calls dispatch_updates itself still services them, because the SDK
        # dispatches before each suspension. Without it, an update is only
        # handled by a workflow whose author remembered to ask.
        host = LocalHostCalls()
        ran: list[bool] = []
        host.register_update_handler("ping", lambda payload: (ran.append(True), "pong")[1])
        host.enqueue_update("ping", "{}", "")

        host.sleep_ms(1)

        assert ran, (
            "sleep did not dispatch pending updates. Every suspension point must, "
            "or a workflow that suspends without asking never services them."
        )

    def test_every_pending_update_is_drained_in_one_dispatch(self) -> None:
        # Two updates arriving between suspensions must both be handled at the
        # next one; draining only the first would leave the second waiting for a
        # suspension that may never come.
        host = LocalHostCalls()
        seen: list[str] = []
        host.register_update_handler("append", lambda p: (seen.append(p), "ok")[1])

        host.enqueue_update("append", "a", "")
        host.enqueue_update("append", "b", "")
        host.dispatch_updates()

        assert seen == ["a", "b"], "both pending updates must be handled, in order"
        assert len(host.completed_updates()) == 2

    def test_the_envelope_uses_compact_separators(self) -> None:
        # json.dumps defaults to ", " and ": ", which would make the Python
        # harness's envelope differ from every other SDK's for no reason. The
        # same trap the signal envelope hit (IMPROVEMENT-PLAN 3.220).
        host = LocalHostCalls()
        host.enqueue_update("add", "{}", "")
        envelope = host.poll_update()
        assert ", " not in envelope and '": ' not in envelope, (
            f"the envelope is not compact: {envelope!r}"
        )
        assert envelope.startswith('{"name":"add"')
