"""
Local in-process runtime for developing and testing Cleat workflows without WASM.

The :class:`LocalRuntime` routes ``HostCalls.cleat_call()`` invocations
to actual Python service implementations, stores query state in memory,
and manages workflow lifecycle.  This is a **development-only** stand-in
for the Cleat WASM host runtime.

Usage::

    from workflow import place_order_workflow
    from services import DBService, NotifierService
    from runtime import LocalRuntime

    rt = LocalRuntime(db=DBService(), notifier=NotifierService())

    run_id = rt.start_workflow(
        place_order_workflow,
        customer="Alice",
        item="Widget",
        quantity=2,
    )
    order_id = rt.get_query_state(run_id, "order_id")

Implementation note:
    The ``@cleat_entry`` decorator wraps the original function in a WASM
    export wrapper.  The local runtime accesses the original function via
    the ``__wrapped__`` attribute that ``functools.wraps`` sets on the
    wrapper.  See ISSUES.md Issue #1 for details.
"""

from __future__ import annotations

import json
import time
import uuid
from typing import Any, Callable, Optional

from cleat_sdk.host_calls import HostCalls, SuspendSentinel
from cleat_sdk.test_harness import CleatTestHarness


class _LocalHostCalls(CleatTestHarness):
    """A HostCalls surrogate that routes ``cleat_call`` to local services.

    Extends :class:`CleatTestHarness` so that test assertions
    (``call_count``, ``call_history``) remain available while still
    executing real service logic.
    """

    def __init__(self, run_id: str, query_store: dict[str, str]) -> None:
        super().__init__()
        self._local_run_id = run_id
        self._local_query_store = query_store
        self._local_services: dict[str, Any] = {}

    def register_service(self, name: str, instance: Any) -> None:
        """Register a host service instance for dispatching.

        ``cleat_call(name, op, req)`` will call
        ``getattr(instance, op)(**req)``.
        """
        self._local_services[name] = instance

    # --- overrides ------------------------------------------------------

    def cleat_call(
        self, service: str, operation: str, request: Any
    ) -> str:
        """Route to a registered local service instead of using stubs."""
        req_str = self._marshal(request)

        svc = self._local_services.get(service)
        if svc is None:
            raise RuntimeError(
                f"LocalRuntime: no service registered for '{service}'"
            )

        method = getattr(svc, operation, None)
        if method is None:
            raise RuntimeError(
                f"LocalRuntime: service '{service}' has no operation "
                f"'{operation}'"
            )

        request_data: dict[str, Any] = (
            json.loads(req_str) if isinstance(request, (dict, list)) else {}
        )
        result = method(**request_data)

        # Record the call in history (for test assertions).
        result_json = json.dumps(result) if not isinstance(result, str) else result
        from cleat_sdk.test_harness import CallRecord  # noqa: I001
        self.call_history.append(
            CallRecord(
                service=service,
                operation=operation,
                request=req_str,
                response=result_json,
            )
        )

        return result_json

    def set_query_state(self, key: str, value: str) -> None:
        """Store query state in the runtime's cross-workflow store."""
        self._local_query_store[f"{self._local_run_id}:{key}"] = value

    def current_workflow_id(self) -> str:
        return self._local_run_id


class LocalRuntime:
    """In-process runtime for developing Cleat workflows locally.

    Routes ``cleat_call("service", "op", {...})`` from the workflow to
    actual Python services.  Manages query state, workflow lifecycle, and
    result tracking.

    Parameters
    ----------
    db:
        Instance of :class:`services.DBService`.
    notifier:
        Instance of :class:`services.NotifierService`.
    """

    def __init__(
        self,
        db: Any,
        notifier: Any,
    ) -> None:
        self._services = {"db": db, "notifier": notifier}
        self._query_state: dict[str, str] = {}
        self._workflow_status: dict[str, dict] = {}

    def start_workflow(
        self, workflow_fn: Callable, **kwargs: Any
    ) -> str:
        """Execute a workflow function locally.

        Parameters
        ----------
        workflow_fn:
            A function decorated with ``@cleat_entry``.  The original
            function is accessed via ``__wrapped__``.
        **kwargs:
            Workflow parameters (must match the function signature,
            excluding ``HostCalls``).

        Returns
        -------
        str
            A locally-assigned run ID.

        Raises
        ------
        RuntimeError
            If the workflow raises an exception (other than
            ``SuspendSentinel``).
        """
        # Access the original function unwrapped from @cleat_entry.
        original_fn = getattr(workflow_fn, "__wrapped__", workflow_fn)

        run_id = str(uuid.uuid4())
        h = self._make_host_calls(run_id)

        try:
            result = original_fn(h, **kwargs)
            self._workflow_status[run_id] = {
                "status": "completed",
                "result": result,
            }
        except SuspendSentinel:
            # Workflow signalled suspension (timer / signal wait).
            self._workflow_status[run_id] = {"status": "suspended"}
        except Exception as exc:
            self._workflow_status[run_id] = {
                "status": "failed",
                "error": str(exc),
            }
            raise

        return run_id

    def get_query_state(self, run_id: str, key: str) -> Optional[str]:
        """Retrieve a query-state value set by the workflow.

        Parameters
        ----------
        run_id:
            The workflow run ID.
        key:
            The query state key.

        Returns
        -------
        str or None
            The value, or ``None`` if not yet set.
        """
        return self._query_state.get(f"{run_id}:{key}")

    def get_workflow_status(self, run_id: str) -> dict:
        """Return the current status of a workflow execution.

        Returns a dict with keys ``status``, ``result`` (if completed),
        or ``error`` (if failed).
        """
        return self._workflow_status.get(run_id, {"status": "unknown"})

    def wait_for_query_state(
        self, run_id: str, key: str, timeout_sec: float = 10.0, poll_ms: float = 50.0
    ) -> Optional[str]:
        """Poll until a query-state value becomes available.

        Parameters
        ----------
        run_id:
            The workflow run ID.
        key:
            The query state key.
        timeout_sec:
            Maximum time to wait (seconds).
        poll_ms:
            Polling interval (milliseconds).

        Returns
        -------
        str or None
            The value, or ``None`` if the timeout expires.
        """
        deadline = time.monotonic() + timeout_sec
        while time.monotonic() < deadline:
            val = self.get_query_state(run_id, key)
            if val is not None:
                return val
            time.sleep(poll_ms / 1000.0)
        return None

    # -- internal helpers ------------------------------------------------

    def _make_host_calls(self, run_id: str) -> _LocalHostCalls:
        """Create a ``_LocalHostCalls`` instance with all services registered."""
        h = _LocalHostCalls(run_id=run_id, query_store=self._query_state)
        for name, svc in self._services.items():
            h.register_service(name, svc)
        return h
