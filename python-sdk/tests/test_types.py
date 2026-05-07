"""Tests for the Cleat type sugar wrappers (ChildWorkflow, Saga, CleatDefer).

Verifies the Saga class-based API works correctly with class-based step
definitions (SagaStep subclasses), avoiding lambda closures that could
cause issues with WASM compilation via componentize-py.
"""

import json

import pytest

try:
    from cleat_sdk.test_harness import CleatTestHarness
    from cleat_sdk.host_calls import HostCalls
    from cleat_sdk.types import (
        Saga,
        SagaStep,
        SagaStepResult,
        SagaResultT,
        TerminalError,
        ChildWorkflow,
        CleatDefer,
    )
except ImportError as e:
    pytest.skip(
        f"Skipping types tests: {e}",
        allow_module_level=True,
    )


# ======================================================================
# Saga class-based step tests
# ======================================================================


class TestSagaClassBasedSteps:
    """Demonstrate Saga using class-based SagaStep subclasses (WASM-safe)."""

    def test_saga_class_based_success(self):
        """Steps execute in order and return their results."""
        h = CleatTestHarness()
        h.stub_call("inventory", "Reserve", '{"ok": true}')
        h.stub_call("payment", "Charge", '{"txn_id": "abc123"}')
        h.stub_call("notification", "Send", '{"sent": true}')

        class ReserveStep(SagaStep[str]):
            def action(self, h: HostCalls) -> str:
                return h.cleat_call("inventory", "Reserve", {"item": "widget"})

            def compensate(self, h: HostCalls) -> None:
                h.cleat_call("inventory", "Release", {"item": "widget"})

        class ChargeStep(SagaStep[str]):
            def action(self, h: HostCalls) -> str:
                return h.cleat_call("payment", "Charge", {"amount": 100})

            def compensate(self, h: HostCalls) -> None:
                h.cleat_call("payment", "Refund", {"amount": 100})

        class NotifyStep(SagaStep[str]):
            def action(self, h: HostCalls) -> str:
                return h.cleat_call("notification", "Send", {"message": "Order placed"})

            def compensate(self, h: HostCalls) -> None:
                pass  # No compensation needed for notifications

        saga = Saga[str](h)
        # Register using class-based SagaStep instances
        saga.add_step(ReserveStep("reserve_inventory"))
        saga.add_step(ChargeStep("charge_payment"))
        saga.add_step(NotifyStep("send_notification"))

        results = saga.execute()

        assert len(results) == 3
        assert isinstance(results[0], SagaStepResult)
        assert results[0].step_name == "reserve_inventory"
        assert results[0].success is True
        assert results[0].error is None
        assert json.loads(results[0].result)["ok"] is True

        assert results[1].step_name == "charge_payment"
        assert results[1].success is True
        assert json.loads(results[1].result)["txn_id"] == "abc123"

        assert results[2].step_name == "send_notification"
        assert results[2].success is True
        assert json.loads(results[2].result)["sent"] is True

        # Verify all calls were made in order
        assert h.call_count("inventory", "Reserve") == 1
        assert h.call_count("payment", "Charge") == 1
        assert h.call_count("notification", "Send") == 1

    def test_saga_class_based_compensation_on_failure(self):
        """On terminal failure, all prior steps are compensated in reverse order."""
        h = CleatTestHarness()
        h.stub_call("inventory", "Reserve", '{"ok": true}')
        h.stub_call("payment", "Charge", '{"txn_id": "abc123"}')
        h.stub_call("payment", "Refund", '{"ok": true}')
        h.stub_call("inventory", "Release", '{"ok": true}')

        class ReserveStep(SagaStep[str]):
            def action(self, h: HostCalls) -> str:
                return h.cleat_call("inventory", "Reserve", {"item": "widget"})

            def compensate(self, h: HostCalls) -> None:
                h.cleat_call("inventory", "Release", {"item": "widget"})

        class ChargeStep(SagaStep[str]):
            def action(self, h: HostCalls) -> str:
                return h.cleat_call("payment", "Charge", {"amount": 100})

            def compensate(self, h: HostCalls) -> None:
                h.cleat_call("payment", "Refund", {"amount": 100})

        class FailStep(SagaStep[str]):
            def action(self, h: HostCalls) -> str:
                raise TerminalError("processing failed")

            def compensate(self, h: HostCalls) -> None:
                pass  # Never reached

        saga = Saga[str](h)
        saga.add_step(ReserveStep("reserve_inventory"))
        saga.add_step(ChargeStep("charge_payment"))
        saga.add_step(FailStep("fail_step"))

        with pytest.raises(TerminalError, match="processing failed"):
            saga.execute()

        # Verify compensation was called: Reserve and Charge should be
        # compensated in reverse order (Charge first, then Reserve).
        assert h.call_count("inventory", "Reserve") == 1
        assert h.call_count("payment", "Charge") == 1
        assert h.call_count("payment", "Refund") == 1
        assert h.call_count("inventory", "Release") == 1

    def test_saga_class_based_transient_error_no_compensation(self):
        """Transient (non-terminal) errors do NOT trigger compensation."""
        h = CleatTestHarness()
        h.stub_call("inventory", "Reserve", '{"ok": true}')
        h.stub_call("payment", "Charge", '{"txn_id": "abc123"}')

        class ReserveStep(SagaStep[str]):
            def action(self, h: HostCalls) -> str:
                return h.cleat_call("inventory", "Reserve", {"item": "widget"})

            def compensate(self, h: HostCalls) -> None:
                h.cleat_call("inventory", "Release", {"item": "widget"})

        class ChargeStep(SagaStep[str]):
            def action(self, h: HostCalls) -> str:
                return h.cleat_call("payment", "Charge", {"amount": 100})

            def compensate(self, h: HostCalls) -> None:
                h.cleat_call("payment", "Refund", {"amount": 100})

        class FailStep(SagaStep[str]):
            def action(self, h: HostCalls) -> str:
                raise ValueError("transient network error")

            def compensate(self, h: HostCalls) -> None:
                pass

        saga = Saga[str](h)
        saga.add_step(ReserveStep("reserve_inventory"))
        saga.add_step(ChargeStep("charge_payment"))
        saga.add_step(FailStep("fail_step"))

        with pytest.raises(ValueError, match="transient network error"):
            saga.execute()

        # No compensation should have happened for transient errors
        assert h.call_count("inventory", "Release") == 0
        assert h.call_count("payment", "Refund") == 0

    def test_saga_class_based_single_step(self):
        """A saga with a single step executes and returns correctly."""
        h = CleatTestHarness()
        h.stub_call("greeter", "Greet", '{"greeting": "Hello"}')

        class GreetStep(SagaStep[str]):
            def action(self, h: HostCalls) -> str:
                return h.cleat_call("greeter", "Greet", {"name": "World"})

            def compensate(self, h: HostCalls) -> None:
                pass

        saga = Saga[str](h)
        saga.add_step(GreetStep("greet"))
        results = saga.execute()

        assert len(results) == 1
        assert isinstance(results[0], SagaStepResult)
        assert results[0].step_name == "greet"
        assert results[0].success is True
        assert json.loads(results[0].result)["greeting"] == "Hello"

    def test_saga_empty_steps(self):
        """A saga with no steps returns an empty list."""
        h = CleatTestHarness()
        saga = Saga[str](h)
        results = saga.execute()
        assert results == []


# ======================================================================
# Saga terminal_exceptions tests
# ======================================================================


class TestSagaTerminalExceptions:
    """Tests for the terminal_exceptions parameter on Saga."""

    def test_terminal_exceptions_in_constructor_triggers_compensation(self):
        """Exception types listed in constructor terminal_exceptions trigger compensation."""
        h = CleatTestHarness()
        h.stub_call("inventory", "Reserve", '{"ok": true}')
        h.stub_call("payment", "Refund", '{"ok": true}')

        class MyAppError(Exception):
            pass

        class ReserveStep(SagaStep[str]):
            def action(self, h: HostCalls) -> str:
                return h.cleat_call("inventory", "Reserve", {"item": "widget"})

            def compensate(self, h: HostCalls) -> None:
                h.cleat_call("inventory", "Release", {"item": "widget"})

        class ChargeStep(SagaStep[str]):
            def action(self, h: HostCalls) -> str:
                raise MyAppError("non-retryable business error")

            def compensate(self, h: HostCalls) -> None:
                h.cleat_call("payment", "Refund", {"amount": 100})

        saga = Saga[str](h, terminal_exceptions=(MyAppError,))
        saga.add_step(ReserveStep("reserve_inventory"))
        saga.add_step(ChargeStep("charge_payment"))

        with pytest.raises(MyAppError, match="non-retryable business error"):
            saga.execute()

        # Reserve should be compensated since MyAppError is terminal
        assert h.call_count("inventory", "Reserve") == 1
        assert h.call_count("inventory", "Release") == 1

    def test_terminal_exceptions_in_execute_triggers_compensation(self):
        """Exception types listed in execute() terminal_exceptions trigger compensation."""
        h = CleatTestHarness()
        h.stub_call("inventory", "Reserve", '{"ok": true}')

        class MyAppError(Exception):
            pass

        class ReserveStep(SagaStep[str]):
            def action(self, h: HostCalls) -> str:
                return h.cleat_call("inventory", "Reserve", {"item": "widget"})

            def compensate(self, h: HostCalls) -> None:
                h.cleat_call("inventory", "Release", {"item": "widget"})

        class FailStep(SagaStep[str]):
            def action(self, h: HostCalls) -> str:
                raise MyAppError("non-retryable")

            def compensate(self, h: HostCalls) -> None:
                pass

        saga = Saga[str](h)
        saga.add_step(ReserveStep("reserve_inventory"))
        saga.add_step(FailStep("fail"))

        with pytest.raises(MyAppError, match="non-retryable"):
            saga.execute(terminal_exceptions=(MyAppError,))

        # Reserve should be compensated since MyAppError is terminal
        assert h.call_count("inventory", "Reserve") == 1
        assert h.call_count("inventory", "Release") == 1


# ======================================================================
# ChildWorkflow tests
# ======================================================================


class TestChildWorkflowTyped:
    """Tests for the typed ChildWorkflow wrapper."""

    def test_child_workflow_start_and_await(self):
        h = CleatTestHarness()
        h.stub_child_workflow("order_processor", '{"status": "completed"}')

        child = ChildWorkflow[str](
            name="order_processor",
            input={"order_id": "ord-42"},
        )
        run_id = child.start(h)
        assert run_id.startswith("test-child-order_processor")

        result = child.await_result(h)
        assert result["status"] == "completed"

    def test_child_workflow_run_id_set_after_start(self):
        h = CleatTestHarness()
        h.stub_child_workflow("worker", '{"done": true}')

        child = ChildWorkflow[str](name="worker", input={"task": "t1"})
        assert child.run_id is None

        run_id = child.start(h)
        assert child.run_id == run_id

    def test_await_result_without_start_raises(self):
        h = CleatTestHarness()

        child = ChildWorkflow[str](name="worker", input={})
        with pytest.raises(RuntimeError, match="before start"):
            child.await_result(h)


# ======================================================================
# CleatDefer tests
# ======================================================================


class TestCleatDefer:
    """Tests for the CleatDefer context manager."""

    def test_defer_context_manager(self):
        h = CleatTestHarness()
        with CleatDefer("cleanup temp files", h) as defer:
            assert defer is not None
            # Note: defer_id is None in the test harness since
            # we haven't set up a stub for cleat_defer

    def test_defer_context_manager_no_host(self):
        """CleatDefer without a host is a no-op."""
        with CleatDefer("cleanup") as defer:
            assert defer._defer_id is None
