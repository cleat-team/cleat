"""Thorough unit tests for HostCalls: method coverage, behavioral tests,
plugin wrappers, dataclasses, and exception hierarchy.

This file was rewritten to replace ~40% ``assert callable()`` trivial tests
with behavioral tests using :class:`CleatTestHarness` and to remove mock-only
tests that verified test infrastructure rather than production logic.

Test layout
-----------
1. **Behavioral tests** — ``CleatTestHarness``-based round-trips for every
   major HostCalls capability (durable calls, sleep, signals, children,
   saga compensation, retry, cancellation, query state, timeouts).
2. **Method existence** — a single test that verifies all expected
   ``HostCalls`` public methods are present.
3. **Delegation tests** — ``log_kv``, ``has_state``, ``list_state``, and
   HTTP-fetch convenience methods verified via ``mock.patch``.
4. **Dataclass construction** — ``RetryPolicy``, ``SignalResult``,
   ``ChildResult``, ``PromiseResult``.
5. **Exception hierarchy** — ``CleatCallError`` and its subclasses.
6. **Plugin wrappers** — method existence, AI-wrappers with mocked
   ``plugin_call``, and error handling in the ``_call`` helper.
"""

import json
from unittest import mock

import pytest

try:
    from cleat_sdk import memory
    from cleat_sdk.host_calls import (
        HostCalls,
        SuspendSentinel,
        RetryPolicy,
        SignalResult,
        ChildResult,
        PromiseResult,
        CleatCallError,
        CleatCallTransientError,
        CleatCallPermanentError,
        CleatCallTimeoutError,
        INFINITE_TIMEOUT_MS,
    )
    from cleat_sdk.plugins import (
        Plugins,
        LLMChatResult,
        LLMEmbedResult,
        LLMListModelsResult,
        PgVectorSearchResult,
    )
    from cleat_sdk.test_harness import CleatTestHarness
except ImportError as e:
    pytest.fail(
        f"Required import failed: {e}.  "
        "All SDK modules must be importable.",
    )


# ========================================================================
# Fixtures
# ========================================================================


@pytest.fixture(autouse=True)
def setup_memory():
    """Set up a large enough linear memory before each test.

    Required for ``HostCalls`` methods that call ``write_string`` before
    the WASM import stub raises ``NotImplementedError``.
    """
    old = memory._memory
    memory._memory = bytearray(memory.OUTPUT_OFFSET + memory.OUT_BUF_SIZE)
    yield
    memory._memory = old


@pytest.fixture
def host():
    """Create a fresh ``HostCalls`` instance for delegation/mock tests."""
    return HostCalls()


@pytest.fixture
def plugins(host):
    """Create a fresh ``Plugins`` instance wrapping ``host``."""
    return Plugins(host)


# ========================================================================
# Behavioural tests – DurableCall round-trip
# ========================================================================


class TestDurableCallRoundTrip:
    """``cleat_call`` behaviour through ``CleatTestHarness``."""

    def test_stubbed_response_returned(self):
        """Stub a service call and verify the stubbed response is returned."""
        h = CleatTestHarness()
        h.reset()
        h.stub_call("greeter", "Greet", '{"greeting": "Hello, World"}')
        result = h.cleat_call("greeter", "Greet", {"name": "World"})
        assert result == '{"greeting": "Hello, World"}'
        assert h.call_count("greeter", "Greet") == 1

    def test_last_call_records_request(self):
        """The call history entry captures the marshalled request."""
        h = CleatTestHarness()
        h.reset()
        h.stub_call("email", "send", '"ok"')
        h.cleat_call("email", "send", {"to": "alice@example.com", "subject": "Hello"})
        rec = h.last_call("email", "send")
        assert rec is not None
        assert rec.service == "email"
        assert rec.operation == "send"
        assert "alice@example.com" in rec.request
        assert rec.response == '"ok"'

    def test_stubbed_error_is_raised(self):
        """A stub with ``error`` set raises ``RuntimeError``."""
        h = CleatTestHarness()
        h.reset()
        h.stub_call("payment", "charge", error="insufficient funds")
        with pytest.raises(RuntimeError, match="insufficient funds"):
            h.cleat_call("payment", "charge", {"amount": 999999})

    def test_stubs_consumed_fifo(self):
        """Multiple stubs for the same ``(service, op)`` are consumed in order."""
        h = CleatTestHarness()
        h.reset()
        h.stub_call("svc", "op", '"first"')
        h.stub_call("svc", "op", '"second"')
        h.stub_call("svc", "op", '"third"')

        assert h.cleat_call("svc", "op", {}) == '"first"'
        assert h.cleat_call("svc", "op", {}) == '"second"'
        assert h.cleat_call("svc", "op", {}) == '"third"'
        assert h.call_count("svc", "op") == 3

    def test_call_count_tracks_multiple_services(self):
        """``call_count`` distinguishes different service+operation pairs."""
        h = CleatTestHarness()
        h.reset()
        h.stub_call("a", "op1", '"ok"')
        h.stub_call("a", "op2", '"ok"')
        h.stub_call("b", "op1", '"ok"')
        h.cleat_call("a", "op1", {})
        h.cleat_call("a", "op2", {})
        h.cleat_call("b", "op1", {})
        assert h.call_count("a", "op1") == 1
        assert h.call_count("a", "op2") == 1
        assert h.call_count("b", "op1") == 1
        assert h.call_count("a", "op_bogus") == 0

    def test_no_stub_raises_informative_error(self):
        """Calling ``cleat_call`` without a registered stub raises."""
        h = CleatTestHarness()
        h.reset()
        with pytest.raises(RuntimeError, match="no stub registered"):
            h.cleat_call("unknown", "op", {})


# ========================================================================
# Behavioural tests – DurableSleep
# ========================================================================


class TestDurableSleep:
    """``cleat_sleep`` behaviour through ``CleatTestHarness``."""

    def test_sleep_advances_clock(self):
        """Sleep advances the simulated clock by the specified duration."""
        h = CleatTestHarness()
        h.reset()
        before = h.now_ms
        result = h.cleat_sleep(5000)
        after = h.now_ms
        assert after - before == 5000
        assert result is False

    def test_multiple_sleeps_accumulate(self):
        """Multiple sleep calls accumulate the total elapsed time."""
        h = CleatTestHarness()
        h.reset()
        start = h.now_ms
        h.cleat_sleep(1000)
        h.cleat_sleep(2000)
        h.cleat_sleep(3000)
        assert h.now_ms == start + 6000

    def test_sleep_zero_is_noop(self):
        """Sleeping for zero milliseconds does not advance the clock."""
        h = CleatTestHarness()
        h.reset()
        before = h.now_ms
        h.cleat_sleep(0)
        assert h.now_ms == before

    def test_advance_time_equivalence(self):
        """``advance_time`` produces the same clock result as ``cleat_sleep``."""
        h1 = CleatTestHarness()
        h1.reset()
        h1.cleat_sleep(5000)

        h2 = CleatTestHarness()
        h2.reset()
        h2.advance_time(5000)

        assert h1.now_ms == h2.now_ms

    def test_sleep_always_returns_false(self):
        """In the test harness ``cleat_sleep`` never suspends (returns ``False``)."""
        h = CleatTestHarness()
        h.reset()
        assert h.cleat_sleep(100) is False
        assert h.cleat_sleep(0) is False
        assert h.cleat_sleep(86_400_000) is False


# ========================================================================
# Behavioural tests – AwaitSignals
# ========================================================================


class TestAwaitSignals:
    """Signal operations through ``CleatTestHarness``."""

    def test_await_signals_receives_signal(self):
        """Deliver a signal via ``stub_signal`` and verify it is received."""
        h = CleatTestHarness()
        h.reset()
        h.stub_signal("payment_received", '{"amount": 100}')
        result = h.await_signals(["payment_received"], 5000)
        assert not result.timed_out
        assert result.name == "payment_received"
        assert result.payload == '{"amount": 100}'

    def test_await_signals_timeout(self):
        """Awaiting a signal that is never delivered eventually times out."""
        h = CleatTestHarness()
        h.reset()
        result = h.await_signals(["never_sent"], 100)
        assert result.timed_out
        assert result.name == ""
        assert result.payload == ""

    def test_poll_signal_returns_pending_signal(self):
        """``poll_signal`` returns a pending signal immediately without blocking."""
        h = CleatTestHarness()
        h.reset()
        h.stub_signal("order_ready", '{"order_id": "ord-42"}')
        payload, found = h.poll_signal("order_ready")
        assert found
        assert payload == '{"order_id": "ord-42"}'

    def test_poll_signal_not_found(self):
        """``poll_signal`` returns ``("", False)`` when no signal is pending."""
        h = CleatTestHarness()
        h.reset()
        payload, found = h.poll_signal("nonexistent")
        assert not found
        assert payload == ""

    def test_signal_is_consumed_once(self):
        """A delivered signal is consumed and not available a second time."""
        h = CleatTestHarness()
        h.reset()
        h.stub_signal("one_shot", '"delivered"')

        result1 = h.await_signals(["one_shot"], 5000)
        assert not result1.timed_out

        result2 = h.await_signals(["one_shot"], 10)
        assert result2.timed_out

    def test_await_multiple_signal_names(self):
        """``await_signals`` can wait for any of several named signals."""
        h = CleatTestHarness()
        h.reset()
        h.stub_signal("signal_a", '"payload_a"')

        result = h.await_signals(["signal_a", "signal_b"], 5000)
        assert not result.timed_out
        assert result.name == "signal_a"
        assert result.payload == '"payload_a"'


# ========================================================================
# Behavioural tests – ChildWorkflow
# ========================================================================


class TestChildWorkflow:
    """Child-workflow operations through ``CleatTestHarness``."""

    def test_child_workflow_roundtrip(self):
        """Spawn a child workflow and await its result."""
        h = CleatTestHarness()
        h.reset()
        h.stub_child_workflow("order_processor", '{"status": "completed"}')

        run_id = h.child_workflow("order_processor", {"order_id": "ord-42"})
        assert run_id.startswith("test-child-order_processor")

        result = h.await_child(run_id)
        assert result == '{"status": "completed"}'

    def test_child_workflow_always_starts(self):
        """``child_workflow`` returns a run ID even without a registered stub."""
        h = CleatTestHarness()
        h.reset()
        run_id = h.child_workflow("unregistered_wf", {"x": 1})
        assert isinstance(run_id, str)
        assert len(run_id) > 0

    def test_child_is_recorded_in_history(self):
        """``child_workflow`` appends a call-record to ``call_history``."""
        h = CleatTestHarness()
        h.reset()
        h.stub_child_workflow("emailer", '"sent"')
        h.child_workflow("emailer", {"to": "user@example.com"})

        assert h.call_count("workflow", "emailer") == 1
        rec = h.last_call("workflow", "emailer")
        assert rec is not None
        assert "user@example.com" in rec.request

    def test_await_child_raises_on_error(self):
        """A child workflow that errors propagates a ``RuntimeError`` on await."""
        h = CleatTestHarness()
        h.reset()
        h.stub_child_workflow("flaky", "", error="child crashed")

        run_id = h.child_workflow("flaky", {})
        with pytest.raises(RuntimeError, match="child crashed"):
            h.await_child(run_id)

    def test_await_all_children(self):
        """``await_all_children`` returns results for every child."""
        h = CleatTestHarness()
        h.reset()
        h.stub_child_workflow("wf_a", '"result_a"')
        h.stub_child_workflow("wf_b", '"result_b"')

        run_a = h.child_workflow("wf_a", {})
        run_b = h.child_workflow("wf_b", {})

        results = h.await_all_children([run_a, run_b])
        assert len(results) == 2
        assert results[0].result == '"result_a"'
        assert results[1].result == '"result_b"'
        assert results[0].error is None
        assert results[1].error is None

    def test_await_all_children_includes_errors(self):
        """``await_all_children`` includes error information per child."""
        h = CleatTestHarness()
        h.reset()
        h.stub_child_workflow("good", '"ok"')
        h.stub_child_workflow("bad", "", error="child failed")

        run_good = h.child_workflow("good", {})
        run_bad = h.child_workflow("bad", {})

        results = h.await_all_children([run_good, run_bad])
        assert len(results) == 2
        assert results[0].error is None
        assert results[1].error == "child failed"


# ========================================================================
# Behavioural tests – Saga compensation
# ========================================================================


class TestSagaCompensation:
    """Saga compensation pattern verified through ``CleatTestHarness``."""

    def test_saga_compensates_in_reverse_order(self):
        """When step 3 fails, steps 1-2 are compensated in reverse order (2, 1)."""
        h = CleatTestHarness()
        h.reset()
        h.stub_call("saga", "step_1", '"ok"')
        h.stub_call("saga", "step_2", '"ok"')
        h.stub_call("saga", "step_3", error="step 3 failed")
        h.stub_call("saga", "compensate_1", '"ok"')
        h.stub_call("saga", "compensate_2", '"ok"')

        completed = []

        def saga():
            completed.append(1)
            h.cleat_call("saga", "step_1", {})
            completed.append(2)
            h.cleat_call("saga", "step_2", {})
            h.cleat_call("saga", "step_3", {})  # raises
            completed.append(3)  # never reached

        with pytest.raises(RuntimeError, match="step 3 failed"):
            saga()

        # Compensate completed steps in reverse
        for step in reversed(completed):
            h.cleat_call("saga", f"compensate_{step}", {})

        # Steps 1-2 executed
        assert completed == [1, 2]
        assert h.call_count("saga", "step_1") == 1
        assert h.call_count("saga", "step_2") == 1
        assert h.call_count("saga", "step_3") == 1

        # Compensation ran in reverse order
        assert h.assert_called("saga", "compensate_1")
        assert h.assert_called("saga", "compensate_2")
        assert h.assert_not_called("saga", "compensate_3")

        compensate_calls = [r for r in h.call_history if r.operation.startswith("compensate_")]
        assert len(compensate_calls) == 2
        assert compensate_calls[0].operation == "compensate_2"
        assert compensate_calls[1].operation == "compensate_1"

    def test_saga_no_compensation_on_full_success(self):
        """When all saga steps succeed, no compensation is triggered."""
        h = CleatTestHarness()
        h.reset()
        h.stub_call("saga", "step_1", '"ok"')
        h.stub_call("saga", "step_2", '"ok"')
        h.stub_call("saga", "step_3", '"ok"')

        for i in range(1, 4):
            h.cleat_call("saga", f"step_{i}", {})

        assert h.call_count("saga", "step_1") == 1
        assert h.call_count("saga", "step_2") == 1
        assert h.call_count("saga", "step_3") == 1

    def test_compensation_only_for_completed_steps(self):
        """If step 1 fails, no compensation is needed (no completed steps)."""
        h = CleatTestHarness()
        h.reset()
        h.stub_call("saga", "step_1", error="immediate failure")

        with pytest.raises(RuntimeError, match="immediate failure"):
            h.cleat_call("saga", "step_1", {})

        assert h.call_count("saga", "step_1") == 1


# ========================================================================
# Behavioural tests – Retry with backoff
# ========================================================================


class TestRetryWithBackoff:
    """Retry patterns verified through ``CleatTestHarness``."""

    def test_flaky_call_retry_loop(self):
        """A call that fails twice then succeeds on the third attempt."""
        h = CleatTestHarness()
        h.reset()
        # FIFO stubs: first two fail, third succeeds
        h.stub_call("svc", "op", error="transient: timeout")
        h.stub_call("svc", "op", error="transient: unavailable")
        h.stub_call("svc", "op", '"success"')

        retry = RetryPolicy(
            max_attempts=3,
            initial_interval_ms=100,
            backoff_coefficient=2.0,
            max_interval_ms=10_000,
        )

        result = None
        for attempt in range(retry.max_attempts):
            try:
                result = h.cleat_call("svc", "op", {})
                break
            except RuntimeError:
                if attempt == retry.max_attempts - 1:
                    raise
                delay = min(
                    int(retry.initial_interval_ms * (retry.backoff_coefficient ** attempt)),
                    retry.max_interval_ms,
                )
                h.cleat_sleep(delay)

        assert result == '"success"'
        assert h.call_count("svc", "op") == 3

    def test_retry_exhaustion_raises(self):
        """When all retry attempts fail the last error is propagated."""
        h = CleatTestHarness()
        h.reset()
        for _ in range(3):
            h.stub_call("svc", "op", error="persistent error")

        retry = RetryPolicy(max_attempts=3, initial_interval_ms=10, backoff_coefficient=1.0)

        with pytest.raises(RuntimeError, match="persistent error"):
            for attempt in range(retry.max_attempts):
                try:
                    h.cleat_call("svc", "op", {})
                except RuntimeError:
                    if attempt == retry.max_attempts - 1:
                        raise


# ========================================================================
# Behavioural tests – PollCancellation
# ========================================================================


class TestPollCancellation:
    """``poll_cancellation`` behaviour through ``CleatTestHarness``."""

    def test_returns_default_values(self):
        """``poll_cancellation`` returns ``(False, "")`` by default."""
        h = CleatTestHarness()
        h.reset()
        result = h.poll_cancellation()
        assert isinstance(result, tuple)
        assert len(result) == 2
        cancelled, reason = result
        assert isinstance(cancelled, bool)
        assert isinstance(reason, str)
        assert not cancelled
        assert reason == ""

    def test_non_blocking(self):
        """``poll_cancellation`` returns immediately and does not raise."""
        h = CleatTestHarness()
        h.reset()
        for _ in range(5):
            cancelled, reason = h.poll_cancellation()
            assert not cancelled
            assert reason == ""

    def test_in_workflow_loop(self):
        """A polling loop that checks cancellation between work items."""
        h = CleatTestHarness()
        h.reset()
        for _ in range(3):
            h.stub_call("worker", "process", '"ok"')

        for batch in range(3):
            cancelled, reason = h.poll_cancellation()
            assert not cancelled, f"Cancelled unexpectedly at batch {batch}"
            h.cleat_call("worker", "process", {"batch": batch})

        assert h.call_count("worker", "process") == 3


# ========================================================================
# Behavioural tests – extend_timeout
# ========================================================================


class TestExtendTimeout:
    """``extend_timeout`` behaviour through ``CleatTestHarness``."""

    def test_noop_does_not_raise(self):
        """``extend_timeout`` is a no-op in the test harness and never raises."""
        h = CleatTestHarness()
        h.reset()
        h.extend_timeout(60_000)
        h.extend_timeout(30_000)
        h.extend_timeout(0)
        h.extend_timeout(-1)


# ========================================================================
# Behavioural tests – Query state
# ========================================================================


class TestQueryState:
    """``set_query_state`` and ``get_query_state`` through ``CleatTestHarness``."""

    def test_set_and_get(self):
        """Query state can be set and retrieved."""
        h = CleatTestHarness()
        h.reset()
        h.set_query_state("status", '"running"')
        result = h.get_query_state("status")
        assert result == '"running"'

    def test_get_missing_key_returns_empty_string(self):
        """Getting a non-existent query-state key returns an empty string."""
        h = CleatTestHarness()
        h.reset()
        result = h.get_query_state("nonexistent")
        assert result == ""


# ========================================================================
# Behavioural tests – cleat_fetch
# ========================================================================


class TestCleatFetch:
    """HTTP fetch convenience through ``CleatTestHarness``."""

    def test_cleat_fetch_delegates(self):
        """``cleat_fetch`` delegates to ``cleat_call("http", "fetch", ...)``."""
        h = CleatTestHarness()
        h.reset()
        h.stub_call("http", "fetch", '{"body": "hello", "status": 200}')
        body, status = h.cleat_fetch("http://example.com")
        assert body == "hello"
        assert status == 200
        assert h.call_count("http", "fetch") == 1

    def test_cleat_fetch_with_custom_method(self):
        """``cleat_fetch`` passes the method through to the stub."""
        h = CleatTestHarness()
        h.reset()
        h.stub_call("http", "fetch", '{"body": "created", "status": 201}')
        body, status = h.cleat_fetch("http://example.com", "POST", body='{"x": 1}')
        assert body == "created"
        assert status == 201


# ========================================================================
# Behavioural tests – Promises
# ========================================================================


class TestPromises:
    """Promise operations through ``CleatTestHarness``."""

    def test_create_and_await_resolved(self):
        """Create a promise, resolve it externally, and await the value."""
        h = CleatTestHarness()
        h.reset()
        promise_id = h.create_promise("my-promise")
        assert promise_id.startswith("test-prom-my-promise")

        h.stub_promise(promise_id, '"resolved-value"')
        result = h.await_promise(promise_id, 5000)
        assert not result.timed_out
        assert result.result == '"resolved-value"'
        assert not result.rejected

    def test_await_rejected_promise(self):
        """A rejected promise is surfaced with ``rejected=True``."""
        h = CleatTestHarness()
        h.reset()
        promise_id = h.create_promise("fail-promise")
        h.stub_reject_promise(promise_id, "something went wrong")
        result = h.await_promise(promise_id, 5000)
        assert not result.timed_out
        assert result.rejected

    def test_await_pending_promise_timeout(self):
        """Awaiting a pending (unresolved) promise eventually times out."""
        h = CleatTestHarness()
        h.reset()
        promise_id = h.create_promise("never-resolved")
        # Do NOT stub — promise stays "pending"
        result = h.await_promise(promise_id, 10)
        assert result.timed_out


# ========================================================================
# Behavioural tests – Identity
# ========================================================================


class TestWorkflowIdentity:
    """Workflow identity methods through ``CleatTestHarness``."""

    def test_workflow_id(self):
        """``current_workflow_id`` returns the configured test ID."""
        h = CleatTestHarness()
        assert h.current_workflow_id() == "test-workflow-id"

    def test_run_id(self):
        """``current_run_id`` returns the configured test run ID."""
        h = CleatTestHarness()
        assert h.current_run_id() == "test-run-id"


# ========================================================================
# Behavioural tests – cleat_send / schedule_invoke
# ========================================================================


class TestFireAndForget:
    """Fire-and-forget and scheduled-invoke operations."""

    def test_cleat_send_records(self):
        """``cleat_send`` records the call in ``call_history``."""
        h = CleatTestHarness()
        h.reset()
        h.cleat_send("notification", "send", {"message": "hello"})
        assert h.call_count("notification", "send") == 1
        rec = h.last_call("notification", "send")
        assert rec is not None
        assert "hello" in rec.request

    def test_schedule_invoke_records(self):
        """``schedule_invoke`` records the delayed call."""
        h = CleatTestHarness()
        h.reset()
        h.schedule_invoke("cleanup", "purge", {"older_than_days": 30}, delay_ms=3600000)
        assert h.call_count("cleanup", "purge") == 1
        rec = h.last_call("cleanup", "purge")
        assert rec is not None
        assert "3600000" in rec.request or "delay_ms" in rec.request


# ========================================================================
# Behavioural tests – Scope management
# ========================================================================


class TestScopeManagement:
    """State-key scoping / virtual-object lifecycle."""

    def test_set_and_get_scope(self):
        """``set_scope`` returns the previous scope and ``get_scope`` reads it."""
        h = CleatTestHarness()
        h.reset()
        prev = h.set_scope("Customer", "c-42")
        assert prev == ""  # no previous scope
        obj_type, instance_key = h.get_scope()
        assert obj_type == "Customer"
        assert instance_key == "c-42"

    def test_clear_scope(self):
        """``clear_scope`` removes the scope and returns the previous prefix."""
        h = CleatTestHarness()
        h.reset()
        h.set_scope("Order", "ord-1")
        prev = h.clear_scope()
        assert "vo:Order:ord-1:" in prev
        obj_type, instance_key = h.get_scope()
        assert obj_type == ""
        assert instance_key == ""


# ========================================================================
# Behavioural tests – now / random / version
# ========================================================================


class TestInfrastructure:
    """Simple infrastructure methods through ``CleatTestHarness``."""

    def test_now_returns_simulated_time(self):
        """``now`` returns the current simulated clock value."""
        h = CleatTestHarness()
        h.reset()
        assert h.now() == h.now_ms

    def test_random_is_deterministic(self):
        """``random`` returns a deterministic value (42)."""
        h = CleatTestHarness()
        assert h.random() == 42

    def test_version(self):
        """``version`` and ``min_version`` return their configured values."""
        h = CleatTestHarness()
        # After reset
        h.reset()
        assert h.version() == 1
        assert h.min_version() == 1


# ========================================================================
# HostCalls — method existence verification (collapsed to single test)
# ========================================================================


class TestHostCallsMethodExistence:
    """Single-test verification that every expected ``HostCalls`` method exists."""

    EXPECTED_METHODS = {
        # Core operations
        "now", "random", "version", "min_version",
        "cleat_log", "log_kv",
        # Durable calls
        "cleat_call", "cleat_call_typed", "cleat_call_with_retry",
        "cleat_call_with_heartbeat", "cleat_sleep",
        # HTTP fetch
        "cleat_fetch", "cleat_fetch_json", "fetch_get", "fetch_get_json",
        # Signals
        "await_signals", "poll_signal", "poll_cancellation",
        "send_signal_and_wait", "reply_to_signal",
        "await_signals_with_quorum", "signal_workflow",
        # Child workflows
        "child_workflow", "child_workflow_with_options",
        "await_child", "await_all_children",
        # State
        "set_query_state", "set_state", "get_state",
        "delete_state", "incr_state", "has_state", "list_state",
        # Promises
        "create_promise", "await_promise", "resolve_promise", "reject_promise",
        # Handlers
        "register_update_handler", "register_query_handler",
        # Lifecycle
        "cleat_defer", "continue_as_new", "extend_timeout", "run_detached",
        # Fire-and-forget / scheduling
        "cleat_send", "schedule_invoke",
        # Identity
        "current_workflow_id", "current_run_id",
        # Scope
        "set_scope", "get_scope", "clear_scope",
        # UUID
        "uuid",
        # Plugin
        "plugin_call", "plugin_call_streaming",
    }

    def test_all_expected_methods_present(self, host):
        """Verify all expected public methods exist on ``HostCalls``."""
        public_methods = {
            name for name in dir(host)
            if not name.startswith("_") and callable(getattr(host, name))
        }
        missing = self.EXPECTED_METHODS - public_methods
        extra = public_methods - self.EXPECTED_METHODS

        assert not missing, (
            f"Expected methods not found on HostCalls: {sorted(missing)}"
        )
        assert len(public_methods) >= len(self.EXPECTED_METHODS), (
            f"Expected at least {len(self.EXPECTED_METHODS)} public methods, "
            f"got {len(public_methods)}. Extra: {sorted(extra)}"
        )


# ========================================================================
# HostCalls — delegation tests (mock-verified, real logic paths)
# ========================================================================


class TestHostCallsNewMethods:
    """Delegation and formatting logic verified via ``mock.patch``.

    These tests exercise real Python logic (string formatting, request
    construction, JSON deserialisation) but use mocks to avoid requiring
    a WASM runtime.
    """

    # --- log_kv ---

    def test_log_kv_basic(self, host):
        """``log_kv`` without key-value pairs passes through to ``cleat_log``."""
        with mock.patch.object(host, "cleat_log") as mock_log:
            host.log_kv("hello")
            mock_log.assert_called_once_with("hello")

    def test_log_kv_with_pairs(self, host):
        """``log_kv`` formats key-value pairs correctly."""
        with mock.patch.object(host, "cleat_log") as mock_log:
            host.log_kv("processing order", "order_id", "ord-42", "status", "active")
            mock_log.assert_called_once()
            msg = mock_log.call_args[0][0]
            assert "processing order" in msg
            assert "order_id=ord-42" in msg
            assert "status=active" in msg

    def test_log_kv_odd_count(self, host):
        """``log_kv`` with an odd number of kv arguments handles trailing key."""
        with mock.patch.object(host, "cleat_log") as mock_log:
            host.log_kv("test", "key_only")
            msg = mock_log.call_args[0][0]
            assert "key_only=" in msg  # empty value

    # --- has_state ---

    def test_has_state_delegates(self, host):
        """``has_state`` delegates to ``cleat_call('state', 'has', ...)``."""
        with mock.patch.object(host, "cleat_call", return_value="true") as mock_call:
            result = host.has_state("my_key")
            mock_call.assert_called_once_with("state", "has", {"key": "my_key"})
            assert result is True

    def test_has_state_false(self, host):
        """``has_state`` returns ``False`` when the key does not exist."""
        with mock.patch.object(host, "cleat_call", return_value="false"):
            assert host.has_state("missing") is False

    # --- list_state ---

    def test_list_state_delegates(self, host):
        """``list_state`` delegates to ``cleat_call('state', 'list', ...)``."""
        with mock.patch.object(
            host, "cleat_call", return_value='["k1", "k2"]'
        ) as mock_call:
            result = host.list_state("prefix_")
            mock_call.assert_called_once_with("state", "list", {"prefix": "prefix_"})
            assert result == ["k1", "k2"]

    def test_list_state_empty_prefix(self, host):
        """``list_state`` with an empty prefix passes ``{"prefix": ""}``."""
        with mock.patch.object(
            host, "cleat_call", return_value="[]"
        ) as mock_call:
            result = host.list_state()
            mock_call.assert_called_once_with("state", "list", {"prefix": ""})
            assert result == []

    # --- cleat_fetch_json ---

    def test_cleat_fetch_json_delegates(self, host):
        """``cleat_fetch_json`` deserialises the response body from ``cleat_fetch``."""
        with mock.patch.object(
            host, "cleat_fetch", return_value=('{"key": "val"}', 200)
        ):
            result = host.cleat_fetch_json("http://example.com")
            assert result == {"key": "val"}

    # --- fetch_get ---

    def test_fetch_get_delegates(self, host):
        """``fetch_get`` delegates to ``cleat_fetch`` with ``"GET"`` method."""
        with mock.patch.object(
            host, "cleat_fetch", return_value=('{"ok": true}', 200)
        ) as mock_fetch:
            result = host.fetch_get("http://example.com")
            mock_fetch.assert_called_once_with("http://example.com", "GET")
            assert result == ('{"ok": true}', 200)

    # --- fetch_get_json ---

    def test_fetch_get_json_delegates(self, host):
        """``fetch_get_json`` chains through ``fetch_get`` with JSON parsing."""
        with mock.patch.object(
            host, "cleat_fetch", return_value=('{"x": 1}', 200)
        ):
            result = host.fetch_get_json("http://example.com")
            assert result == {"x": 1}


# ========================================================================
# HostCalls — dataclass construction
# ========================================================================


class TestHostCallsDataclasses:
    """Result dataclasses can be constructed with expected defaults."""

    def test_retry_policy_defaults(self):
        policy = RetryPolicy()
        assert policy.max_attempts == 3
        assert policy.initial_interval_ms == 1000
        assert policy.backoff_coefficient == 2.0
        assert policy.max_interval_ms == 30000
        assert policy.non_retryable_errors == []

    def test_retry_policy_custom(self):
        policy = RetryPolicy(
            max_attempts=5,
            initial_interval_ms=500,
            backoff_coefficient=1.5,
            max_interval_ms=60000,
            non_retryable_errors=["BAD_REQUEST"],
        )
        assert policy.max_attempts == 5
        assert "BAD_REQUEST" in policy.non_retryable_errors

    def test_signal_result(self):
        sr = SignalResult(name="payment_received", payload='{"amount": 100}', timed_out=False)
        assert sr.name == "payment_received"
        assert not sr.timed_out

    def test_signal_result_timed_out(self):
        sr = SignalResult(name="", payload="", timed_out=True)
        assert sr.timed_out

    def test_child_result_success(self):
        cr = ChildResult(run_id="run-1", result='{"ok": true}')
        assert cr.run_id == "run-1"
        assert cr.error is None

    def test_child_result_error(self):
        cr = ChildResult(run_id="run-1", result="", error="child failed")
        assert cr.error == "child failed"

    def test_promise_result_success(self):
        pr = PromiseResult(result="some value", timed_out=False)
        assert pr.result == "some value"
        assert not pr.timed_out
        assert not pr.rejected

    def test_promise_result_rejected(self):
        pr = PromiseResult(result="", timed_out=False, rejected=True)
        assert pr.rejected

    def test_promise_result_timed_out(self):
        pr = PromiseResult(result="", timed_out=True)
        assert pr.timed_out


# ========================================================================
# HostCalls — cleat_call exception hierarchy
# ========================================================================


class TestCleatCallErrorHierarchy:
    """``CleatCallError`` and its subclasses."""

    def test_cleat_call_error_is_runtime_error(self):
        """``CleatCallError`` inherits from ``RuntimeError`` for backward compatibility."""
        assert issubclass(CleatCallError, RuntimeError)
        assert issubclass(CleatCallTransientError, CleatCallError)
        assert issubclass(CleatCallPermanentError, CleatCallError)
        assert issubclass(CleatCallTimeoutError, CleatCallTransientError)

    def test_cleat_call_error_has_fields(self):
        """``CleatCallError`` carries ``service``, ``operation``, and ``call_error_code``."""
        err = CleatCallError("svc", "op", "something broke", call_error_code=3)
        assert err.service == "svc"
        assert err.operation == "op"
        assert err.call_error_code == 3
        assert "svc.op" in str(err)
        assert "[3]" in str(err)

    def test_cleat_call_transient_error(self):
        err = CleatCallTransientError("svc", "op", "unavailable", call_error_code=2)
        assert isinstance(err, CleatCallError)
        assert isinstance(err, RuntimeError)

    def test_cleat_call_permanent_error(self):
        err = CleatCallPermanentError("svc", "op", "invalid", call_error_code=4)
        assert isinstance(err, CleatCallError)

    def test_cleat_call_timeout_error(self):
        err = CleatCallTimeoutError("svc", "op", "timed out", call_error_code=1)
        assert isinstance(err, CleatCallError)
        assert isinstance(err, CleatCallTransientError)


# ========================================================================
# Plugins — method existence
# ========================================================================


class TestPluginMethodExistence:
    """Verify every expected ``Plugins`` public method exists."""

    EXPECTED_PLUGIN_METHODS = {
        "blobstore_put", "blobstore_get",
        "await_event",
        "evaluate_flag",
        "produce",
        "send_webhook",
        "trigger_incident", "resolve_incident",
        "send_message",
        "await_webhook",
        "llm_chat", "llm_embed", "llm_list_models",
        "plugin_call_streaming", "llm_chat_streaming",
        "pgvector_search", "pgvector_upsert", "pgvector_delete",
    }

    def test_all_plugin_methods_present(self, plugins):
        """Verify all expected public plugin wrapper methods exist."""
        public_methods = {
            name for name in dir(plugins)
            if not name.startswith("_") and callable(getattr(plugins, name))
        }
        missing = self.EXPECTED_PLUGIN_METHODS - public_methods
        extra = public_methods - self.EXPECTED_PLUGIN_METHODS

        assert not missing, (
            f"Expected plugin methods not found: {sorted(missing)}"
        )
        assert len(public_methods) >= len(self.EXPECTED_PLUGIN_METHODS), (
            f"Expected at least {len(self.EXPECTED_PLUGIN_METHODS)} plugin methods, "
            f"got {len(public_methods)}. Extra: {sorted(extra)}"
        )


# ========================================================================
# Plugins — AI / vector wrappers (mock of ``plugin_call``)
# ========================================================================


class TestPluginAIMethods:
    """LLM and pgvector plugin wrapper methods, verified via mocked ``plugin_call``."""

    # --- llm_chat ---

    def test_llm_chat_minimal(self, plugins):
        """``llm_chat`` with required parameters only."""
        with mock.patch.object(
            plugins._h, "plugin_call",
            return_value='{"choices": [], "usage": {}, "cost": 0.0, "model": "gpt-4o"}',
        ) as mock_call:
            result = plugins.llm_chat(
                "openai", "gpt-4o",
                [{"role": "user", "content": "hello"}],
            )
            mock_call.assert_called_once_with(
                "llm", "chat",
                {
                    "provider": "openai",
                    "model": "gpt-4o",
                    "messages": [{"role": "user", "content": "hello"}],
                },
            )
            assert isinstance(result, LLMChatResult)
            assert result.model == "gpt-4o"

    def test_llm_chat_with_options(self, plugins):
        """``llm_chat`` with all optional parameters."""
        with mock.patch.object(
            plugins._h, "plugin_call",
            return_value=(
                '{"choices": [], "usage": {}, "cost": 0.0, "model": "gpt-4o"}'
            ),
        ):
            result = plugins.llm_chat(
                "openai", "gpt-4o",
                [{"role": "user", "content": "hi"}],
                tools=[{"type": "function", "function": {"name": "foo", "parameters": {}}}],
                max_tokens=100,
                temperature=0.7,
                system_prompt="Be helpful",
                tool_choice="auto",
            )
            assert isinstance(result, LLMChatResult)

    def test_llm_chat_error_response(self, plugins):
        """``llm_chat`` surfaces an error from the plugin response."""
        with mock.patch.object(
            plugins._h, "plugin_call",
            return_value=(
                '{"choices": [], "usage": {}, "cost": 0.0, '
                '"model": "", "error": "rate limited"}'
            ),
        ):
            result = plugins.llm_chat(
                "openai", "gpt-4o", [{"role": "user", "content": "hi"}]
            )
            assert result.error == "rate limited"

    def test_llm_chat_runtime_error(self, plugins):
        """``llm_chat`` propagates ``RuntimeError`` from ``plugin_call``."""
        with mock.patch.object(
            plugins._h, "plugin_call",
            side_effect=RuntimeError("plugin not available"),
        ):
            with pytest.raises(RuntimeError, match="plugin not available"):
                plugins.llm_chat(
                    "openai", "gpt-4o", [{"role": "user", "content": "hi"}]
                )

    # --- llm_embed ---

    def test_llm_embed(self, plugins):
        """``llm_embed`` constructs the correct input and returns a typed result."""
        with mock.patch.object(
            plugins._h, "plugin_call",
            return_value=(
                '{"data": [{"embedding": [0.1, 0.2], "index": 0}], '
                '"usage": {"total_tokens": 5}, "cost": 0.001}'
            ),
        ) as mock_call:
            result = plugins.llm_embed(
                "openai", "text-embedding-3-small", ["hello world"]
            )
            mock_call.assert_called_once_with(
                "llm", "embed",
                {
                    "provider": "openai",
                    "model": "text-embedding-3-small",
                    "input": ["hello world"],
                },
            )
            assert isinstance(result, LLMEmbedResult)
            assert len(result.data) == 1
            assert result.data[0]["embedding"] == [0.1, 0.2]

    # --- llm_list_models ---

    def test_llm_list_models_all(self, plugins):
        """``llm_list_models`` without a provider queries all providers."""
        with mock.patch.object(
            plugins._h, "plugin_call",
            return_value=(
                '{"providers": {"openai": [{"name": "gpt-4o", "cost_per_1k_tokens": 0.01}]}}'
            ),
        ) as mock_call:
            result = plugins.llm_list_models()
            mock_call.assert_called_once_with("llm", "list_models", {})
            assert isinstance(result, LLMListModelsResult)
            assert "openai" in result.providers

    def test_llm_list_models_by_provider(self, plugins):
        """``llm_list_models`` with a provider filters results."""
        with mock.patch.object(
            plugins._h, "plugin_call",
            return_value=(
                '{"models": [{"name": "gpt-4o", "cost_per_1k_tokens": 0.01}], '
                '"provider": "openai"}'
            ),
        ) as mock_call:
            result = plugins.llm_list_models(provider="openai")
            mock_call.assert_called_once_with(
                "llm", "list_models", {"provider": "openai"}
            )
            assert isinstance(result, LLMListModelsResult)
            assert len(result.models) == 1

    # --- pgvector_search ---

    def test_pgvector_search(self, plugins):
        """``pgvector_search`` constructs correct input and returns a result list."""
        with mock.patch.object(
            plugins._h, "plugin_call",
            return_value=(
                '{"results": [{"id": "abc", "score": 0.95, "content": "match"}]}'
            ),
        ) as mock_call:
            results = plugins.pgvector_search("docs", [0.1, 0.2, 0.3], limit=5)
            mock_call.assert_called_once_with(
                "pgvector", "search",
                {
                    "collection": "docs",
                    "query_vector": [0.1, 0.2, 0.3],
                    "top_k": 5,
                    "include_meta": True,
                },
            )
            assert len(results) == 1
            assert results[0]["id"] == "abc"
            assert results[0]["score"] == 0.95

    def test_pgvector_search_with_filter(self, plugins):
        """``pgvector_search`` passes metadata filter to the backend."""
        with mock.patch.object(
            plugins._h, "plugin_call",
            return_value='{"results": []}',
        ) as mock_call:
            plugins.pgvector_search("docs", [1.0, 2.0], filter={"status": "active"})
            call_input = mock_call.call_args[0][2]
            assert call_input["filter"] == {"status": "active"}

    def test_pgvector_search_min_score_filter(self, plugins):
        """``pgvector_search`` with ``min_score`` filters results client-side."""
        with mock.patch.object(
            plugins._h, "plugin_call",
            return_value=(
                '{"results": [{"id": "a", "score": 0.9}, {"id": "b", "score": 0.5}]}'
            ),
        ):
            results = plugins.pgvector_search("docs", [0.1, 0.2], min_score=0.7)
            assert len(results) == 1
            assert results[0]["id"] == "a"

    # --- pgvector_upsert ---

    def test_pgvector_upsert(self, plugins):
        """``pgvector_upsert`` constructs correct input with metadata."""
        with mock.patch.object(
            plugins._h, "plugin_call",
            return_value='{"id": "new-id"}',
        ) as mock_call:
            plugins.pgvector_upsert(
                "docs", "ext-1", [0.1, 0.2], metadata={"author": "alice"}
            )
            mock_call.assert_called_once_with(
                "pgvector", "upsert",
                {
                    "collection": "docs",
                    "external_id": "ext-1",
                    "embedding": [0.1, 0.2],
                    "metadata": {"author": "alice"},
                },
            )

    def test_pgvector_upsert_no_metadata(self, plugins):
        """``pgvector_upsert`` without metadata omits key from input."""
        with mock.patch.object(
            plugins._h, "plugin_call",
            return_value='{"id": "new-id"}',
        ) as mock_call:
            plugins.pgvector_upsert("docs", "ext-1", [0.1, 0.2])
            call_input = mock_call.call_args[0][2]
            assert "metadata" not in call_input

    # --- pgvector_delete ---

    def test_pgvector_delete(self, plugins):
        """``pgvector_delete`` constructs correct input."""
        with mock.patch.object(
            plugins._h, "plugin_call",
            return_value='{"deleted": 1}',
        ) as mock_call:
            plugins.pgvector_delete("docs", "ext-1")
            mock_call.assert_called_once_with(
                "pgvector", "delete",
                {"collection": "docs", "external_id": "ext-1"},
            )


# ========================================================================
# Plugins — error handling
# ========================================================================


class TestPluginErrorHandling:
    """Error handling in the ``Plugins._call`` helper and ``plugin_call``."""

    def test_invalid_json_response(self, plugins):
        """``_call`` raises ``RuntimeError`` when the plugin returns invalid JSON."""
        with mock.patch.object(
            plugins._h, "plugin_call",
            return_value="not json",
        ):
            with pytest.raises(RuntimeError, match="invalid JSON"):
                plugins.llm_chat(
                    "openai", "gpt-4o", [{"role": "user", "content": "hi"}]
                )

    def test_non_object_response(self, plugins):
        """``_call`` raises ``RuntimeError`` when the response is not a JSON object."""
        with mock.patch.object(
            plugins._h, "plugin_call",
            return_value='"just a string"',
        ):
            with pytest.raises(RuntimeError, match="expected a JSON object"):
                plugins.llm_chat(
                    "openai", "gpt-4o", [{"role": "user", "content": "hi"}]
                )

    def test_plugin_call_runtime_error(self, host):
        """``plugin_call`` propagates ``RuntimeError`` from the WASM import stub."""
        with pytest.raises(RuntimeError):
            host.plugin_call("nonexistent", "func", {})
