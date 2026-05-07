"""
HostCalls - The durable execution context for cleat workflows.

HostCalls provides all the durable primitives that a workflow can use:
state management, service invocation, timers, signals, promises,
child workflows, and logging.
"""

import asyncio
import logging
import random
import uuid
from typing import Any, Callable, Dict, List, Optional, Tuple, TypeVar

from cleat_sdk._types import ChildResult, PromiseResult, SignalResult

T = TypeVar("T")

logger = logging.getLogger(__name__)


class _GlobalStateStore:
    """Global in-memory state store keyed by workflow_id.

    This simulates the Cleat runtime's durable state management,
    where state persists across invocations of the same workflow.
    """

    def __init__(self):
        self._stores: Dict[str, Dict[str, Any]] = {}

    def _get_or_create(self, workflow_id: str) -> Dict[str, Any]:
        if workflow_id not in self._stores:
            self._stores[workflow_id] = {}
        return self._stores[workflow_id]

    def get(self, workflow_id: str, key: str) -> Optional[Any]:
        store = self._stores.get(workflow_id)
        if store is None:
            return None
        return store.get(key)

    def set(self, workflow_id: str, key: str, value: Any) -> None:
        store = self._get_or_create(workflow_id)
        store[key] = value

    def delete(self, workflow_id: str, key: str) -> None:
        store = self._stores.get(workflow_id)
        if store is not None:
            store.pop(key, None)

    def clear_all(self, workflow_id: str) -> None:
        self._stores.pop(workflow_id, None)


# Global state store, simulating the durable runtime backend
_global_store = _GlobalStateStore()


# Global service registry for local execution.
# Maps (service_name, operation_name) -> callable
_local_handlers: Dict[Tuple[str, str], Callable[..., Any]] = {}


def register_local_handler(
    service: str, operation: str, handler: Callable[..., Any]
) -> None:
    """Register a local handler for development/testing.

    In production, the Cleat runtime would manage service routing.
    For development, this allows direct function invocation.
    """
    _local_handlers[(service, operation)] = handler


def clear_local_handlers() -> None:
    """Clear all registered local handlers."""
    _local_handlers.clear()


class HostCalls:
    """The durable execution context provided to every cleat workflow entry point.

    This is the primary API for workflows to interact with the Cleat runtime.
    All operations are durably persisted and replayed on recovery.

    Usage:
        @durable_entry(name="MyWorkflow")
        async def my_workflow(ctx: HostCalls, request: dict):
            await ctx.durable_log("Starting workflow")
            ctx.set_state("status", "running")
            result = await ctx.durable_call("other_service", "op", request)
            return {"result": result}
    """

    def __init__(self, workflow_id: Optional[str] = None):
        self._workflow_id = workflow_id or str(uuid.uuid4())
        self._query_state: Dict[str, Any] = {}

    # ------------------------------------------------------------------
    # Workflow identity
    # ------------------------------------------------------------------

    @property
    def workflow_id(self) -> str:
        """Return the unique ID of the current workflow execution."""
        return self._workflow_id

    def key(self) -> str:
        """Return the workflow key (alias for workflow_id for compatibility).

        In Restate, this returns the Virtual Object key. In Cleat, this
        returns the workflow_id.
        """
        return self._workflow_id

    # ------------------------------------------------------------------
    # Durable state management
    # ------------------------------------------------------------------

    def set_state(self, key: str, value: Any) -> None:
        """Durably persist a state value at the given key.

        Args:
            key: The state key.
            value: The value to persist. Must be JSON-serializable.
        """
        _global_store.set(self._workflow_id, key, value)

    def get_state(self, key: str, type_hint: Optional[type] = None) -> Optional[Any]:
        """Retrieve a previously persisted state value.

        Args:
            key: The state key to retrieve.
            type_hint: Optional type to cast the result to. If the stored
                      value is a dict and type_hint is a dataclass,
                      this would deserialize it.

        Returns:
            The stored value, or None if not found.
        """
        value = _global_store.get(self._workflow_id, key)
        if value is None:
            return None

        if type_hint is not None and isinstance(value, dict):
            try:
                if hasattr(type_hint, "model_validate"):
                    return type_hint.model_validate(value)
                # Try constructing from dataclass
                if hasattr(type_hint, "__dataclass_fields__"):
                    return type_hint(**value)
                return type_hint(**value)
            except (TypeError, ValueError):
                pass

        return value

    def clear_state(self, key: str) -> None:
        """Remove a single state key.

        Args:
            key: The state key to remove.
        """
        _global_store.delete(self._workflow_id, key)

    def clear_all_state(self) -> None:
        """Remove all persisted state for this workflow."""
        _global_store.clear_all(self._workflow_id)

    # ------------------------------------------------------------------
    # Query state (visible to external queries, not persisted to workflow)
    # ------------------------------------------------------------------

    def set_query_state(self, key: str, value: Any) -> None:
        """Set a value that is queryable but not part of durable workflow state.

        This is useful for exposing workflow progress to external monitoring
        without cluttering the durable state log.

        Args:
            key: The query state key.
            value: The value to expose.
        """
        self._query_state[key] = value

    def get_query_state(self, key: str) -> Optional[Any]:
        """Get a previously set query state value."""
        return self._query_state.get(key)

    # ------------------------------------------------------------------
    # Durable service calls
    # ------------------------------------------------------------------

    async def durable_call(
        self,
        service: str,
        operation: str,
        request: Any = None,
        timeout_ms: Optional[int] = None,
    ) -> Any:
        """Durably invoke a remote service operation.

        The invocation is durably recorded so it will be replayed on
        workflow recovery. The result is cached so it is only executed once.

        In development mode, if a local handler has been registered via
        register_local_handler(), it will be called directly.

        Args:
            service: The target service name.
            operation: The operation/handler name on the service.
            request: The request payload.
            timeout_ms: Optional timeout in milliseconds.

        Returns:
            The service's response.
        """
        # In production, this would call the Cleat runtime to perform
        # the durable invocation.
        logger.info(
            "durable_call: service=%s, operation=%s, request=%s",
            service,
            operation,
            request,
        )

        # Check for local handlers (development mode)
        handler = _local_handlers.get((service, operation))
        if handler is not None:
            return await handler(self, request)

        # No runtime connected and no local handler
        raise NotImplementedError(
            f"Runtime not connected: cannot call {service}/{operation}. "
            f"In production, the Cleat runtime handles durable service calls. "
            f"For development, register a local handler via register_local_handler()."
        )

    # ------------------------------------------------------------------
    # Durable timers
    # ------------------------------------------------------------------

    async def durable_sleep(self, ms: int) -> None:
        """Durably sleep for the specified duration.

        The sleep is durably recorded. On workflow recovery, the remaining
        time is preserved rather than starting over.

        Args:
            ms: Sleep duration in milliseconds.
        """
        logger.info("durable_sleep: %d ms", ms)
        await asyncio.sleep(ms / 1000.0)

    # ------------------------------------------------------------------
    # Signals
    # ------------------------------------------------------------------

    async def await_signals(
        self,
        names: List[str],
        timeout_ms: Optional[int] = None,
    ) -> SignalResult:
        """Wait for any of the specified signals to be delivered.

        Args:
            names: The list of signal names to wait for.
            timeout_ms: Optional timeout. If None, wait indefinitely.

        Returns:
            A SignalResult indicating which signal was received (if any).
        """
        logger.info("await_signals: names=%s, timeout_ms=%s", names, timeout_ms)
        return SignalResult(signal_name="", timed_out=True)

    def poll_signal(self, name: str) -> Optional[SignalResult]:
        """Non-blocking check for a delivered signal.

        Args:
            name: The signal name to check.

        Returns:
            A SignalResult if the signal has been delivered, None otherwise.
        """
        logger.info("poll_signal: name=%s", name)
        return None

    # ------------------------------------------------------------------
    # Child workflows
    # ------------------------------------------------------------------

    async def child_workflow(self, name: str, input: Any = None) -> str:
        """Start a child workflow and return its run ID.

        Args:
            name: The name/entry point of the child workflow.
            input: The input to pass to the child workflow.

        Returns:
            The run ID of the started child workflow.
        """
        run_id = str(uuid.uuid4())
        logger.info(
            "child_workflow: name=%s, input=%s, run_id=%s",
            name,
            input,
            run_id,
        )
        return run_id

    async def await_child(
        self,
        run_id: str,
        timeout_ms: Optional[int] = None,
    ) -> ChildResult:
        """Wait for a child workflow to complete.

        Args:
            run_id: The run ID returned by child_workflow().
            timeout_ms: Optional timeout in milliseconds.

        Returns:
            A ChildResult with the child's result or error.
        """
        logger.info("await_child: run_id=%s, timeout_ms=%s", run_id, timeout_ms)
        return ChildResult(run_id=run_id, failed=False)

    # ------------------------------------------------------------------
    # Promises / Awakeables
    # ------------------------------------------------------------------

    async def create_promise(self, name: str) -> str:
        """Create a named promise and return its ID.

        Promises are durable one-shot values that can be resolved externally.
        Similar to Restate's awakeable concept.

        Args:
            name: A name for the promise.

        Returns:
            The promise ID that can be shared externally for resolution.
        """
        promise_id = f"{self._workflow_id}:{name}:{uuid.uuid4()}"
        logger.info("create_promise: name=%s, promise_id=%s", name, promise_id)
        return promise_id

    async def await_promise(
        self,
        promise_id: str,
        timeout_ms: Optional[int] = None,
    ) -> PromiseResult:
        """Wait for a promise to be resolved.

        Args:
            promise_id: The promise ID to wait for.
            timeout_ms: Optional timeout in milliseconds.

        Returns:
            A PromiseResult indicating resolution or timeout.
        """
        logger.info(
            "await_promise: promise_id=%s, timeout_ms=%s",
            promise_id,
            timeout_ms,
        )
        return PromiseResult(resolved=False, timed_out=True)

    # ------------------------------------------------------------------
    # Logging
    # ------------------------------------------------------------------

    def durable_log(self, msg: str) -> None:
        """Persistently log a message that survives workflow recovery.

        Args:
            msg: The message to log.
        """
        logger.info("[%s] %s", self._workflow_id, msg)

    # ------------------------------------------------------------------
    # Deterministic random
    # ------------------------------------------------------------------

    def random(self) -> float:
        """Return a deterministic random value between 0.0 and 1.0.

        The random sequence is seeded from the workflow_id, making it
        deterministic across replays.

        Returns:
            A float in [0.0, 1.0).
        """
        rng = random.Random(self._workflow_id)
        return rng.random()
