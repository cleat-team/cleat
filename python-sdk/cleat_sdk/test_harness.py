"""
Mock HostCalls subclass with configurable stubs for WASM-free testing.

Provides the :class:`CleatTestHarness` class, a drop-in replacement for
:class:`HostCalls` that records all calls and lets you stub responses,
deliver signals manually, and control the simulated clock without
compiling to WASM or running a host runtime.

Usage::

    from cleat_sdk.test_harness import CleatTestHarness

    def test_my_workflow():
        h = CleatTestHarness()

        # Stub a durable call response
        h.stub_call("greeter", "Greet", '{"greeting": "Hello, World"}')

        # Run the workflow function directly (no WASM needed)
        result = my_workflow(h, GreetingRequest(name="World"))

        # Assert the call was actually made
        assert h.call_count("greeter", "Greet") == 1
        assert result == '{"greeting": "Hello, World"}'

Port of the Go pattern from ``durable/durabletest/durabletest.go``.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from typing import Any, Callable, Optional

from .host_calls import (
    HostCalls,
    ChildResult,
    PromiseResult,
    RetryPolicy,
    SignalResult,
    SuspendSentinel,
)
from .memory import SUSPEND_SENTINEL


# ---------------------------------------------------------------------------
# Call record for test assertions
# ---------------------------------------------------------------------------


@dataclass
class CallRecord:
    """A single recorded call made through the test harness.

    Attributes
    ----------
    service:
        The service name called (e.g. ``"payment"``).
    operation:
        The operation name called (e.g. ``"charge"``).
    request:
        The request string (JSON for durable calls).
    response:
        The configured stub response string.
    error:
        The configured stub error, or ``None``.
    """

    service: str
    operation: str
    request: str
    response: str = ""
    error: Optional[str] = None


# ---------------------------------------------------------------------------
# Stub types
# ---------------------------------------------------------------------------


@dataclass
class _CallStub:
    """Internal stub record for a durable_call."""

    service: str
    operation: str
    response: str
    error: Optional[str] = None


@dataclass
class _ChildStub:
    """Internal stub record for a child workflow."""

    name: str
    result: str
    error: Optional[str] = None


@dataclass
class _SignalStub:
    """A pending signal waiting to be delivered."""

    name: str
    payload: str
    deliver_at_ms: int


@dataclass
class _PromiseState:
    """State of a promise in the test harness."""

    name: str
    status: str = "pending"  # "pending", "resolved", "rejected"
    result: str = ""
    error: str = ""


# ---------------------------------------------------------------------------
# TestHarness — configurable HostCalls mock
# ---------------------------------------------------------------------------


class CleatTestHarness(HostCalls):
    """Mock HostCalls with configurable stubs for WASM-free testing.

    Records all durable calls, signals, child workflows, and promises.
    Use the ``stub_*`` methods to configure responses before invoking
    workflow code.

    Attributes
    ----------
    now_ms:
        Current simulated time in milliseconds since epoch.
    call_history:
        List of all durable calls made (for test assertions).
    """

    def __init__(self) -> None:
        super().__init__()
        # Simulated clock (starts at 2024-01-01T00:00:00Z)
        self._start_ms: int = 1704067200000
        self.now_ms: int = self._start_ms
        self._version_val: int = 1
        self._min_version_val: int = 1
        self._workflow_id: str = "test-workflow-id"
        self._run_id: str = "test-run-id"

        # Stubs
        self._call_stubs: list[_CallStub] = []
        self._child_stubs: dict[str, _ChildStub] = {}
        self._pending_signals: list[_SignalStub] = []

        # Call history for assertions
        self.call_history: list[CallRecord] = []

        # Promise state
        self._promises: dict[str, _PromiseState] = {}
        self._promise_counter: int = 0

        # Query state store
        self._query_state: dict[str, str] = {}

    # ------------------------------------------------------------------
    # Stub configuration
    # ------------------------------------------------------------------

    def stub_call(
        self,
        service: str,
        operation: str,
        response: str = "",
        error: Optional[str] = None,
    ) -> None:
        """Register a stub response for a ``durable_call``.

        Stubs are consumed in FIFO order. If multiple stubs are registered
        for the same (service, operation), they are used in sequence.

        Parameters
        ----------
        service:
            Service name to match (e.g. ``"payment"``).
        operation:
            Operation name to match (e.g. ``"charge"``).
        response:
            The response string to return (typically JSON).
        error:
            If set, raises a ``RuntimeError`` with this message instead
            of returning the response.
        """
        self._call_stubs.append(
            _CallStub(service=service, operation=operation, response=response, error=error)
        )

    def stub_signal(self, signal_name: str, payload: str) -> None:
        """Queue a pending signal for delivery.

        The signal is available for ``poll_signal`` immediately. For
        ``await_signals``, it is delivered when the simulated clock
        reaches or passes its delivery time.

        Parameters
        ----------
        signal_name:
            The signal name.
        payload:
            The signal payload string.
        """
        self._pending_signals.append(
            _SignalStub(name=signal_name, payload=payload, deliver_at_ms=self.now_ms)
        )

    def stub_promise(self, promise_id: str, value: str) -> None:
        """Resolve a promise with the given value.

        After calling this, ``await_promise`` will immediately return
        the resolved value for the given promise ID.

        Parameters
        ----------
        promise_id:
            The promise ID to resolve.
        value:
            The resolved value string.
        """
        if promise_id in self._promises:
            self._promises[promise_id].status = "resolved"
            self._promises[promise_id].result = value
        else:
            self._promises[promise_id] = _PromiseState(
                name="stub", status="resolved", result=value
            )

    def stub_reject_promise(self, promise_id: str, error: str) -> None:
        """Reject a promise with the given error message.

        Parameters
        ----------
        promise_id:
            The promise ID to reject.
        error:
            The error message.
        """
        if promise_id in self._promises:
            self._promises[promise_id].status = "rejected"
            self._promises[promise_id].error = error
        else:
            self._promises[promise_id] = _PromiseState(
                name="stub", status="rejected", error=error
            )

    def stub_child_workflow(self, name: str, result: str, error: Optional[str] = None) -> None:
        """Register a stub result for a child workflow.

        Parameters
        ----------
        name:
            Child workflow definition name.
        result:
            The result JSON to return.
        error:
            If set, ``await_child`` will raise a ``RuntimeError``.
        """
        self._child_stubs[name] = _ChildStub(name=name, result=result, error=error)

    def register_child_stub(self, name: str, response: str) -> None:
        """Register a stub for a child workflow with the given name.

        When a child workflow with this name is started, ``child_workflow``
        returns a run ID and the pre-configured response is returned by
        ``await_child``. Supports multiple different child workflow names.

        This is a simpler alternative to ``stub_child_workflow`` (no error
        parameter).

        Parameters
        ----------
        name:
            Child workflow definition name.
        response:
            The response string to return when awaiting the child.
        """
        self._child_stubs[name] = _ChildStub(name=name, result=response, error=None)

    # ------------------------------------------------------------------
    # Clock control
    # ------------------------------------------------------------------

    def advance_time(self, ms: int) -> None:
        """Advance the simulated clock by the given milliseconds.

        Parameters
        ----------
        ms:
            Number of milliseconds to advance.
        """
        self.now_ms += ms

    def set_time(self, epoch_ms: int) -> None:
        """Set the simulated clock to an absolute time.

        Parameters
        ----------
        epoch_ms:
            Milliseconds since Unix epoch.
        """
        self.now_ms = epoch_ms

    def reset(self) -> None:
        """Reset all harness state: stubs, history, clock, promises."""
        self.now_ms = self._start_ms
        self._call_stubs.clear()
        self._child_stubs.clear()
        self._pending_signals.clear()
        self.call_history.clear()
        self._promises.clear()
        self._query_state.clear()
        self._version_val = 1
        self._min_version_val = 1

    # ------------------------------------------------------------------
    # Test assertions
    # ------------------------------------------------------------------

    def call_count(self, service: str, operation: str) -> int:
        """Return how many calls were made to the given service+operation.

        Parameters
        ----------
        service:
            Service name.
        operation:
            Operation name.

        Returns
        -------
        int
            Number of matching calls in the call history.
        """
        return sum(
            1 for r in self.call_history if r.service == service and r.operation == operation
        )

    def assert_called(self, service: str, operation: str) -> bool:
        """Return True if a call to the given service+operation was made.

        Parameters
        ----------
        service:
            Service name.
        operation:
            Operation name.

        Returns
        -------
        bool
            True if the call appears in the history.
        """
        return self.call_count(service, operation) > 0

    def assert_not_called(self, service: str, operation: str) -> bool:
        """Return True if NO call to the given service+operation was made.

        Parameters
        ----------
        service:
            Service name.
        operation:
            Operation name.

        Returns
        -------
        bool
            True if the call does NOT appear in the history.
        """
        return self.call_count(service, operation) == 0

    def last_call(self, service: str, operation: str) -> Optional[CallRecord]:
        """Return the last call record for the given service+operation.

        Parameters
        ----------
        service:
            Service name.
        operation:
            Operation name.

        Returns
        -------
        CallRecord or None
            The last matching call record, or None if no match.
        """
        for rec in reversed(self.call_history):
            if rec.service == service and rec.operation == operation:
                return rec
        return None

    # ------------------------------------------------------------------
    # HostCalls method overrides
    # ------------------------------------------------------------------

    def now(self) -> int:
        return self.now_ms

    def random(self) -> int:
        # Deterministic: always return 42 for test predictability
        return 42

    def version(self) -> int:
        return self._version_val

    def min_version(self) -> int:
        return self._min_version_val

    def current_workflow_id(self) -> str:
        return self._workflow_id

    def current_run_id(self) -> str:
        return self._run_id

    def durable_log(self, message: str) -> None:
        # Best-effort: no-op in test harness
        pass

    def set_query_state(self, key: str, value: str) -> None:
        self._query_state[key] = value

    def get_query_state(self, key: str, result_type: type = str) -> Any:
        val = self._query_state.get(key, "")
        if result_type is str:
            return val
        data = json.loads(val)
        if isinstance(data, dict):
            return result_type(**data)
        return result_type(data)

    # ------------------------------------------------------------------
    # durable_call
    # ------------------------------------------------------------------

    def durable_call(self, service: str, operation: str, request: Any) -> str:
        req_str = self._marshal(request)

        # Find first matching stub
        for i, stub in enumerate(self._call_stubs):
            if stub.service == service and stub.operation == operation:
                # Consume the stub
                self._call_stubs.pop(i)
                rec = CallRecord(
                    service=service,
                    operation=operation,
                    request=req_str,
                    response=stub.response,
                    error=stub.error,
                )
                self.call_history.append(rec)
                if stub.error:
                    raise RuntimeError(
                        f"durable_call failed: {stub.error}"
                    )
                return stub.response

        # No stub found
        rec = CallRecord(
            service=service,
            operation=operation,
            request=req_str,
            response="",
            error="no stub registered",
        )
        self.call_history.append(rec)
        raise RuntimeError(
            f"CleatTestHarness: no stub registered for {service}.{operation} "
            f"(request: {req_str})"
        )

    def durable_call_with_retry(
        self, service: str, operation: str, request: Any, retry: RetryPolicy
    ) -> str:
        # Delegate to durable_call (simplified; no actual retry in harness)
        return self.durable_call(service, operation, request)

    def durable_call_with_heartbeat(
        self,
        service: str,
        operation: str,
        request: Any,
        heartbeat_interval_ms: int,
        progress: Callable[[str], None],
    ) -> str:
        # Delegate to durable_call (no heartbeat simulation)
        return self.durable_call(service, operation, request)

    def durable_fetch(
        self,
        url: str,
        method: str = "GET",
        headers: Optional[dict] = None,
        body: str = "",
    ) -> tuple:
        # Delegate to durable_call("http", "fetch", ...)
        request = {"url": url, "method": method, "headers": headers or {}, "body": body}
        result = self.durable_call("http", "fetch", request)
        data = json.loads(result)
        return data.get("body", ""), data.get("status", 200)

    # ------------------------------------------------------------------
    # durable_sleep
    # ------------------------------------------------------------------

    def durable_sleep(self, duration_ms: int) -> bool:
        # In the test harness, sleep advances the clock immediately
        self.now_ms += duration_ms
        return False

    # ------------------------------------------------------------------
    # Signals
    # ------------------------------------------------------------------

    def poll_signal(self, name: str) -> tuple:
        for i, sig in enumerate(self._pending_signals):
            if sig.name == name and sig.deliver_at_ms <= self.now_ms:
                self._pending_signals.pop(i)
                return (sig.payload, True)
        return ("", False)

    def await_signals(self, signal_names: list[str], timeout_ms: int) -> SignalResult:
        # Check for any immediately available signal
        for i, sig in enumerate(self._pending_signals):
            if sig.name in signal_names and sig.deliver_at_ms <= self.now_ms:
                self._pending_signals.pop(i)
                return SignalResult(name=sig.name, payload=sig.payload, timed_out=False)

        # Indefinite wait: timeout_ms <= 0 means "wait forever".
        # Simulate by raising SuspendSentinel (matching real host behavior).
        if timeout_ms <= 0:
            raise SuspendSentinel()

        # Advance time to simulate blocking
        self.now_ms += timeout_ms
        # Check again after time advance
        for i, sig in enumerate(self._pending_signals):
            if sig.name in signal_names and sig.deliver_at_ms <= self.now_ms:
                self._pending_signals.pop(i)
                return SignalResult(name=sig.name, payload=sig.payload, timed_out=False)

        return SignalResult(name="", payload="", timed_out=True)

    def poll_cancellation(self) -> tuple:
        return (False, "")

    # ------------------------------------------------------------------
    # Child workflows
    # ------------------------------------------------------------------

    def child_workflow(self, name: str, input: Any) -> str:
        input_str = self._marshal(input)
        stub = self._child_stubs.get(name)
        run_id = f"test-child-{name}-{len(self.call_history)}"

        # Record the child workflow invocation.
        self.call_history.append(
            CallRecord(
                service="workflow",
                operation=name,
                request=input_str,
                response=run_id,
            )
        )

        if stub:
            return run_id

        return run_id

    def await_child(self, run_id: str) -> str:
        # Resolve by finding the matching stub
        for child_name, stub in self._child_stubs.items():
            if run_id.startswith(f"test-child-{child_name}"):
                if stub.error:
                    raise RuntimeError(stub.error)
                return stub.result
        return '{"status": "completed"}'

    def await_all_children(self, run_ids: list[str]) -> list[ChildResult]:
        results = []
        for run_id in run_ids:
            try:
                result = self.await_child(run_id)
                results.append(ChildResult(run_id=run_id, result=result, error=None))
            except RuntimeError as e:
                results.append(ChildResult(run_id=run_id, result="", error=str(e)))
        return results

    # ------------------------------------------------------------------
    # State
    # ------------------------------------------------------------------

    def set_state(self, key: str, value: Any) -> None:
        self.durable_call("state", "set", {"key": key, "value": value})

    def get_state(self, key: str, result_type: type = str) -> Any:
        result = self.durable_call("state", "get", {"key": key})
        data = json.loads(result)
        if isinstance(data, dict):
            if result_type is str:
                return result
            return result_type(**data)
        if result_type is str:
            return str(data)
        return result_type(data)

    def delete_state(self, key: str) -> None:
        self.durable_call("state", "delete", {"key": key})

    def incr_state(self, key: str, delta: int = 1) -> int:
        result = self.durable_call("state", "incr", {"key": key, "delta": delta})
        return int(json.loads(result))

    # ------------------------------------------------------------------
    # Promises
    # ------------------------------------------------------------------

    def create_promise(self, name: str, ttl_ms: Optional[int] = None) -> str:
        self._promise_counter += 1
        promise_id = f"test-prom-{name}-{self._promise_counter}"
        self._promises[promise_id] = _PromiseState(name=name)
        return promise_id

    def await_promise(self, promise_id: str, timeout_ms: int) -> PromiseResult:
        ps = self._promises.get(promise_id)
        if ps is None:
            raise RuntimeError(f"promise {promise_id} not found")

        if ps.status == "resolved":
            return PromiseResult(result=ps.result, timed_out=False, rejected=False)
        if ps.status == "rejected":
            return PromiseResult(result=ps.error, timed_out=False, rejected=True)

        # Pending — simulate timeout
        self.now_ms += timeout_ms
        return PromiseResult(result="", timed_out=True, rejected=False)

    def resolve_promise(self, promise_id: str, value: str) -> None:
        if promise_id in self._promises:
            self._promises[promise_id].status = "resolved"
            self._promises[promise_id].result = value

    def reject_promise(self, promise_id: str, error: str) -> None:
        if promise_id in self._promises:
            self._promises[promise_id].status = "rejected"
            self._promises[promise_id].error = error

    # ------------------------------------------------------------------
    # Defer, continue_as_new, send, schedule, plugin
    # ------------------------------------------------------------------

    def durable_defer(self, description: str) -> str:
        return f"test-defer-{len(self.call_history)}"

    def continue_as_new(self, input: Any) -> None:
        pass

    def extend_timeout(self, additional_ms: int) -> None:
        """Extend the workflow's execution timeout (no-op in test harness)."""
        pass

    def durable_send(self, service: str, operation: str, request: Any) -> None:
        req_str = self._marshal(request)
        self.call_history.append(
            CallRecord(service=service, operation=operation, request=req_str)
        )

    def schedule_invoke(
        self, service: str, operation: str, request: Any, delay_ms: int
    ) -> None:
        req_str = self._marshal(request)
        self.call_history.append(
            CallRecord(
                service=service,
                operation=operation,
                request=json.dumps({"body": req_str, "delay_ms": delay_ms}),
            )
        )

    def plugin_call(self, plugin_name: str, function_name: str, input: Any) -> str:
        input_str = self._marshal(input)
        # Check for stubs — try matching registered stubs
        for i, stub in enumerate(self._call_stubs):
            if stub.service == f"plugin:{plugin_name}" and stub.operation == function_name:
                self._call_stubs.pop(i)
                if stub.error:
                    raise RuntimeError(stub.error)
                return stub.response
        raise RuntimeError(
            f"CleatTestHarness: no stub registered for plugin {plugin_name}.{function_name}"
        )
