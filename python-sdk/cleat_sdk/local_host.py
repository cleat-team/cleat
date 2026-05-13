"""In-process host calls for testing Python workflows without WASM.

Provides the :class:`LocalHostCalls` class, a standalone implementation of
all :class:`HostCalls` methods that runs locally without a WASM runtime.
Supports two modes:

* ``"record"`` (default) — executes calls in-process and records every
  interaction to an event log for later replay.
* ``"replay"`` — replays from a pre-recorded event log, returning
  identical results without executing any real logic.

Usage::

    from cleat_sdk.local_host import LocalHostCalls

    # Record mode: make real calls in-process
    h = LocalHostCalls(mode="record")
    h.set_state("counter", 0)
    result = h.call("greeter", "Greet", {"name": "World"})
    log = h.get_event_log()

    # Replay mode: replay the recorded log
    h2 = LocalHostCalls(mode="replay")
    h2.load_event_log(log)
    result2 = h2.call("greeter", "Greet", {"name": "World"})
"""

from __future__ import annotations

import hashlib
import json
import time
from dataclasses import dataclass, field
from typing import Any, Callable, Iterator, Optional, TypeVar

from .host_calls import (
    ChildResult,
    ChildWorkflowOptions,
    PromiseResult,
    RetryPolicy,
    SignalResult,
    SuspendSentinel,
)
from .host_calls import INFINITE_TIMEOUT_MS

T = TypeVar("T")

# Sentinel value used internally to distinguish "no result provided" from None.
_UNSET = object()

# ---------------------------------------------------------------------------
# Event log entry
# ---------------------------------------------------------------------------


@dataclass
class _EventLogEntry:
    """A single recorded event for record/replay."""

    method: str
    args: tuple = ()
    kwargs: dict = field(default_factory=dict)
    result: Any = None
    exception: Optional[str] = None


# ---------------------------------------------------------------------------
# Internal state helpers
# ---------------------------------------------------------------------------


@dataclass
class _ChildState:
    """State of a child workflow in the local host."""

    name: str
    run_id: str
    result: str
    error: Optional[str] = None


@dataclass
class _PromiseState:
    """State of a promise in the local host."""

    name: str
    promise_id: str
    status: str = "pending"  # "pending", "resolved", "rejected"
    result: str = ""
    error: str = ""


# ---------------------------------------------------------------------------
# LocalHostCalls
# ---------------------------------------------------------------------------


class LocalHostCalls:
    """In-process host calls for testing Python workflows without WASM.

    Parameters
    ----------
    mode : str
        ``"record"`` — execute calls and record to event log.
        ``"replay"`` — replay from a pre-recorded event log.
    """

    def __init__(self, mode: str = "record") -> None:
        if mode not in ("record", "replay"):
            raise ValueError(f"mode must be 'record' or 'replay', got {mode!r}")
        self._mode = mode
        self._event_log: list[_EventLogEntry] = []
        self._replay_cursor: int = 0
        self._state: dict[str, Any] = {}
        self._signals: list[dict] = []
        self._promises: dict[str, _PromiseState] = {}
        self._promise_counter: int = 0
        self._locks: set[str] = set()
        self._query_state: dict[str, str] = {}
        self._children: dict[str, _ChildState] = {}
        self._children_by_name: dict[str, _ChildState] = {}
        self._child_counter: int = 0
        self._deferrals: dict[str, str] = {}
        self._deferral_counter: int = 0
        self._cron_schedules: dict[str, dict] = {}
        self._cron_counter: int = 0
        self._update_handlers: dict[
            str, tuple[Callable[[str], str], Optional[Callable[[str], bool]]]
        ] = {}
        self._query_handlers: dict[str, Callable[[str], str]] = {}
        self._scope_prefix: str = ""
        self._workflow_id: str = "local-wf-id"
        self._run_id: str = "local-run-id"
        self._workflow_version: int = 1
        self._workflow_min_version: int = 1
        self._now_ms: int = int(time.time() * 1000)
        self._detached_context: bool = False

    # ------------------------------------------------------------------
    # Record / Replay internals
    # ------------------------------------------------------------------

    def _record(self, _method_name: str, *args: Any, result: Any = _UNSET, **kwargs: Any) -> None:
        """Append a method call to the event log.

        Parameters
        ----------
        _method_name : str
            The method name being recorded.
        args : Any
            Positional arguments.
        result : Any
            The return value of the method call.  Stored for replay.
        kwargs : Any
            Keyword arguments.
        """
        if self._mode == "record":
            self._event_log.append(
                _EventLogEntry(
                    method=_method_name,
                    args=args,
                    kwargs=kwargs,
                    result=None if result is _UNSET else result,
                )
            )

    def _replay_next(self, expected_method: str) -> Any:
        """Get the next recorded result from the event log.

        Parameters
        ----------
        expected_method : str
            The method name expected at this cursor position.

        Returns
        -------
        Any
            The recorded result (the value that ``result`` kwarg held when
            :meth:`_record` was called).

        Raises
        ------
        RuntimeError
            If the next event does not match *expected_method* or the log
            has been exhausted.
        """
        if self._replay_cursor >= len(self._event_log):
            raise RuntimeError(
                f"Replay exhausted: expected {expected_method!r} "
                f"but no more events in log (cursor={self._replay_cursor})"
            )
        entry = self._event_log[self._replay_cursor]
        self._replay_cursor += 1
        if entry.method != expected_method:
            raise RuntimeError(
                f"Replay mismatch at cursor {self._replay_cursor - 1}: "
                f"expected {expected_method!r}, got {entry.method!r}"
            )
        if entry.exception is not None:
            raise RuntimeError(entry.exception)
        return entry.result

    def get_event_log(self) -> list[dict]:
        """Return the recorded event log as a list of serialisable dicts.

        Returns
        -------
        list[dict]
            Each entry has ``method``, ``args``, ``kwargs``, ``result``,
            and optionally ``exception`` keys.
        """
        return [
            {
                "method": e.method,
                "args": e.args,
                "kwargs": e.kwargs,
                "result": e.result,
                "exception": e.exception,
            }
            for e in self._event_log
        ]

    def load_event_log(self, log: list[dict]) -> None:
        """Load a previously recorded event log for replay.

        Parameters
        ----------
        log : list[dict]
            Event log entries as returned by :meth:`get_event_log`.
        """
        self._event_log = [
            _EventLogEntry(
                method=e["method"],
                args=tuple(e.get("args", [])),
                kwargs=e.get("kwargs", {}),
                result=e.get("result"),
                exception=e.get("exception"),
            )
            for e in log
        ]
        self._replay_cursor = 0

    def reset(self) -> None:
        """Reset all local state: state store, signals, promises, etc."""
        self._state.clear()
        self._signals.clear()
        self._promises.clear()
        self._promise_counter = 0
        self._locks.clear()
        self._query_state.clear()
        self._children.clear()
        self._children_by_name.clear()
        self._child_counter = 0
        self._deferrals.clear()
        self._deferral_counter = 0
        self._cron_schedules.clear()
        self._cron_counter = 0
        self._update_handlers.clear()
        self._query_handlers.clear()
        self._scope_prefix = ""

    # ------------------------------------------------------------------
    # Internal helpers
    # ------------------------------------------------------------------

    @staticmethod
    def _marshal(value: Any) -> str:
        """Convert *value* to a JSON string.

        Strings are passed through unchanged; all other types are
        ``json.dumps``'d.
        """
        if isinstance(value, str):
            return value
        return json.dumps(value)

    def _scoped_key(self, key: str) -> str:
        """Apply the current scope prefix to *key*, if a scope is active."""
        if self._scope_prefix:
            return self._scope_prefix + key
        return key

    # ------------------------------------------------------------------
    # Scope management for virtual object instances
    # ------------------------------------------------------------------

    def set_scope(self, object_type: str, instance_key: str) -> str:
        """Set the state key prefix for virtual object instances.

        Parameters
        ----------
        object_type : str
            The virtual object type name.
        instance_key : str
            The instance key for this specific object.

        Returns
        -------
        str
            The previous scope prefix (empty string if none was set).
        """
        prev = self._scope_prefix
        self._scope_prefix = (
            f"vo:{object_type}:{instance_key}:" if object_type and instance_key else ""
        )
        return prev

    def get_scope(self) -> tuple[str, str]:
        """Get the current virtual object scope.

        Returns
        -------
        tuple[str, str]
            ``(object_type, instance_key)`` or ``("", "")`` if no scope is
            active.
        """
        if not self._scope_prefix:
            return "", ""
        prefix = self._scope_prefix.rstrip(":")
        parts = prefix.split(":", 2)
        if len(parts) == 3 and parts[0] == "vo":
            return parts[1], parts[2]
        return "", ""

    def clear_scope(self) -> str:
        """Remove the current scope and return the previous scope prefix.

        Returns
        -------
        str
            The scope prefix that was active before clearing (empty string
            if none was set).
        """
        prev = self._scope_prefix
        self._scope_prefix = ""
        return prev

    # ------------------------------------------------------------------
    # UUID — deterministic ID generation
    # ------------------------------------------------------------------

    def uuid(self, seed: str) -> str:
        """Return a deterministic UUID scoped to the current workflow and seed.

        Parameters
        ----------
        seed : str
            A seed string that determines the UUID within this workflow.

        Returns
        -------
        str
            A UUID-formatted string.
        """
        wf_id = self.current_workflow_id()
        data = (wf_id + ":" + seed).encode("utf-8")
        h = hashlib.sha256(data).digest()[:16]
        h_bytes = bytearray(h)
        h_bytes[6] = (h_bytes[6] & 0x0F) | 0x50
        h_bytes[8] = (h_bytes[8] & 0x3F) | 0x80
        return (
            f"{h_bytes[0:4].hex()}-"
            f"{h_bytes[4:6].hex()}-"
            f"{h_bytes[6:8].hex()}-"
            f"{h_bytes[8:10].hex()}-"
            f"{h_bytes[10:16].hex()}"
        )

    # ------------------------------------------------------------------
    # 1. now — wall-clock time
    # ------------------------------------------------------------------

    def now(self) -> int:
        """Get the current wall-clock time in milliseconds since epoch."""
        result = self._now_ms
        self._record("now", result=result)
        return result

    # ------------------------------------------------------------------
    # 2. random — deterministic random value
    # ------------------------------------------------------------------

    def random(self) -> int:
        """Get a deterministic random value."""
        result = 42
        self._record("random", result=result)
        return result

    # ------------------------------------------------------------------
    # 3. current_workflow_id
    # ------------------------------------------------------------------

    def current_workflow_id(self) -> str:
        """Get the current workflow's unique identifier."""
        result = self._workflow_id
        self._record("current_workflow_id", result=result)
        return result

    # ------------------------------------------------------------------
    # 4. current_run_id
    # ------------------------------------------------------------------

    def current_run_id(self) -> str:
        """Get the current workflow run's unique identifier."""
        result = self._run_id
        self._record("current_run_id", result=result)
        return result

    # ------------------------------------------------------------------
    # 5. version
    # ------------------------------------------------------------------

    def version(self) -> int:
        """Get the workflow definition version."""
        result = self._workflow_version
        self._record("version", result=result)
        return result

    # ------------------------------------------------------------------
    # 6. min_version
    # ------------------------------------------------------------------

    def min_version(self) -> int:
        """Get the minimum supported version for this workflow definition."""
        result = self._workflow_min_version
        self._record("min_version", result=result)
        return result

    # ------------------------------------------------------------------
    # 7. log
    # ------------------------------------------------------------------

    def log(self, message: str) -> None:
        """Log a message to the workflow event history.

        In local mode, the message is printed to stdout for visibility.
        """
        if self._mode == "replay":
            self._replay_next("log")
            return
        self._record("log", message=message)

    # ------------------------------------------------------------------
    # 8. log_kv
    # ------------------------------------------------------------------

    def log_kv(self, message: str, *kvs: Any) -> None:
        """Log a structured key-value message to the event history.

        Parameters
        ----------
        message : str
            The main log message.
        *kvs : Any
            Alternating key-value pairs for structured logging.
        """
        if kvs:
            parts = [message]
            for i in range(0, len(kvs), 2):
                key = kvs[i]
                val = kvs[i + 1] if i + 1 < len(kvs) else ""
                parts.append(f"  {key}={val}")
            formatted = "\n".join(parts)
        else:
            formatted = message
        self.log(formatted)

    # ------------------------------------------------------------------
    # 9. call — recorded API call
    # ------------------------------------------------------------------

    def call(
        self,
        service: str,
        operation: str,
        request: Any,
        timeout_ms: Optional[int] = None,
    ) -> str:
        """Make a durable (deterministically replayed) call to an external service.

        In record mode, returns a mock response with the service and
        operation reflected back.  In replay mode, returns the recorded
        result.
        """
        if self._mode == "replay":
            return self._replay_next("call")
        result = json.dumps(
            {
                "status": "ok",
                "service": service,
                "operation": operation,
                "echo": self._marshal(request),
            }
        )
        self._record("call", result=result, service=service, operation=operation, request=request)
        return result

    # ------------------------------------------------------------------
    # 10. call_typed
    # ------------------------------------------------------------------

    def call_typed(
        self, service: str, operation: str, request: Any, result_type: type[T]
    ) -> T:
        """Make a cleat call and deserialise the JSON response."""
        response = self.call(service, operation, request)
        data = json.loads(response)
        if isinstance(data, dict):
            return result_type(**data)
        return result_type(data)

    # ------------------------------------------------------------------
    # 11. call_with_retry
    # ------------------------------------------------------------------

    def call_with_retry(
        self, service: str, operation: str, request: Any, retry: RetryPolicy
    ) -> str:
        """Make a cleat call with server-side retry (single attempt local)."""
        return self.call(service, operation, request)

    # ------------------------------------------------------------------
    # 12. call_with_heartbeat
    # ------------------------------------------------------------------

    def call_with_heartbeat(
        self,
        service: str,
        operation: str,
        request: Any,
        heartbeat_interval_ms: int,
        progress: Callable[[str], None],
    ) -> str:
        """Make a cleat call with heartbeat / progress updates."""
        return self.call(service, operation, request)

    # ------------------------------------------------------------------
    # 13. sleep
    # ------------------------------------------------------------------

    def sleep(self, timeout_seconds: float) -> bool:
        """Suspend workflow execution for a duration in seconds."""
        return self.sleep_ms(int(timeout_seconds * 1000))

    def sleep_ms(self, timeout_ms: int) -> bool:
        """Suspend workflow execution for a duration in milliseconds."""
        if self._mode == "replay":
            return self._replay_next("sleep_ms")
        if timeout_ms > 0:
            time.sleep(timeout_ms / 1000.0)
            self._now_ms += timeout_ms
        result = False
        self._record("sleep_ms", result=result, timeout_ms=timeout_ms)
        return result

    # ------------------------------------------------------------------
    # 14. fetch
    # ------------------------------------------------------------------

    def fetch(
        self,
        url: str,
        method: str = "GET",
        headers: Optional[dict] = None,
        body: str = "",
    ) -> tuple:
        """Perform an HTTP fetch via the ``"http"`` service.

        Returns a mock response tuple (body, status_code) without making
        an actual HTTP request.
        """
        if self._mode == "replay":
            return self._replay_next("fetch")
        body_text = json.dumps(
            {"url": url, "method": method, "headers": headers or {}, "body": body}
        )
        status_code = 200
        result = (body_text, status_code)
        self._record("fetch", result=result, url=url, method=method, headers=headers, body=body)
        return result

    # ------------------------------------------------------------------
    # 15. fetch_json
    # ------------------------------------------------------------------

    def fetch_json(
        self,
        url: str,
        method: str = "GET",
        headers: Optional[dict] = None,
        body: str = "",
        result_type: type[T] = dict,
    ) -> T:
        """Perform a cleat HTTP fetch and deserialize the JSON response."""
        resp_body, status = self.fetch(url, method, headers, body)
        data = json.loads(resp_body)
        if isinstance(data, dict) and result_type is not dict:
            return result_type(**data)
        return data  # type: ignore[return-value]

    # ------------------------------------------------------------------
    # 16. fetch_get
    # ------------------------------------------------------------------

    def fetch_get(self, url: str) -> tuple:
        """Shorthand for a GET request via :meth:`fetch`."""
        return self.fetch(url, "GET")

    # ------------------------------------------------------------------
    # 17. fetch_get_json
    # ------------------------------------------------------------------

    def fetch_get_json(
        self,
        url: str,
        result_type: type[T] = dict,
    ) -> T:
        """Shorthand for a cleat GET request with JSON deserialization."""
        return self.fetch_json(url, "GET", result_type=result_type)

    # ------------------------------------------------------------------
    # 18. await_signals
    # ------------------------------------------------------------------

    def await_signals(self, signal_names: list[str], timeout_seconds: float) -> SignalResult:
        """Wait for one or more external signals, with a timeout in seconds."""
        return self.await_signals_ms(signal_names, int(timeout_seconds * 1000))

    def await_signals_ms(self, signal_names: list[str], timeout_ms: int) -> SignalResult:
        """Wait for one or more external signals, with a timeout in milliseconds."""
        if self._mode == "replay":
            return self._replay_next("await_signals_ms")

        # Check for any immediately available signal
        for i, sig in enumerate(self._signals):
            if sig["name"] in signal_names:
                self._signals.pop(i)
                result = SignalResult(
                    name=sig["name"], payload=sig.get("payload", ""), timed_out=False
                )
                self._record("await_signals_ms", result=result, signal_names=signal_names, timeout_ms=timeout_ms)
                return result

        # No signal available — simulate timeout if finite
        if timeout_ms <= 0 or timeout_ms >= INFINITE_TIMEOUT_MS:
            result = SignalResult(name="", payload="", timed_out=True)
            self._record("await_signals_ms", result=result, signal_names=signal_names, timeout_ms=timeout_ms)
            return result

        # Simulate waiting by advancing time
        if timeout_ms > 0:
            self._now_ms += timeout_ms

        # Check again after simulated wait
        for i, sig in enumerate(self._signals):
            if sig["name"] in signal_names:
                self._signals.pop(i)
                result = SignalResult(
                    name=sig["name"], payload=sig.get("payload", ""), timed_out=False
                )
                self._record("await_signals_ms", result=result, signal_names=signal_names, timeout_ms=timeout_ms)
                return result

        result = SignalResult(name="", payload="", timed_out=True)
        self._record("await_signals_ms", result=result, signal_names=signal_names, timeout_ms=timeout_ms)
        return result

    # ------------------------------------------------------------------
    # 19. poll_signal
    # ------------------------------------------------------------------

    def poll_signal(self, name: str) -> tuple:
        """Poll for a specific pending signal (non-blocking).

        Returns
        -------
        tuple[str, bool]
            ``(payload, found)`` where *found* is ``True`` if a signal was
            pending.
        """
        if self._mode == "replay":
            return self._replay_next("poll_signal")
        for i, sig in enumerate(self._signals):
            if sig["name"] == name:
                self._signals.pop(i)
                result = (sig.get("payload", ""), True)
                self._record("poll_signal", result=result, name=name)
                return result
        result = ("", False)
        self._record("poll_signal", result=result, name=name)
        return result

    # ------------------------------------------------------------------
    # 20. poll_cancellation
    # ------------------------------------------------------------------

    def poll_cancellation(self) -> tuple:
        """Check if workflow cancellation has been requested.

        Returns
        -------
        tuple[bool, str]
            ``(cancelled, reason)``.
        """
        if self._mode == "replay":
            return self._replay_next("poll_cancellation")
        result = (False, "")
        self._record("poll_cancellation", result=result)
        return result

    # ------------------------------------------------------------------
    # 21. send_signal_and_wait
    # ------------------------------------------------------------------

    def send_signal_and_wait(
        self,
        target_run_id: str,
        signal_name: str,
        payload: str,
        timeout_seconds: float,
    ) -> str:
        """Send a signal to a target workflow and wait for a response."""
        return self.send_signal_and_wait_ms(
            target_run_id, signal_name, payload, int(timeout_seconds * 1000)
        )

    def send_signal_and_wait_ms(
        self,
        target_run_id: str,
        signal_name: str,
        payload: str,
        timeout_ms: int,
    ) -> str:
        """Send a signal to a target workflow and wait for a response, ms."""
        if self._mode == "replay":
            return self._replay_next("send_signal_and_wait_ms")
        result = json.dumps(
            {
                "status": "signal_sent",
                "target": target_run_id,
                "signal": signal_name,
                "echo": self._marshal(payload),
            }
        )
        self._record(
            "send_signal_and_wait_ms", result=result,
            target_run_id=target_run_id, signal_name=signal_name,
            payload=payload, timeout_ms=timeout_ms,
        )
        return result

    # ------------------------------------------------------------------
    # 22. reply_to_signal
    # ------------------------------------------------------------------

    def reply_to_signal(self, correlation_id: str, response: str) -> None:
        """Send a response back to the sender of a signal."""
        if self._mode == "replay":
            self._replay_next("reply_to_signal")
            return
        self._record("reply_to_signal", correlation_id=correlation_id, response=response)

    # ------------------------------------------------------------------
    # 23. signal_workflow
    # ------------------------------------------------------------------

    def signal_workflow(
        self,
        target_run_id: str,
        signal_name: str,
        payload: Any,
    ) -> None:
        """Send a signal to a target workflow (fire-and-forget)."""
        if self._mode == "replay":
            self._replay_next("signal_workflow")
            return
        self._signals.append(
            {"name": signal_name, "payload": self._marshal(payload), "target": target_run_id}
        )
        self._record(
            "signal_workflow",
            target_run_id=target_run_id, signal_name=signal_name, payload=payload,
        )

    # ------------------------------------------------------------------
    # 24. await_signals_with_quorum
    # ------------------------------------------------------------------

    def await_signals_with_quorum(
        self,
        signal_names: list[str],
        min_count: int,
        max_rejections: int,
        timeout_ms: int,
    ) -> list[SignalResult]:
        """Wait for at least ``min_count`` signals from the named set."""
        if self._mode == "replay":
            return self._replay_next("await_signals_with_quorum")

        deadline_ns = time.monotonic_ns() + timeout_ms * 1_000_000
        results: list[SignalResult] = []
        rejection_count = 0

        while len(results) < min_count:
            remaining_ms = max(0, (deadline_ns - time.monotonic_ns()) // 1_000_000)
            if remaining_ms <= 0:
                raise RuntimeError(f"quorum timeout: got {len(results)}/{min_count} signals")

            # Scan signals for a match
            found = False
            for i, sig in enumerate(self._signals):
                if sig["name"] in signal_names:
                    self._signals.pop(i)
                    sr = SignalResult(
                        name=sig["name"], payload=sig.get("payload", ""), timed_out=False
                    )
                    results.append(sr)
                    found = True

                    # Check rejection
                    if max_rejections >= 0 and sr.payload:
                        try:
                            payload_data = json.loads(sr.payload)
                            if isinstance(payload_data, dict) and payload_data.get("rejected"):
                                rejection_count += 1
                                if rejection_count > max_rejections:
                                    raise RuntimeError(
                                        f"quorum exceeded max rejections ({max_rejections})"
                                    )
                        except (json.JSONDecodeError, TypeError):
                            pass
                    break

            if not found:
                # Advance time slightly and retry
                time.sleep(0.001)

        self._record(
            "await_signals_with_quorum", result=results,
            signal_names=signal_names, min_count=min_count,
            max_rejections=max_rejections, timeout_ms=timeout_ms,
        )
        return results

    # ------------------------------------------------------------------
    # 25. child_workflow
    # ------------------------------------------------------------------

    def child_workflow(self, name: str, input: Any) -> str:
        """Start a child workflow instance.

        Returns
        -------
        str
            The child workflow's run ID.
        """
        if self._mode == "replay":
            return self._replay_next("child_workflow")
        self._child_counter += 1
        run_id = f"local-child-{name}-{self._child_counter}"
        input_str = self._marshal(input)
        child = _ChildState(name=name, run_id=run_id, result=input_str)
        self._children[run_id] = child
        self._children_by_name[name] = child
        self._record("child_workflow", result=run_id, name=name, input=input)
        return run_id

    def child_workflow_with_options(
        self, name: str, input: Any, options: ChildWorkflowOptions = ChildWorkflowOptions()
    ) -> str:
        """Start a child workflow instance with version options."""
        if self._mode == "replay":
            return self._replay_next("child_workflow_with_options")
        run_id = self.child_workflow(name, input)
        self._record("child_workflow_with_options", result=run_id, name=name, input=input, options=options)
        return run_id

    # ------------------------------------------------------------------
    # 26. await_child
    # ------------------------------------------------------------------

    def await_child(self, run_id: str) -> str:
        """Wait for a child workflow to complete.

        Returns
        -------
        str
            The child's output JSON.
        """
        if self._mode == "replay":
            return self._replay_next("await_child")
        child = self._children.get(run_id)
        if child is None:
            raise RuntimeError(f"await_child(run_id='{run_id}'): child not found")
        if child.error:
            raise RuntimeError(child.error)
        result = child.result
        self._record("await_child", result=result, run_id=run_id)
        return result

    # ------------------------------------------------------------------
    # 27. await_all_children
    # ------------------------------------------------------------------

    def await_all_children(self, run_ids: list[str]) -> list[ChildResult]:
        """Wait for multiple child workflows to complete (batch).

        Returns
        -------
        list[ChildResult]
            Results for each child workflow, in the order of *run_ids*.
        """
        if self._mode == "replay":
            return self._replay_next("await_all_children")
        results = []
        for run_id in run_ids:
            child = self._children.get(run_id)
            if child is None:
                results.append(ChildResult(run_id=run_id, result="", error="child not found"))
            elif child.error:
                results.append(ChildResult(run_id=run_id, result="", error=child.error))
            else:
                results.append(ChildResult(run_id=run_id, result=child.result, error=None))
        self._record("await_all_children", result=results, run_ids=run_ids)
        return results

    # ------------------------------------------------------------------
    # 28. set_query_state
    # ------------------------------------------------------------------

    def set_query_state(self, key: str, value: str) -> None:
        """Set a key-value pair in the workflow's queryable state."""
        if self._mode == "replay":
            self._replay_next("set_query_state")
            return
        self._query_state[key] = value
        self._record("set_query_state", key=key, value=value)

    def get_query_state(self, key: str, result_type: type = str) -> Any:
        """Get a value from queryable state."""
        val = self._query_state.get(key, "")
        if result_type is str:
            return val
        data = json.loads(val)
        if isinstance(data, dict):
            return result_type(**data)
        return result_type(data)

    # ------------------------------------------------------------------
    # 29. set_state
    # ------------------------------------------------------------------

    def set_state(self, key: str, value: Any) -> None:
        """Set typed cleat state (marshals *value* to JSON)."""
        if self._mode == "replay":
            self._replay_next("set_state")
            return
        sk = self._scoped_key(key)
        self._state[sk] = value
        self._record("set_state", key=key, value=value)

    # ------------------------------------------------------------------
    # 30. get_state
    # ------------------------------------------------------------------

    def get_state(self, key: str, result_type: type[T] = str) -> T:
        """Get typed cleat state, deserialised into *result_type*."""
        if self._mode == "replay":
            return self._replay_next("get_state")
        sk = self._scoped_key(key)
        value = self._state.get(sk)
        if value is None:
            raise KeyError(f"state key {key!r} not found (scoped: {sk!r})")
        self._record("get_state", result=value, key=key, result_type=str)
        if result_type is str:
            return str(value)  # type: ignore[return-value]
        if isinstance(value, dict):
            return result_type(**value)
        return result_type(value)

    # ------------------------------------------------------------------
    # 31. delete_state
    # ------------------------------------------------------------------

    def delete_state(self, key: str) -> None:
        """Delete a cleat state key."""
        if self._mode == "replay":
            self._replay_next("delete_state")
            return
        sk = self._scoped_key(key)
        self._state.pop(sk, None)
        self._record("delete_state", key=key)

    # ------------------------------------------------------------------
    # 32. incr_state
    # ------------------------------------------------------------------

    def incr_state(self, key: str, delta: int = 1) -> int:
        """Atomically increment a numeric cleat state value.

        Returns
        -------
        int
            The new value after incrementing.
        """
        if self._mode == "replay":
            return self._replay_next("incr_state")
        sk = self._scoped_key(key)
        current = self._state.get(sk, 0)
        if not isinstance(current, (int, float)):
            current = 0
        new_val = int(current) + delta
        self._state[sk] = new_val
        self._record("incr_state", result=new_val, key=key, delta=delta)
        return new_val

    # ------------------------------------------------------------------
    # 33. has_state
    # ------------------------------------------------------------------

    def has_state(self, key: str) -> bool:
        """Check if a cleat state key exists."""
        if self._mode == "replay":
            return self._replay_next("has_state")
        sk = self._scoped_key(key)
        result = sk in self._state
        self._record("has_state", result=result, key=key)
        return result

    # ------------------------------------------------------------------
    # 34. list_state
    # ------------------------------------------------------------------

    def list_state(self, prefix: str = "") -> list[str]:
        """List all cleat state keys matching the given prefix."""
        if self._mode == "replay":
            return self._replay_next("list_state")
        if prefix:
            result = [k for k in self._state if k.startswith(prefix)]
        else:
            result = list(self._state.keys())
        self._record("list_state", result=result, prefix=prefix)
        return result

    # ------------------------------------------------------------------
    # 35. create_promise
    # ------------------------------------------------------------------

    def create_promise(self, name: str, ttl_ms: Optional[int] = None) -> str:
        """Create a cleat promise with the given name.

        Returns
        -------
        str
            The promise ID.
        """
        if self._mode == "replay":
            return self._replay_next("create_promise")
        self._promise_counter += 1
        promise_id = f"local-prom-{name}-{self._promise_counter}"
        self._promises[promise_id] = _PromiseState(
            name=name, promise_id=promise_id, status="pending"
        )
        self._record("create_promise", result=promise_id, name=name, ttl_ms=ttl_ms)
        return promise_id

    # ------------------------------------------------------------------
    # 36. await_promise
    # ------------------------------------------------------------------

    def await_promise(self, promise_id: str, timeout_seconds: float) -> PromiseResult:
        """Wait for a cleat promise to resolve, with a timeout in seconds."""
        return self.await_promise_ms(promise_id, int(timeout_seconds * 1000))

    def await_promise_ms(self, promise_id: str, timeout_ms: int) -> PromiseResult:
        """Wait for a cleat promise to resolve, with a timeout in milliseconds."""
        if self._mode == "replay":
            return self._replay_next("await_promise_ms")
        ps = self._promises.get(promise_id)
        if ps is None:
            raise RuntimeError(f"await_promise(promise_id='{promise_id}'): promise not found")
        if ps.status == "resolved":
            result = PromiseResult(result=ps.result, timed_out=False, rejected=False)
            self._record("await_promise_ms", result=result, promise_id=promise_id, timeout_ms=timeout_ms)
            return result
        if ps.status == "rejected":
            result = PromiseResult(result=ps.error, timed_out=False, rejected=True)
            self._record("await_promise_ms", result=result, promise_id=promise_id, timeout_ms=timeout_ms)
            return result
        # Pending — simulate timeout
        self._now_ms += timeout_ms
        result = PromiseResult(result="", timed_out=True, rejected=False)
        self._record("await_promise_ms", result=result, promise_id=promise_id, timeout_ms=timeout_ms)
        return result

    # ------------------------------------------------------------------
    # 37. resolve_promise
    # ------------------------------------------------------------------

    def resolve_promise(self, promise_id: str, value: str) -> None:
        """Resolve a cleat promise with a value."""
        if self._mode == "replay":
            self._replay_next("resolve_promise")
            return
        if promise_id in self._promises:
            self._promises[promise_id].status = "resolved"
            self._promises[promise_id].result = value
        self._record("resolve_promise", promise_id=promise_id, value=value)

    # ------------------------------------------------------------------
    # 38. reject_promise
    # ------------------------------------------------------------------

    def reject_promise(self, promise_id: str, error: str) -> None:
        """Reject a cleat promise with an error."""
        if self._mode == "replay":
            self._replay_next("reject_promise")
            return
        if promise_id in self._promises:
            self._promises[promise_id].status = "rejected"
            self._promises[promise_id].error = error
        self._record("reject_promise", promise_id=promise_id, error=error)

    # ------------------------------------------------------------------
    # 39. register_update_handler
    # ------------------------------------------------------------------

    def register_update_handler(
        self,
        name: str,
        handler: Callable[[str], str],
        validator: Optional[Callable[[str], bool]] = None,
    ) -> None:
        """Register a handler for update calls on this workflow."""
        if self._mode == "replay":
            self._replay_next("register_update_handler")
            return
        self._update_handlers[name] = (handler, validator)
        self._record("register_update_handler", name=name)

    def _handle_update(self, name: str, payload: str) -> str:
        """Internal: look up and invoke a registered update handler."""
        entry = self._update_handlers.get(name)
        if entry is None:
            raise RuntimeError(f"No update handler registered for '{name}'")
        handler, _ = entry
        return handler(payload)

    def _validate_update(self, name: str, payload: str) -> bool:
        """Internal: look up and invoke a registered update validator."""
        entry = self._update_handlers.get(name)
        if entry is None:
            return False
        _, validator = entry
        if validator is None:
            return True
        return validator(payload)

    # ------------------------------------------------------------------
    # 40. register_query_handler
    # ------------------------------------------------------------------

    def register_query_handler(
        self,
        name: str,
        handler: Callable[[str], str],
    ) -> None:
        """Register a read-only handler for query calls on this workflow."""
        if self._mode == "replay":
            self._replay_next("register_query_handler")
            return
        self._query_handlers[name] = handler
        self._record("register_query_handler", name=name)

    def _handle_query(self, name: str, payload: str) -> str:
        """Internal: look up and invoke a registered query handler."""
        handler = self._query_handlers.get(name)
        if handler is None:
            raise RuntimeError(f"No query handler registered for '{name}'")
        return handler(payload)

    # ------------------------------------------------------------------
    # 41. defer
    # ------------------------------------------------------------------

    def defer(self, description: str) -> str:
        """Register a deferred cleanup action to run on workflow exit.

        Returns
        -------
        str
            The defer ID.
        """
        if self._mode == "replay":
            return self._replay_next("defer")
        self._deferral_counter += 1
        defer_id = f"local-defer-{self._deferral_counter}"
        self._deferrals[defer_id] = description
        self._record("defer", result=defer_id, description=description)
        return defer_id

    # ------------------------------------------------------------------
    # 42. continue_as_new
    # ------------------------------------------------------------------

    def continue_as_new(self, input: Any) -> None:
        """Replace the workflow's input and restart execution from scratch."""
        if self._mode == "replay":
            self._replay_next("continue_as_new")
            return
        self._record("continue_as_new", input=input)

    def continue_as_new_versioned(self, new_input: Any, new_version: int) -> None:
        """Replace the workflow's input and version, then restart execution from scratch.

        Parameters
        ----------
        new_input : Any
            New input for the restarted workflow.
        new_version : int
            New workflow definition version for the restarted run.
        """
        if self._mode == "replay":
            self._replay_next("continue_as_new_versioned")
            return
        self._workflow_version = new_version
        self._record("continue_as_new_versioned", new_input=new_input, new_version=new_version)

    # ------------------------------------------------------------------
    # 42b. child_workflow_in_schema — start child with schema
    # ------------------------------------------------------------------

    def child_workflow_in_schema(
        self,
        target_schema: str,
        name: str,
        input_json: Any,
        version: Optional[int] = None,
        parent_close_policy: Optional[str] = None,
    ) -> str:
        """Start a child workflow in a schema. Delegates to child_workflow, ignoring schema.

        Parameters
        ----------
        target_schema : str
            Target schema (ignored in local mode; all workflows run in-process).
        name : str
            Child workflow definition name.
        input_json : Any
            Input for the child workflow.
        version : int, optional
            Workflow definition version.
        parent_close_policy : str, optional
            Policy for the child when the parent closes.

        Returns
        -------
        str
            The child workflow's run ID.
        """
        return self.child_workflow(name, input_json)

    # ------------------------------------------------------------------
    # 42c. side_effect — deterministic function execution
    # ------------------------------------------------------------------

    def side_effect(self, fn: Callable[[], str]) -> str:
        """Execute a function and record its result for deterministic replay.

        On first execution, *fn* is called and its result is recorded in the
        event log.  On replay, the cached result is returned without calling
        *fn* again.

        Parameters
        ----------
        fn : Callable[[], str]
            A zero-argument callable that returns a string result.

        Returns
        -------
        str
            The function result (from actual execution or replay cache).
        """
        if self._mode == "replay":
            return self._replay_next("side_effect")
        result = fn()
        result_str = self._marshal(result)
        self._record("side_effect", result=result_str)
        return result_str

    # ------------------------------------------------------------------
    # 43. extend_timeout
    # ------------------------------------------------------------------

    def extend_timeout(self, additional_ms: int) -> None:
        """Extend the workflow's execution timeout (no-op in local mode)."""
        if self._mode == "replay":
            self._replay_next("extend_timeout")
            return
        self._record("extend_timeout", additional_ms=additional_ms)

    # ------------------------------------------------------------------
    # 44. run_detached
    # ------------------------------------------------------------------

    def run_detached(self, fn: Callable[["LocalHostCalls"], Any]) -> None:
        """Execute a function that is detached from workflow cancellation."""
        saved = self._detached_context
        self._detached_context = True
        try:
            fn(self)
        finally:
            self._detached_context = saved

    # ------------------------------------------------------------------
    # 45. send
    # ------------------------------------------------------------------

    def send(self, service: str, operation: str, request: Any) -> None:
        """Send a fire-and-forget request to an external service."""
        if self._mode == "replay":
            self._replay_next("send")
            return
        self._record("send", service=service, operation=operation, request=request)

    # ------------------------------------------------------------------
    # 46. schedule_invoke
    # ------------------------------------------------------------------

    def schedule_invoke(
        self,
        service: str,
        operation: str,
        request: Any,
        delay_ms: int,
    ) -> None:
        """Schedule a delayed one-shot invocation."""
        if self._mode == "replay":
            self._replay_next("schedule_invoke")
            return
        self._record(
            "schedule_invoke",
            service=service, operation=operation, request=request, delay_ms=delay_ms,
        )

    # ------------------------------------------------------------------
    # 47. schedule_cron
    # ------------------------------------------------------------------

    def schedule_cron(
        self,
        workflow_name: str,
        cron_expr: str,
        timezone: str,
        input_json: str,
    ) -> str:
        """Create a recurring workflow trigger from a cron expression.

        Returns
        -------
        str
            The schedule ID.
        """
        if self._mode == "replay":
            return self._replay_next("schedule_cron")
        self._cron_counter += 1
        schedule_id = f"local-cron-{self._cron_counter}"
        self._cron_schedules[schedule_id] = {
            "workflow_name": workflow_name,
            "cron_expr": cron_expr,
            "timezone": timezone,
            "input_json": input_json,
            "enabled": True,
        }
        self._record(
            "schedule_cron", result=schedule_id,
            workflow_name=workflow_name, cron_expr=cron_expr,
            timezone=timezone, input_json=input_json,
        )
        return schedule_id

    # ------------------------------------------------------------------
    # 48. delete_cron
    # ------------------------------------------------------------------

    def delete_cron(self, schedule_id: str) -> None:
        """Remove a recurring cron-triggered workflow schedule."""
        if self._mode == "replay":
            self._replay_next("delete_cron")
            return
        self._cron_schedules.pop(schedule_id, None)
        self._record("delete_cron", schedule_id=schedule_id)

    # ------------------------------------------------------------------
    # 49. list_crons
    # ------------------------------------------------------------------

    def list_crons(self) -> list[dict]:
        """List all registered cron-triggered workflow schedules.

        Returns
        -------
        list[dict]
            A list of schedule objects.
        """
        if self._mode == "replay":
            return self._replay_next("list_crons")
        result = [
            {"schedule_id": sid, **info}
            for sid, info in self._cron_schedules.items()
        ]
        self._record("list_crons", result=result)
        return result

    # ------------------------------------------------------------------
    # 50. acquire_lock
    # ------------------------------------------------------------------

    def acquire_lock(self, key: str, ttl_seconds: float) -> bool:
        """Attempt to acquire a concurrency lock for the given key."""
        return self.acquire_lock_ms(key, int(ttl_seconds * 1000))

    def acquire_lock_ms(self, key: str, ttl_ms: int) -> bool:
        """Attempt to acquire a concurrency lock for the given key, ms."""
        if self._mode == "replay":
            return self._replay_next("acquire_lock_ms")
        if key in self._locks:
            result = False
        else:
            self._locks.add(key)
            result = True
        self._record("acquire_lock_ms", result=result, key=key, ttl_ms=ttl_ms)
        return result

    # ------------------------------------------------------------------
    # 51. release_lock
    # ------------------------------------------------------------------

    def release_lock(self, key: str) -> None:
        """Release a concurrency lock previously acquired."""
        if self._mode == "replay":
            self._replay_next("release_lock")
            return
        self._locks.discard(key)
        self._record("release_lock", key=key)

    # ------------------------------------------------------------------
    # Plugin methods
    # ------------------------------------------------------------------

    def plugin_call(self, plugin_name: str, function_name: str, input: Any) -> str:
        """Call a plugin host function.

        In record mode, returns a mock response reflecting the plugin
        name and function.  In replay mode, returns the recorded result.
        """
        if self._mode == "replay":
            return self._replay_next("plugin_call")
        result = json.dumps(
            {
                "status": "ok",
                "plugin": plugin_name,
                "function": function_name,
                "echo": self._marshal(input),
            }
        )
        self._record(
            "plugin_call", result=result,
            plugin_name=plugin_name, function_name=function_name, input=input,
        )
        return result

    def plugin_call_typed(
        self, plugin_name: str, function_name: str, input: Any, result_type: type[T]
    ) -> T:
        """Typed variant of :meth:`plugin_call`."""
        response = self.plugin_call(plugin_name, function_name, input)
        data = json.loads(response)
        if isinstance(data, dict):
            return result_type(**data)
        return result_type(data)

    def plugin_call_streaming(
        self, plugin_name: str, function_name: str, input: Any
    ) -> Iterator[dict]:
        """Call a plugin function that returns a stream of events.

        In record mode, yields one mock event then stops.
        In replay mode, yields recorded events from the log.
        """
        if self._mode == "replay":
            yield self._replay_next("plugin_call_streaming")
            return
        event = {"status": "ok", "plugin": plugin_name, "function": function_name}
        self._record("plugin_call_streaming", result=event)
        yield event

    # ------------------------------------------------------------------
    # Signal injection for testing
    # ------------------------------------------------------------------

    def inject_signal(self, name: str, payload: str = "") -> None:
        """Inject a signal for testing purposes.

        The signal will be available for :meth:`poll_signal` and
        :meth:`await_signals` immediately.

        Parameters
        ----------
        name : str
            The signal name.
        payload : str
            The signal payload string.
        """
        self._signals.append({"name": name, "payload": payload})

    def __repr__(self) -> str:
        return f"LocalHostCalls(mode={self._mode!r})"
