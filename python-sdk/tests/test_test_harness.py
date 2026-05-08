"""Tests for the CleatTestHarness mock HostCalls.

Verifies stub registration, call recording, clock control, signal delivery,
child workflow stubs, promise resolution, and state operations.
"""

import json
import time

import pytest

try:
    from cleat_sdk.test_harness import CleatTestHarness, CallRecord
    from cleat_sdk.host_calls import HostCalls, RetryPolicy, SignalResult, PromiseResult, ChildResult, SuspendSentinel
except ImportError as e:
    pytest.skip(
        f"Skipping test harness tests: {e}",
        allow_module_level=True,
    )


# ======================================================================
# Test harness basic setup
# ======================================================================


class TestCleatTestHarnessInit:
    """Verify initial state of the test harness."""

    def test_init_creates_empty_state(self):
        h = CleatTestHarness()
        assert h.now_ms >= 1704067200000  # Some time in 2024
        assert h.call_history == []
        assert h._call_stubs == []
        assert h._pending_signals == []
        assert h._promises == {}

    def test_workflow_id_defaults(self):
        h = CleatTestHarness()
        assert h.current_workflow_id() == "test-workflow-id"
        assert h.current_run_id() == "test-run-id"


# ======================================================================
# Stub configuration
# ======================================================================


class TestStubCall:
    """Tests for stub_call and cleat_call."""

    def test_stub_call_returns_response(self):
        h = CleatTestHarness()
        h.stub_call("greeter", "Greet", '{"greeting": "Hello"}')
        result = h.cleat_call("greeter", "Greet", {"name": "World"})
        assert result == '{"greeting": "Hello"}'

    def test_stub_call_records_history(self):
        h = CleatTestHarness()
        h.stub_call("payment", "Charge", '{"status": "ok"}')
        h.cleat_call("payment", "Charge", {"amount": 100})
        records = h.call_history
        assert len(records) == 1
        assert records[0].service == "payment"
        assert records[0].operation == "Charge"
        assert "amount" in records[0].request

    def test_stub_call_consumes_fifo(self):
        h = CleatTestHarness()
        h.stub_call("svc", "op", "resp1")
        h.stub_call("svc", "op", "resp2")
        assert h.cleat_call("svc", "op", "") == "resp1"
        assert h.cleat_call("svc", "op", "") == "resp2"

    def test_stub_call_raises_error(self):
        h = CleatTestHarness()
        h.stub_call("svc", "op", "", error="service unavailable")
        with pytest.raises(RuntimeError, match="service unavailable"):
            h.cleat_call("svc", "op", "")

    def test_stub_call_no_stub_raises(self):
        h = CleatTestHarness()
        with pytest.raises(RuntimeError, match="no stub registered"):
            h.cleat_call("unknown", "op", "")

    def test_call_count(self):
        h = CleatTestHarness()
        h.stub_call("a", "x", "1")
        h.stub_call("a", "x", "2")
        h.stub_call("a", "y", "3")
        h.cleat_call("a", "x", "")
        h.cleat_call("a", "x", "")
        h.cleat_call("a", "y", "")
        assert h.call_count("a", "x") == 2
        assert h.call_count("a", "y") == 1
        assert h.call_count("b", "z") == 0

    def test_assert_called(self):
        h = CleatTestHarness()
        h.stub_call("svc", "op", "")
        assert not h.assert_called("svc", "op")
        h.cleat_call("svc", "op", "")
        assert h.assert_called("svc", "op")

    def test_assert_not_called(self):
        h = CleatTestHarness()
        assert h.assert_not_called("svc", "op")
        h.stub_call("svc", "op", "")
        h.cleat_call("svc", "op", "")
        assert not h.assert_not_called("svc", "op")

    def test_last_call(self):
        h = CleatTestHarness()
        h.stub_call("svc", "op", "resp1")
        h.stub_call("svc", "op", "resp2")
        h.cleat_call("svc", "op", "req1")
        h.cleat_call("svc", "op", "req2")
        last = h.last_call("svc", "op")
        assert last is not None
        assert last.response == "resp2"
        assert "req2" in last.request


# ======================================================================
# Clock control
# ======================================================================


class TestClockControl:
    """Tests for advance_time and set_time."""

    def test_advance_time_increases_now(self):
        h = CleatTestHarness()
        before = h.now_ms
        h.advance_time(5000)
        assert h.now_ms - before == 5000

    def test_set_time_absolute(self):
        h = CleatTestHarness()
        h.set_time(1000000)
        assert h.now_ms == 1000000

    def test_sleep_advances_clock(self):
        h = CleatTestHarness()
        before = h.now_ms
        h.cleat_sleep(3000)
        assert h.now_ms - before == 3000


# ======================================================================
# Signal handling
# ======================================================================


class TestSignals:
    """Tests for stub_signal, poll_signal, await_signals."""

    def test_stub_signal_immediately_available(self):
        h = CleatTestHarness()
        h.stub_signal("payment_received", '{"amount": 100}')
        payload, found = h.poll_signal("payment_received")
        assert found
        assert json.loads(payload)["amount"] == 100

    def test_poll_signal_not_found(self):
        h = CleatTestHarness()
        payload, found = h.poll_signal("nonexistent")
        assert not found
        assert payload == ""

    def test_await_signals_immediate(self):
        h = CleatTestHarness()
        h.stub_signal("approved", '{"ok": true}')
        result = h.await_signals(["approved"], 5000)
        assert not result.timed_out
        assert result.name == "approved"

    def test_await_signals_timeout(self):
        h = CleatTestHarness()
        result = h.await_signals(["never_arrives"], 100)
        assert result.timed_out
        assert result.name == ""
        assert result.payload == ""

    def test_await_signals_indefinite_timeout_raises_suspend(self):
        """timeout_ms=0 means wait indefinitely, which raises SuspendSentinel
        in the test harness when no signal is available."""
        h = CleatTestHarness()
        with pytest.raises(SuspendSentinel):
            h.await_signals(["anything"], 0)
        with pytest.raises(SuspendSentinel):
            h.await_signals(["anything"], -1)

    def test_await_signals_indefinite_with_signal(self):
        """With a signal available, indefinite wait returns immediately."""
        h = CleatTestHarness()
        h.stub_signal("greeting", "hello")
        result = h.await_signals(["greeting"], 0)
        assert not result.timed_out
        assert result.name == "greeting"
        assert result.payload == "hello"

    def test_poll_signal_still_available_after_await(self):
        h = CleatTestHarness()
        h.stub_signal("sig1", "data")
        h.stub_signal("sig2", "more")
        # First poll consumes sig1
        payload, found = h.poll_signal("sig1")
        assert found
        # sig2 is still there
        payload2, found2 = h.poll_signal("sig2")
        assert found2


# ======================================================================
# Promises
# ======================================================================


class TestPromises:
    """Tests for create_promise, await_promise, stub_promise."""

    def test_create_promise(self):
        h = CleatTestHarness()
        pid = h.create_promise("approval")
        assert pid.startswith("test-prom-approval")

    def test_await_pending_promise(self):
        h = CleatTestHarness()
        pid = h.create_promise("wait")
        result = h.await_promise(pid, 100)
        assert result.timed_out
        assert result.result == ""

    def test_stub_promise_immediate(self):
        h = CleatTestHarness()
        pid = h.create_promise("done")
        h.stub_promise(pid, '{"status": "ok"}')
        result = h.await_promise(pid, 1000)
        assert not result.timed_out
        assert not result.rejected
        assert "ok" in result.result

    def test_stub_promise_before_create(self):
        h = CleatTestHarness()
        h.stub_promise("pre-existing", "value")
        pid = h.create_promise("pre-existing")
        result = h.await_promise(pid, 1000)
        assert not result.timed_out
        assert result.result == "value"

    def test_stub_reject_promise(self):
        h = CleatTestHarness()
        pid = h.create_promise("fail")
        h.stub_reject_promise(pid, "something went wrong")
        result = h.await_promise(pid, 1000)
        assert result.rejected

    def test_resolve_promise(self):
        h = CleatTestHarness()
        pid = h.create_promise("manual")
        h.resolve_promise(pid, "resolved_value")
        result = h.await_promise(pid, 1000)
        assert not result.timed_out
        assert result.result == "resolved_value"

    def test_reject_promise(self):
        h = CleatTestHarness()
        pid = h.create_promise("manual_reject")
        h.reject_promise(pid, "manual error")
        result = h.await_promise(pid, 1000)
        assert result.rejected


# ======================================================================
# Child workflows
# ======================================================================


class TestChildWorkflows:
    """Tests for stub_child_workflow, child_workflow, await_child."""

    def test_stub_child_workflow(self):
        h = CleatTestHarness()
        h.stub_child_workflow("process_item", '{"status": "done"}')
        run_id = h.child_workflow("process_item", "item-1")
        assert run_id.startswith("test-child-process_item")

    def test_await_child_success(self):
        h = CleatTestHarness()
        h.stub_child_workflow("process_item", '{"status": "done"}')
        run_id = h.child_workflow("process_item", "item-1")
        result = h.await_child(run_id)
        assert "done" in result

    def test_await_child_error(self):
        h = CleatTestHarness()
        h.stub_child_workflow("fail_process", "", error="processing failed")
        run_id = h.child_workflow("fail_process", "item-1")
        with pytest.raises(RuntimeError, match="processing failed"):
            h.await_child(run_id)

    def test_await_all_children(self):
        h = CleatTestHarness()
        h.stub_child_workflow("child_a", '{"result": "a"}')
        h.stub_child_workflow("child_b", '{"result": "b"}')
        rid_a = h.child_workflow("child_a", "")
        rid_b = h.child_workflow("child_b", "")
        results = h.await_all_children([rid_a, rid_b])
        assert len(results) == 2
        assert results[0].result == '{"result": "a"}'
        assert results[1].result == '{"result": "b"}'

    def test_await_all_children_with_error(self):
        h = CleatTestHarness()
        h.stub_child_workflow("good", '{"ok": true}')
        h.stub_child_workflow("bad", "", error="boom")
        rid_good = h.child_workflow("good", "")
        rid_bad = h.child_workflow("bad", "")
        results = h.await_all_children([rid_good, rid_bad])
        assert len(results) == 2
        assert results[0].error is None
        assert results[1].error is not None


# ======================================================================
# State management
# ======================================================================


class TestState:
    """Tests for state operations through the test harness."""

    def test_set_and_get_state(self):
        h = CleatTestHarness()
        h.stub_call("state", "set", '{"ok": true}')
        h.stub_call("state", "get", '{"value": "stored"}')
        h.set_state("mykey", "myvalue")
        result = h.get_state("mykey", str)
        assert result == "stored"

    def test_incr_state(self):
        h = CleatTestHarness()
        h.stub_call("state", "incr", "5")
        val = h.incr_state("counter", 1)
        assert val == 5

    def test_delete_state(self):
        h = CleatTestHarness()
        h.stub_call("state", "delete", '{"ok": true}')
        h.delete_state("mykey")

    def test_set_query_state(self):
        h = CleatTestHarness()
        h.set_query_state("status", '"active"')
        val = h.get_query_state("status")
        assert val == '"active"'


# ======================================================================
# Durable send / schedule / defer / continue_as_new
# ======================================================================


class TestLifecycle:
    """Tests for send, schedule, defer, continue_as_new."""

    def test_cleat_send(self):
        h = CleatTestHarness()
        h.cleat_send("email", "notify", {"user": "u1"})
        assert h.call_count("email", "notify") == 1

    def test_schedule_invoke(self):
        h = CleatTestHarness()
        h.schedule_invoke("queue", "process", {"task": "t1"}, 5000)
        assert h.call_count("queue", "process") == 1

    def test_cleat_defer(self):
        h = CleatTestHarness()
        defer_id = h.cleat_defer("cleanup temp files")
        assert defer_id.startswith("test-defer")

    def test_continue_as_new(self):
        h = CleatTestHarness()
        # Should not raise
        h.continue_as_new({"new_input": "data"})


# ======================================================================
# Reset
# ======================================================================


class TestReset:
    """Tests for the reset() method."""

    def test_reset_clears_everything(self):
        h = CleatTestHarness()
        h.stub_call("svc", "op", "resp")
        h.cleat_call("svc", "op", "req")
        h.stub_signal("sig", "payload")
        pid = h.create_promise("p")
        h.now_ms += 9999

        h.reset()

        assert h.now_ms == h._start_ms
        assert h.call_history == []
        assert h._call_stubs == []
        assert h._pending_signals == []
        assert h._promises == {}


# ======================================================================
# Integration: run a simple workflow
# ======================================================================


class TestWorkflowIntegration:
    """Integration test running a workflow function through the harness."""

    def test_hello_workflow(self):
        """Simulate the hello_workflow example."""
        h = CleatTestHarness()
        h.stub_call("greeter", "Greet", '{"greeting": "Hello, World"}')
        h.stub_call("greeter", "Greet", '{"greeting": "Hello, Alice"}')

        def hello_workflow(h: HostCalls, name: str) -> str:
            h.cleat_log(f"Hello workflow started for {name}")
            response = h.cleat_call(
                "greeter", "Greet",
                {"name": name, "language": "en"}
            )
            return response

        result = hello_workflow(h, "World")
        assert json.loads(result)["greeting"] == "Hello, World"

        result2 = hello_workflow(h, "Alice")
        assert json.loads(result2)["greeting"] == "Hello, Alice"

    def test_workflow_error_handling(self):
        """Workflow that handles a call error."""
        h = CleatTestHarness()
        h.stub_call("payment", "Charge", "", error="insufficient funds")

        def charge_workflow(h: HostCalls, amount: int) -> str:
            try:
                result = h.cleat_call("payment", "Charge", {"amount": amount})
                return result
            except RuntimeError as e:
                return json.dumps({"error": str(e)})

        result = charge_workflow(h, 100)
        assert "insufficient funds" in result


# ======================================================================
# Edge cases
# ======================================================================


class TestEdgeCases:
    """Edge case tests for the test harness."""

    def test_multiple_calls_different_services(self):
        h = CleatTestHarness()
        h.stub_call("a", "op1", "r1")
        h.stub_call("b", "op2", "r2")
        h.stub_call("a", "op1", "r3")

        assert h.cleat_call("a", "op1", "") == "r1"
        assert h.cleat_call("b", "op2", "") == "r2"
        assert h.cleat_call("a", "op1", "") == "r3"

    def test_cleat_call_with_retry_delegates(self):
        h = CleatTestHarness()
        h.stub_call("svc", "op", "ok")
        result = h.cleat_call_with_retry(
            "svc", "op", {},
            RetryPolicy(max_attempts=3)
        )
        assert result == "ok"

    def test_cleat_fetch_delegates(self):
        h = CleatTestHarness()
        h.stub_call("http", "fetch", '{"body": "response body", "status": 200}')
        body, status = h.cleat_fetch("https://example.com")
        assert body == "response body"
        assert status == 200

    def test_now_returns_simulated_time(self):
        h = CleatTestHarness()
        now = h.now()
        assert now >= 1704067200000

    def test_random_is_deterministic(self):
        h = CleatTestHarness()
        assert h.random() == 42
        assert h.random() == 42  # Same value every time

    def test_version_and_min_version(self):
        h = CleatTestHarness()
        assert h.version() == 1
        assert h.min_version() == 1

    def test_last_call_none_when_no_match(self):
        h = CleatTestHarness()
        rec = h.last_call("nonexistent", "op")
        assert rec is None
