"""
Cleat-LangGraph Host Integration

This module provides the integration point between the Cleat Runtime and
the LangGraph host service. It registers the LangGraphRuntime as a Cleat
host service that can handle ``durable_call("langgraph", ...)`` invocations
from Cleat workflows running inside the WASM sandbox.

Architecture
------------
The Cleat Runtime has a service registry that maps service name → handler.
When a Cleat workflow calls ``h.durable_call("langgraph", "step", {"graph_name": "...", ...})``,
the runtime dispatches to the handler registered under ``"langgraph"``.

This module provides:
1. ``register_langgraph_service(runtime)`` — registers the LangGraphRuntime
   with a Cleat-compatible runtime.
2. ``LangGraphServiceHandler`` — wraps ``LangGraphRuntime.handle_request()``
   into the format expected by the Cleat service registry.

Usage
-----
In your application's entry point (host side)::

    from cleat_langgraph import LangGraphRuntime
    from cleat_langgraph.host import register_langgraph_service
    from my_agent.graph import make_agent_graph

    # 1. Create the LangGraph runtime
    langgraph = LangGraphRuntime()

    # 2. Register graphs
    langgraph.register_graph("react-agent", make_agent_graph)

    # 3. Register with the Cleat runtime
    cleat_runtime = CleatRuntime()  # provided by the host environment
    register_langgraph_service(cleat_runtime, langgraph)

    # 4. Start the runtime
    cleat_runtime.start()
"""

from __future__ import annotations

import json
import logging
from typing import Any, Callable, Dict, Optional

from cleat_langgraph.bridge import LangGraphRuntime

logger = logging.getLogger(__name__)


# ---------------------------------------------------------------------------
# Cleat Service Handler
# ---------------------------------------------------------------------------


class LangGraphServiceHandler:
    """Wraps ``LangGraphRuntime`` as a Cleat host service handler.

    The handler is invoked by the Cleat runtime when a workflow calls
    ``durable_call("langgraph", operation, request)``.

    The handler:
    1. Receives the operation name and request dict from the Cleat runtime.
    2. Delegates to ``LangGraphRuntime.handle_request(operation, request)``.
    3. Returns the response (which is recorded in the event log for replay).
    """

    def __init__(self, runtime: LangGraphRuntime):
        self._runtime = runtime

    def handle_call(
        self, operation: str, request: Dict[str, Any]
    ) -> Any:
        """Handle a durable_call invocation from a Cleat workflow.

        Args:
            operation: The operation name (e.g., ``"step"``, ``"route"``,
                ``"execute_node"``, ``"invoke_graph"``).
            request: A JSON-safe dict with operation-specific parameters.

        Returns:
            The operation result, which must be JSON-serializable for
            Cleat's event log.
        """
        logger.debug(
            "LangGraph service call: operation=%s request_keys=%s",
            operation,
            list(request.keys()) if isinstance(request, dict) else "N/A",
        )
        try:
            result = self._runtime.handle_request(operation, request)
            return result
        except Exception as e:
            logger.error(
                "LangGraph service error: operation=%s error=%s",
                operation,
                e,
            )
            raise


# ---------------------------------------------------------------------------
# Registration helpers
# ---------------------------------------------------------------------------


def register_langgraph_service(
    cleat_runtime: Any,
    langgraph_runtime: LangGraphRuntime,
    service_name: str = "langgraph",
) -> None:
    """Register the LangGraphRuntime as a Cleat host service.

    After registration, Cleat workflows can call::

        h.durable_call("langgraph", "step", {
            "graph_name": "my-graph",
            "state": {...},
        })

    Args:
        cleat_runtime: The Cleat runtime instance (which has a
            ``register_service(name, handler)`` or similar method).
        langgraph_runtime: The ``LangGraphRuntime`` instance with
            registered graphs.
        service_name: The service name to register (default:
            ``"langgraph"``). This is the first argument to
            ``durable_call()``.
    """
    handler = LangGraphServiceHandler(langgraph_runtime)

    # Try different registration APIs depending on the runtime version
    if hasattr(cleat_runtime, "register_service"):
        cleat_runtime.register_service(service_name, handler.handle_call)
    elif hasattr(cleat_runtime, "services"):
        cleat_runtime.services[service_name] = handler.handle_call
    elif hasattr(cleat_runtime, "service_registry"):
        cleat_runtime.service_registry[service_name] = handler.handle_call
    else:
        # Fallback: monkey-patch or log
        logger.warning(
            "Cleat runtime %r does not have a standard registration API. "
            "Service '%s' must be registered manually. "
            "Expected method: register_service(name, handler).",
            type(cleat_runtime).__name__,
            service_name,
        )
        # Attempt to set attribute
        if hasattr(cleat_runtime, "__dict__"):
            cleat_runtime.__dict__.setdefault("_services", {})[
                service_name
            ] = handler.handle_call

    logger.info(
        "Registered LangGraph service '%s' with %d graphs and %d entrypoints",
        service_name,
        len(langgraph_runtime._graphs) if hasattr(langgraph_runtime, "_graphs") else 0,
        len(langgraph_runtime._entrypoints)
        if hasattr(langgraph_runtime, "_entrypoints")
        else 0,
    )


# ---------------------------------------------------------------------------
# Convenience: Create and register in one step
# ---------------------------------------------------------------------------


def setup_langgraph_service(
    cleat_runtime: Any,
    graphs: Optional[Dict[str, Callable]] = None,
    entrypoints: Optional[Dict[str, Callable]] = None,
    tasks: Optional[Dict[str, list]] = None,
    service_name: str = "langgraph",
) -> LangGraphRuntime:
    """Create a LangGraphRuntime, register graphs/entrypoints, and
    register with the Cleat runtime.

    Convenience function for simple setups::

        from cleat_runtime import CleatRuntime
        from cleat_langgraph.host import setup_langgraph_service
        from my_agent.graph import make_agent_graph

        runtime = CleatRuntime()
        setup_langgraph_service(
            runtime,
            graphs={"my-agent": make_agent_graph},
        )
        runtime.start()

    Args:
        cleat_runtime: The Cleat runtime instance.
        graphs: Optional dict of ``{name: graph_builder}`` for StateGraphs.
        entrypoints: Optional dict of ``{name: entrypoint_fn}``.
        tasks: Optional dict of ``{entrypoint_name: [task_fns]}``.
        service_name: Service name for ``durable_call()``.

    Returns:
        The ``LangGraphRuntime`` instance for advanced configuration.
    """
    langgraph = LangGraphRuntime()

    if graphs:
        for name, builder in graphs.items():
            langgraph.register_graph(name, builder)

    if entrypoints:
        for name, ep_fn in entrypoints.items():
            task_list = (tasks or {}).get(name, [])
            langgraph.register_entrypoint(name, ep_fn, tasks=task_list)

    register_langgraph_service(cleat_runtime, langgraph, service_name)

    return langgraph
