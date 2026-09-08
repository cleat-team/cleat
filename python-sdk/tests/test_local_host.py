"""Thorough unit tests for LocalHostCalls: record/replay, state, signals,
signals, promises, children, locks, cron, and scope management.

Test layout
-----------
1. **Record mode** — verify calls are recorded to the event log.
2. **Replay mode** — record, save log, replay, verify identical results.
3. **State operations** — set, get, delete, increment, has, list, scoped.
4. **Signal handling** — poll, await, injection, quorum.
5. **Child workflows** — spawn, await, await_all.
6. **Promises** — create, resolve, reject, await.
7. **Miscellaneous** — locks, cron, defer, send, scope, identity.
"""

from __future__ import annotations

import json

import pytest

from cleat_sdk.host_calls import (
    ChildWorkflowOptions,
    RetryPolicy,
)
from cleat_sdk.local_host import LocalHostCalls

# ========================================================================
# Fixtures
# ========================================================================


@pytest.fixture
def host() -> LocalHostCalls:
    """Create a fresh LocalHostCalls instance in record mode."""
    h = LocalHostCalls(mode="record")
    h.reset()
    return h


# ========================================================================
# Record mode
# ========================================================================


# ========================================================================
# Replay mode
# ========================================================================


# ========================================================================
# State operations
# ========================================================================


# ========================================================================
# Signal handling
# ========================================================================


class TestSignalHandling:
    """Signal injection, polling, awaiting, and quorum."""

    def test_inject_and_poll_signal(self, host: LocalHostCalls):
        """An injected signal is found by ``poll_signal``."""
        host.inject_signal("order_ready", '{"order_id": "ord-42"}')
        payload, found = host.poll_signal("order_ready")
        assert found
        assert payload == '{"order_id": "ord-42"}'

    def test_poll_signal_not_found(self, host: LocalHostCalls):
        """``poll_signal`` returns ``("", False)`` when no signal is pending."""
        payload, found = host.poll_signal("nonexistent")
        assert not found
        assert payload == ""

    def test_signal_is_consumed_once(self, host: LocalHostCalls):
        """A delivered signal is consumed and not available again."""
        host.inject_signal("one_shot", "payload")
        result1 = host.await_signals(["one_shot"], 5.0)
        assert not result1.timed_out
        result2 = host.await_signals(["one_shot"], 1.0)
        assert result2.timed_out

    def test_await_signals_receives(self, host: LocalHostCalls):
        """``await_signals`` receives a matching injected signal."""
        host.inject_signal("payment_received", '{"amount": 100}')
        result = host.await_signals(["payment_received"], 5.0)
        assert not result.timed_out
        assert result.name == "payment_received"
        assert result.payload == '{"amount": 100}'

    def test_await_signals_timeout(self, host: LocalHostCalls):
        """``await_signals`` times out when no signal arrives."""
        result = host.await_signals(["never_sent"], 0.01)
        assert result.timed_out
        assert result.name == ""

    def test_signal_workflow_adds_signal(self, host: LocalHostCalls):
        """``signal_workflow`` adds a signal to the local queue."""
        host.signal_workflow("target-run", "hello", {"msg": "world"})
        payload, found = host.poll_signal("hello")
        assert found
        assert "world" in payload

    def test_await_signals_infinite_timeout(self, host: LocalHostCalls):
        """Awaiting signals with a zero/negative timeout returns immediately."""
        result = host.await_signals(["none"], 0)
        assert result.timed_out

    def test_poll_signal_multiple_names(self, host: LocalHostCalls):
        """``poll_signal`` only matches the exact name."""
        host.inject_signal("sig_a", "payload_a")
        host.inject_signal("sig_b", "payload_b")
        payload, found = host.poll_signal("sig_b")
        assert found
        assert payload == "payload_b"

    def test_await_signals_with_quorum_basic(self, host: LocalHostCalls):
        """``await_signals_with_quorum`` collects signals until min_count."""
        host.inject_signal("vote", '{"choice": "yes"}')
        host.inject_signal("vote", '{"choice": "no"}')
        results = host.await_signals_with_quorum(["vote"], 2, -1, 5000)
        assert len(results) == 2
        assert results[0].name == "vote"
        assert results[1].name == "vote"

    def test_await_signals_with_quorum_timeout(self, host: LocalHostCalls):
        """``await_signals_with_quorum`` raises on timeout."""
        with pytest.raises(RuntimeError, match="quorum timeout"):
            host.await_signals_with_quorum(["never"], 1, -1, 10)


# ========================================================================
# Child workflows
# ========================================================================


class TestChildWorkflows:
    """Child workflow spawn, await, and batch await."""

    def test_child_workflow_returns_run_id(self, host: LocalHostCalls):
        """``child_workflow`` returns a valid run ID."""
        run_id = host.child_workflow("order_processor", {"order_id": "ord-42"})
        assert isinstance(run_id, str)
        assert run_id.startswith("local-child-order_processor")

    def test_await_child_returns_input(self, host: LocalHostCalls):
        """``await_child`` returns the child's result."""
        run_id = host.child_workflow("wf", {"data": "test"})
        result = host.await_child(run_id)
        assert json.loads(result)["data"] == "test"

    def test_await_child_not_found(self, host: LocalHostCalls):
        """``await_child`` with an unknown run_id raises RuntimeError."""
        with pytest.raises(RuntimeError, match="child not found"):
            host.await_child("nonexistent-run-id")

    def test_child_workflow_with_options(self, host: LocalHostCalls):
        """``child_workflow_with_options`` accepts and stores version info."""
        opts = ChildWorkflowOptions(version=2, priority=5)
        run_id = host.child_workflow_with_options("wf", {}, opts)
        assert isinstance(run_id, str)
        result = host.await_child(run_id)
        assert result is not None

    def test_await_all_children(self, host: LocalHostCalls):
        """``await_all_children`` returns results for every child."""
        r1 = host.child_workflow("wf_a", {"x": 1})
        r2 = host.child_workflow("wf_b", {"y": 2})
        results = host.await_all_children([r1, r2])
        assert len(results) == 2
        assert results[0].error is None
        assert results[1].error is None
        assert "x" in results[0].result

    def test_await_all_children_missing(self, host: LocalHostCalls):
        """``await_all_children`` flags missing children with error."""
        results = host.await_all_children(["missing-run-id"])
        assert len(results) == 1
        assert results[0].error == "child not found"


# ========================================================================
# Promises
# ========================================================================


class TestPromises:
    """Promise create, resolve, reject, await operations."""

    def test_create_promise(self, host: LocalHostCalls):
        """``create_promise`` returns a promise ID."""
        pid = host.create_promise("my-prom")
        assert pid.startswith("local-prom-my-prom")

    def test_resolve_and_await(self, host: LocalHostCalls):
        """A resolved promise returns its value."""
        pid = host.create_promise("test")
        host.resolve_promise(pid, '"resolved-value"')
        result = host.await_promise(pid, 5.0)
        assert not result.timed_out
        assert result.result == '"resolved-value"'
        assert not result.rejected

    def test_reject_and_await(self, host: LocalHostCalls):
        """A rejected promise returns with ``rejected=True``."""
        pid = host.create_promise("fail")
        host.reject_promise(pid, "something went wrong")
        result = host.await_promise(pid, 5.0)
        assert not result.timed_out
        assert result.rejected

    def test_await_pending_timeout(self, host: LocalHostCalls):
        """A pending promise times out."""
        pid = host.create_promise("never-resolved")
        result = host.await_promise(pid, 0.01)
        assert result.timed_out

    def test_await_missing_promise(self, host: LocalHostCalls):
        """Awaiting a non-existent promise raises RuntimeError."""
        with pytest.raises(RuntimeError, match="promise not found"):
            host.await_promise("nonexistent", 1.0)

    def test_resolve_nonexistent_promise(self, host: LocalHostCalls):
        """Settling a non-existent promise raises, matching the engine.

        These two asserted the opposite until 2026-09-06 -- that settling an
        unknown promise is a silent no-op -- and that was true of the engine
        when they were written. #818 made the store report ErrPromiseNotFound
        for a settle matching no row, and engine/promises.go turns any store
        error into a non-zero result code, so the harness had become more
        permissive than production and these tests were what held it there.

        It matters beyond promises because a reply address IS a promise ID
        (IMPROVEMENT-PLAN 3.220): a stale reply address and an unknown promise
        are now the same case, and succeeding on it would leave the sender
        suspended until its timeout with the error visible nowhere.
        """
        with pytest.raises(RuntimeError, match="promise not found"):
            host.resolve_promise("nonexistent", '"val"')

    def test_reject_nonexistent_promise(self, host: LocalHostCalls):
        """Rejecting a non-existent promise raises. See the test above."""
        with pytest.raises(RuntimeError, match="promise not found"):
            host.reject_promise("nonexistent", "error")


# ========================================================================
# Locks
# ========================================================================


class TestLocks:
    """Lock acquire and release operations."""

    def test_acquire_lock(self, host: LocalHostCalls):
        """``acquire_lock`` returns True for an available lock."""
        assert host.acquire_lock("my-lock", 60.0)

    def test_acquire_lock_twice(self, host: LocalHostCalls):
        """Acquiring an already-held lock returns False."""
        assert host.acquire_lock("my-lock", 60.0)
        assert not host.acquire_lock("my-lock", 60.0)

    def test_release_lock(self, host: LocalHostCalls):
        """After release, the lock can be acquired again."""
        host.acquire_lock("my-lock", 60.0)
        host.release_lock("my-lock")
        assert host.acquire_lock("my-lock", 60.0)

    def test_release_unheld_lock(self, host: LocalHostCalls):
        """Releasing an unheld lock is safe (no-op)."""
        host.release_lock("never-held")  # should not raise

    def test_acquire_lock_ms(self, host: LocalHostCalls):
        """``acquire_lock_ms`` works with millisecond TTL."""
        assert host.acquire_lock_ms("lock", 5000)
        assert not host.acquire_lock_ms("lock", 5000)


# ========================================================================
# Cron scheduling
# ========================================================================


class TestCronSchedules:
    """Cron schedule create, delete, and list operations."""

    def test_schedule_cron_returns_id(self, host: LocalHostCalls):
        """``schedule_cron`` returns a schedule ID."""
        sid = host.schedule_cron("my_wf", "0 0 * * *", "UTC", "{}")
        assert sid.startswith("local-cron-")

    def test_list_crons(self, host: LocalHostCalls):
        """``list_crons`` returns registered schedules."""
        host.schedule_cron("wf1", "* * * * *", "UTC", "{}")
        host.schedule_cron("wf2", "0 */2 * * *", "America/New_York", '{"x": 1}')
        crons = host.list_crons()
        assert len(crons) == 2
        assert crons[0]["workflow_name"] == "wf1"
        assert crons[1]["workflow_name"] == "wf2"

    def test_delete_cron(self, host: LocalHostCalls):
        """``delete_cron`` removes a schedule."""
        sid = host.schedule_cron("wf", "* * * * *", "UTC", "{}")
        assert len(host.list_crons()) == 1
        host.delete_cron(sid)
        assert len(host.list_crons()) == 0

    def test_list_crons_empty(self, host: LocalHostCalls):
        """``list_crons`` returns empty list when no schedules exist."""
        assert host.list_crons() == []


# ========================================================================
# Defer, send, schedule_invoke
# ========================================================================


class TestLifecycleHelpers:
    """Defer, send, schedule_invoke, extend_timeout, continue_as_new."""

    def test_cleat_defer_returns_id(self, host: LocalHostCalls):
        """``defer`` returns a defer ID."""
        did = host.defer("cleanup resources")
        assert did.startswith("local-defer-")

    def test_cleat_send_no_error(self, host: LocalHostCalls):
        """``send`` does not raise."""
        host.send("notification", "send", {"msg": "hello"})  # should not raise

    def test_schedule_invoke_no_error(self, host: LocalHostCalls):
        """``schedule_invoke`` does not raise."""
        host.schedule_invoke("cleanup", "purge", {"days": 30}, 3600000)  # should not raise

    def test_continue_as_new_no_error(self, host: LocalHostCalls):
        """``continue_as_new`` does not raise."""
        host.continue_as_new({"new": "input"})  # should not raise

    def test_extend_timeout_no_error(self, host: LocalHostCalls):
        """``extend_timeout`` does not raise."""
        host.extend_timeout(60000)  # should not raise

    def test_run_detached_records_the_workflow_it_starts(self, host: LocalHostCalls):
        """``run_detached`` records the start, and does not run anything inline.

        This test asserted the opposite until IMPROVEMENT-PLAN 3.253: it passed a
        function and checked that it had been *executed*, which is exactly the
        behaviour that made the method a silent no-op -- the work ran inside the
        caller and was cancelled with it, while the docstring promised the host
        would keep it alive.

        A test can pin a defect as firmly as a feature. This one did, and it
        passed for as long as the defect existed.
        """
        host.run_detached("detached-workflow", '{"a": 1}')

        recorded = [e for e in host._event_log if e.method == "run_detached"]
        assert len(recorded) == 1, (
            "run_detached must be recorded like any other host call; a call that "
            "logs nothing cannot be replayed"
        )
        assert recorded[0].kwargs["name"] == "detached-workflow"

    def test_uuid_deterministic(self, host: LocalHostCalls):
        """``uuid`` returns a deterministic UUID for the same seed."""
        u1 = host.uuid("seed-1")
        u2 = host.uuid("seed-1")
        u3 = host.uuid("seed-2")
        assert u1 == u2
        assert u1 != u3
        # Check UUID format
        parts = u1.split("-")
        assert len(parts) == 5

    def test_uuid_format(self, host: LocalHostCalls):
        """``uuid`` returns a valid UUIDv5-formatted string."""
        u = host.uuid("test")
        assert len(u) == 36
        assert u.count("-") == 4


# ========================================================================
# Identity and version
# ========================================================================


class TestIdentity:
    """Workflow identity methods."""

    def test_workflow_id(self, host: LocalHostCalls):
        """``current_workflow_id`` returns the configured ID."""
        assert host.current_workflow_id() == "local-wf-id"

    def test_run_id(self, host: LocalHostCalls):
        """``current_run_id`` returns the configured run ID."""
        assert host.current_run_id() == "local-run-id"

    def test_version(self, host: LocalHostCalls):
        """``version`` and ``min_version`` return defaults."""
        assert host.version() == 1
        assert host.min_version() == 1

    def test_now(self, host: LocalHostCalls):
        """``now`` returns a reasonable timestamp."""
        now = host.now()
        assert isinstance(now, int)
        assert now > 0

    def test_random_deterministic(self, host: LocalHostCalls):
        """``random`` returns a deterministic value."""
        assert host.random() == 42


# ========================================================================
# Scope management
# ========================================================================


# ========================================================================
# Plugin calls
# ========================================================================


class TestPluginCalls:
    """Plugin call operations."""

    def test_plugin_call_returns_response(self, host: LocalHostCalls):
        """``plugin_call`` returns a mock response."""
        result = host.plugin_call("llm", "chat", {"messages": []})
        data = json.loads(result)
        assert data["status"] == "ok"
        assert data["plugin"] == "llm"
        assert data["function"] == "chat"


# ========================================================================
# fetch convenience methods
# ========================================================================


class TestFetchMethods:
    """HTTP fetch convenience methods."""

    def test_cleat_fetch(self, host: LocalHostCalls):
        """``fetch`` returns a tuple (body, status)."""
        body, status = host.fetch("http://example.com")
        assert status == 200
        assert isinstance(body, str)

    def test_fetch_get(self, host: LocalHostCalls):
        """``fetch_get`` delegates to fetch with GET."""
        _body, status = host.fetch_get("http://example.com")
        assert status == 200

    def test_fetch_get_json(self, host: LocalHostCalls):
        """``fetch_get_json`` returns a dict."""
        result = host.fetch_get_json("http://example.com")
        assert isinstance(result, dict)

    def test_cleat_fetch_json(self, host: LocalHostCalls):
        """``fetch_json`` deserialises the response."""
        result = host.fetch_json("http://example.com")
        assert isinstance(result, dict)


# ========================================================================
# log / log_kv
# ========================================================================


class TestLogging:
    """Logging operations."""

    def test_cleat_log_no_error(self, host: LocalHostCalls):
        """``log`` does not raise."""
        host.log("hello world")  # should not raise

    def test_log_kv_no_error(self, host: LocalHostCalls):
        """``log_kv`` does not raise."""
        host.log_kv("processing", "id", "42")  # should not raise

    def test_log_kv_odd_count(self, host: LocalHostCalls):
        """``log_kv`` handles an odd number of key-value args."""
        host.log_kv("test", "key_only")  # should not raise


# ========================================================================
# call error handling (non-replay path)
# ========================================================================


class TestCallErrorHandling:
    """Error conditions for call and related methods."""

    def test_cleat_call_with_retry_delegates(self, host: LocalHostCalls):
        """``call_with_retry`` delegates to ``call``."""
        policy = RetryPolicy(max_attempts=3)
        result = host.call_with_retry("svc", "op", {"x": 1}, policy)
        assert json.loads(result)["status"] == "ok"

    def test_cleat_call_typed(self, host: LocalHostCalls):
        """``call_typed`` returns an instance of the target type."""
        from dataclasses import dataclass

        @dataclass
        class CallResponse:
            status: str = ""
            service: str = ""
            operation: str = ""
            echo: str = ""

        result = host.call_typed("svc", "greet", {"name": "World"}, CallResponse)
        assert isinstance(result, CallResponse)
        assert result.status == "ok"
        assert result.service == "svc"

    def test_send_signal_and_wait_delivers_a_reply_address_and_the_original_payload(
        self, host: LocalHostCalls
    ):
        """The receiver sees the payload as sent, plus an address to answer at.

        What this replaces asserted ``data["status"] == "signal_sent"`` -- a
        constant this harness returned without sending a signal, waiting, or
        ever receiving a reply, so it could not fail for any reason connected
        to request/reply (IMPROVEMENT-PLAN 3.220).

        It cannot assert the SENDER waking: await_promise_ms returns
        immediately for a pending promise rather than suspending
        (IMPROVEMENT-PLAN 3.235). The Go harnesses had this and no longer do --
        they wait on a channel a concurrent resolver closes. That fix ports
        here, because LocalHostCalls is an ordinary object a second thread can
        reach; it does not port to the Rust harness, which holds its state in a
        RefCell.
        """
        with pytest.raises(RuntimeError, match="no reply to signal"):
            host.send_signal_and_wait("target-run", "sig", '{"key":"val"}', 5.0)

        sig = host.await_signals(["sig"], 1.0)
        assert not sig.timed_out, "the signal was never sent"
        assert sig.payload == '{"key":"val"}', "receiver must see the payload as sent"
        assert sig.reply_to, "expected a reply address"

        host.reply_to_signal(sig.reply_to, '"reply-response"')

        res = host.await_promise(sig.reply_to, 1.0)
        assert not res.timed_out, "the reply promise is still pending after reply_to_signal"
        assert res.result == '"reply-response"'

    def test_signal_workflow_carries_no_reply_address(self, host: LocalHostCalls):
        """Negative control: a one-way signal must carry no reply address.

        Without this, a receiver could not tell a request that wants an answer
        from a notification, and would reply into a promise nobody awaits.
        """
        host.signal_workflow("target-run", "sig", '{"key":"val"}')
        sig = host.await_signals(["sig"], 1.0)
        assert not sig.timed_out
        assert sig.reply_to == "", "a one-way signal must carry no reply address"
        assert sig.payload == '{"key":"val"}', "payload must pass through unchanged"


# ========================================================================
# reply_to_signal
# ========================================================================


class TestReplyToSignal:
    """Reply-to-signal operations."""

    def test_reply_to_an_unknown_address_raises(self, host: LocalHostCalls):
        """Replying to an address nobody awaits is an error, not a success.

        This asserted the opposite -- that reply_to_signal never raises -- back
        when it only appended a journal entry and reached nothing. Now it
        resolves the reply promise, so a stale or empty address has to be
        visible rather than leaving the sender suspended until its timeout.
        """
        with pytest.raises(RuntimeError, match="promise not found"):
            host.reply_to_signal("corr-123", '"ok"')
        with pytest.raises(RuntimeError, match="empty correlation ID"):
            host.reply_to_signal("", '"ok"')


# ========================================================================
# Mode validation
# ========================================================================


class TestModeValidation:
    """Mode parameter validation."""

    def test_invalid_mode_raises(self):
        """An invalid mode string raises ValueError."""
        with pytest.raises(ValueError, match="mode must be 'record' or 'replay'"):
            LocalHostCalls(mode="invalid")

    def test_default_mode_is_record(self):
        """The default mode is ``"record"``."""
        h = LocalHostCalls()
        assert h._mode == "record"


# ========================================================================
# Reset
# ========================================================================


# ========================================================================
# Poll cancellation
# ========================================================================


class TestPollCancellation:
    """Poll cancellation operations."""

    def test_poll_cancellation_returns_default(self, host: LocalHostCalls):
        """``poll_cancellation`` returns ``(False, "")``."""
        cancelled, reason = host.poll_cancellation()
        assert not cancelled
        assert reason == ""
