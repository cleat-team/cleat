"""
Cleat-LangGraph Bridge: Core SDK

This module provides the two-sided bridge between Cleat's durable execution
framework and LangGraph's agent-execution framework.

Host Side (``LangGraphRuntime``)
    Runs outside the WASM sandbox. Manages LangGraph graph compilation,
    node execution, and routing. Each call from the Cleat workflow is
    stateless — all state is passed through the request/response cycle.

Workflow Side (``CleatLangGraph``)
    Runs inside the WASM sandbox (Cleat workflow). Wraps
    ``HostCalls.cleat_call()`` into a convenient API for stepping
    through LangGraph graphs with per-node durability.

Usage
-----
Host side setup::

    from cleat_langgraph import LangGraphRuntime
    from my_agent.graph import make_agent_graph

    runtime = LangGraphRuntime()
    runtime.register_graph("react-agent", make_agent_graph)

    # Later, process incoming cleat_calls:
    #   runtime.handle_route(graph_name, state) → {"next_node": ...}
    #   runtime.handle_execute(graph_name, node, state) → {"state": ...}

Cleat workflow::

    from cleat_sdk import HostCalls, cleat_entry
    from cleat_langgraph import CleatLanggraph

    @cleat_entry(name="react_agent")
    def run(h: HostCalls, query: str) -> str:
        agent = CleatLangGraph(h, "react-agent")
        state = {"input": query, "messages": [], "final_answer": ""}

        while not state.get("done"):
            step_result = agent.step(state)
            state = step_result["state"]

        return state["final_answer"]
"""

from __future__ import annotations

import json
import logging
import time
from typing import Any, Callable, Dict, List, Optional, Tuple

from cleat_langgraph.serialization import (
    CleatSerializer,
    LangGraphSerializer,
)

logger = logging.getLogger(__name__)


# ===================================================================
# Host-Side Runtime (outside WASM)
# ===================================================================


class LangGraphRuntime:
    """Host-side runtime that manages LangGraph graph execution.

    Compiles and caches LangGraph graphs, executes individual nodes,
    and handles routing between nodes. Called from Cleat workflows
    via cleat_call (which goes through handle_request()).

    This class runs OUTSIDE the WASM sandbox, so it has full access
    to Python, async, and third-party libraries.
    """

    def __init__(self) -> None:
        # name -> compiled StateGraph
        self._graphs: Dict[str, Any] = {}
        # name -> graph metadata (nodes, edges, structure)
        self._graph_meta: Dict[str, Dict[str, Any]] = {}
        # name -> {node_name: callable}
        self._node_functions: Dict[str, Dict[str, Callable]] = {}
        # name -> callable (Functional API entrypoint)
        self._entrypoints: Dict[str, Callable] = {}
        # name -> [task callables] (Functional API tasks)
        self._tasks: Dict[str, List[Callable]] = {}

    # ------------------------------------------------------------------
    # Registration
    # ------------------------------------------------------------------

    def register_graph(self, name: str, graph_builder: Callable) -> None:
        """Register a LangGraph StateGraph for use from Cleat workflows.

        Args:
            name: Unique name for this graph (used in cleat_call).
            graph_builder: A callable that returns a ``StateGraph`` instance
                (or compiled graph). The builder is invoked immediately
                to compile and cache the graph.
        """
        graph = graph_builder()
        # Check if already compiled
        if hasattr(graph, "compile"):
            compiled = graph.compile()
        else:
            compiled = graph  # assume already compiled

        self._graphs[name] = compiled
        self._graph_meta[name] = self._extract_meta(compiled)
        self._node_functions[name] = {}
        logger.info(
            "Registered LangGraph graph '%s' with %d nodes",
            name,
            len(self._graph_meta[name].get("nodes", [])),
        )

    def register_entrypoint(
        self,
        name: str,
        entrypoint_fn: Callable,
        tasks: Optional[List[Callable]] = None,
    ) -> None:
        """Register a LangGraph Functional API entrypoint.

        Args:
            name: Unique name for this entrypoint.
            entrypoint_fn: The ``@entrypoint`` decorated function.
            tasks: List of ``@task`` decorated functions used by the entrypoint.
        """
        self._entrypoints[name] = entrypoint_fn
        self._node_functions[name] = {}
        if tasks:
            self._tasks[name] = tasks
            for task_fn in tasks:
                self._node_functions[name][task_fn.__name__] = task_fn
        logger.info("Registered LangGraph entrypoint '%s'", name)

    # ------------------------------------------------------------------
    # Request handling (called from Cleat runtime)
    # ------------------------------------------------------------------

    def handle_request(self, operation: str, request: Dict[str, Any]) -> Any:
        """Dispatch a cleat_call request to the appropriate handler.

        This is the single entry point for all ``cleat_call("langgraph", …)``
        invocations from Cleat workflows.

        Supported operations:
          - ``"step"``: Execute one node and return updated state.
          - ``"route"``: Determine the next node to execute.
          - ``"invoke_graph"``: Run a full graph in one call (coarse granularity).
          - ``"invoke_entrypoint"``: Run a full entrypoint in one call.
          - ``"execute_node"``: Execute a specific named node.
          - ``"list_nodes"``: Return the list of node names for a graph.
          - ``"graph_info"``: Return graph metadata (nodes, edges, etc.).
        """
        handlers = {
            "step": self._handle_step,
            "route": self._handle_route,
            "invoke_graph": self._handle_invoke_graph,
            "invoke_entrypoint": self._handle_invoke_entrypoint,
            "execute_node": self._handle_execute_node,
            "list_nodes": self._handle_list_nodes,
            "graph_info": self._handle_graph_info,
        }
        handler = handlers.get(operation)
        if handler is None:
            raise ValueError(f"Unknown LangGraph operation: {operation}")
        return handler(request)

    def _handle_route(self, request: Dict[str, Any]) -> Dict[str, Any]:
        """Determine the next node to execute.

        Deserializes the state, applies the graph's routing logic
        (conditional edges → next node name), and returns the result.

        Args:
            request: Must contain ``graph_name`` and ``state``.

        Returns:
            dict with ``next_node`` (str or ``"__end__"``) and
            optional ``metadata``.
        """
        graph_name = request["graph_name"]
        state = LangGraphSerializer.to_langgraph_state(request["state"])

        if graph_name not in self._graphs:
            raise ValueError(f"Graph '{graph_name}' not registered")

        compiled = self._graphs[graph_name]
        meta = self._graph_meta[graph_name]

        # Determine the current position in the graph
        # The state may contain a __node__ key tracking the last executed node
        last_node = state.get("__node__", "__start__")

        # Find outgoing edges from the last node
        next_node = self._resolve_next_node(compiled, meta, last_node, state)

        logger.debug(
            "Route: graph=%s last_node=%s next_node=%s",
            graph_name,
            last_node,
            next_node,
        )

        return {"next_node": next_node, "metadata": {"last_node": last_node}}

    def _handle_step(self, request: Dict[str, Any]) -> Dict[str, Any]:
        """Execute one step of the graph.

        Combines ``route`` + ``execute_node`` into a single call.
        This is the most common operation for Cleat workflows.

        Args:
            request: Must contain ``graph_name`` and ``state``.

        Returns:
            dict with ``state`` (updated), ``next_node`` (or ``"__end__"``),
            ``node_executed``, and ``done`` (bool).
        """
        graph_name = request["graph_name"]
        state = LangGraphSerializer.to_langgraph_state(request["state"])

        if graph_name not in self._graphs:
            raise ValueError(f"Graph '{graph_name}' not registered")

        compiled = self._graphs[graph_name]
        meta = self._graph_meta[graph_name]

        # Route: determine which node to execute
        last_node = state.get("__node__", "__start__")
        next_node = self._resolve_next_node(compiled, meta, last_node, state)

        if next_node == "__end__":
            return {
                "state": LangGraphSerializer.from_langgraph_state(state),
                "next_node": "__end__",
                "node_executed": None,
                "done": True,
            }

        # Execute the node
        new_state = self._execute_node_fn(
            compiled, meta, graph_name, next_node, state
        )
        new_state["__node__"] = next_node

        logger.debug(
            "Step: graph=%s node=%s executed",
            graph_name,
            next_node,
        )

        return {
            "state": LangGraphSerializer.from_langgraph_state(new_state),
            "next_node": next_node,
            "node_executed": next_node,
            "done": False,
        }

    def _handle_execute_node(self, request: Dict[str, Any]) -> Dict[str, Any]:
        """Execute a specific named node.

        Args:
            request: Must contain ``graph_name``, ``node_name``, and ``state``.

        Returns:
            dict with ``state`` (updated).
        """
        graph_name = request["graph_name"]
        node_name = request["node_name"]
        state = LangGraphSerializer.to_langgraph_state(request["state"])

        if graph_name not in self._graphs:
            raise ValueError(f"Graph '{graph_name}' not registered")

        compiled = self._graphs[graph_name]
        meta = self._graph_meta[graph_name]

        new_state = self._execute_node_fn(
            compiled, meta, graph_name, node_name, state
        )
        new_state["__node__"] = node_name

        return {"state": LangGraphSerializer.from_langgraph_state(new_state)}

    def _handle_invoke_graph(self, request: Dict[str, Any]) -> Dict[str, Any]:
        """Execute a full graph in a single call.

        Coarser granularity but simpler. Each full execution is one
        durable event rather than per-node events.

        Args:
            request: Must contain ``graph_name`` and ``state``.

        Returns:
            dict with ``state`` (final) and ``result``.
        """
        graph_name = request["graph_name"]
        state = LangGraphSerializer.to_langgraph_state(request["state"])

        if graph_name not in self._graphs:
            raise ValueError(f"Graph '{graph_name}' not registered")

        compiled = self._graphs[graph_name]
        result = compiled.invoke(state)

        serialized = LangGraphSerializer.from_langgraph_state(
            result if isinstance(result, dict) else {}
        )

        return {
            "state": serialized,
            "result": LangGraphSerializer.serialize_result(result),
        }

    def _handle_invoke_entrypoint(self, request: Dict[str, Any]) -> Dict[str, Any]:
        """Execute a Functional API entrypoint in a single call.

        Args:
            request: Must contain ``entrypoint_name`` and ``input``.

        Returns:
            dict with ``result``.
        """
        ep_name = request["entrypoint_name"]
        input_data = LangGraphSerializer.deserialize_input(request.get("input", {}))

        if ep_name not in self._entrypoints:
            raise ValueError(f"Entrypoint '{ep_name}' not registered")

        entrypoint_fn = self._entrypoints[ep_name]

        # Functional API entrypoints are async. We run them synchronously
        # by using an event loop. In production, the host runtime would
        # manage the event loop.
        import asyncio

        result = asyncio.run(entrypoint_fn(input_data))

        return {"result": LangGraphSerializer.serialize_result(result)}

    def _handle_list_nodes(self, request: Dict[str, Any]) -> Dict[str, Any]:
        """List all node names for a registered graph."""
        graph_name = request["graph_name"]
        meta = self._graph_meta.get(graph_name)
        if meta is None:
            raise ValueError(f"Graph '{graph_name}' not registered")
        return {
            "nodes": meta.get("nodes", []),
            "edges": meta.get("edges", []),
            "conditional_edges": meta.get("conditional_edges", []),
        }

    def _handle_graph_info(self, request: Dict[str, Any]) -> Dict[str, Any]:
        """Return full metadata for a registered graph."""
        graph_name = request["graph_name"]
        if graph_name not in self._graph_meta:
            raise ValueError(f"Graph '{graph_name}' not registered")
        return dict(self._graph_meta[graph_name])

    # ------------------------------------------------------------------
    # Internal helpers
    # ------------------------------------------------------------------

    def _resolve_next_node(
        self,
        compiled: Any,
        meta: Dict[str, Any],
        current_node: str,
        state: Dict[str, Any],
    ) -> str:
        """Determine the next node from the graph's edge structure.

        Handles:
        - ``START`` → first node
        - Normal edges: ``node_a`` → ``node_b``
        - Conditional edges: runs the routing function
        - End of graph: returns ``__end__``
        """
        # If we're at START, find the first node from the edges
        if current_node == "__start__":
            # Find edges from START
            for edge in meta.get("edges", []):
                if edge.get("from") == "__start__":
                    return edge["to"]
            # Fallback: first node in the list
            nodes = meta.get("nodes", [])
            return nodes[0] if nodes else "__end__"

        # Check for direct edges from the current node
        for edge in meta.get("edges", []):
            if edge.get("from") == current_node:
                return edge["to"]

        # Check for conditional edges
        for cond_edge in meta.get("conditional_edges", []):
            if cond_edge.get("from") == current_node:
                routing_fn_name = cond_edge.get("condition", "")
                routing_fn = self._find_routing_function(
                    compiled, current_node, routing_fn_name
                )
                if routing_fn:
                    try:
                        result = routing_fn(state)
                        return result if isinstance(result, str) else "__end__"
                    except Exception as e:
                        logger.error(
                            "Routing function '%s' failed: %s",
                            routing_fn_name,
                            e,
                        )
                        return "__end__"

        # No outgoing edges — graph is done
        return "__end__"

    def _execute_node_fn(
        self,
        compiled: Any,
        meta: Dict[str, Any],
        graph_name: str,
        node_name: str,
        state: Dict[str, Any],
    ) -> Dict[str, Any]:
        """Execute a single node function.

        Tries in order:
        1. In-memory registered node function (fastest path)
        2. Compiled graph's internal node function
        3. Function lookup from metadata

        Handles both async and sync node functions.
        """
        # Try direct registered function
        node_fns = self._node_functions.get(graph_name, {})
        if node_name in node_fns:
            fn = node_fns[node_name]
            return self._call_node_fn(fn, state)

        # Try extracting the node function from the compiled graph
        try:
            if hasattr(compiled, "get_node"):
                node_def = compiled.get_node(node_name)
                if node_def and hasattr(node_def, "fn"):
                    return self._call_node_fn(node_def.fn, state)
        except Exception:
            pass

        # Try the compiled graph's internal nodes dict
        try:
            if hasattr(compiled, "nodes") and node_name in compiled.nodes:
                fn = compiled.nodes[node_name]
                if isinstance(fn, dict) and "fn" in fn:
                    return self._call_node_fn(fn["fn"], state)
                return self._call_node_fn(fn, state)
        except Exception:
            pass

        raise ValueError(
            f"Cannot find node function '{node_name}' in graph '{graph_name}'"
        )

    def _call_node_fn(
        self, fn: Callable, state: Dict[str, Any]
    ) -> Dict[str, Any]:
        """Call a node function, handling both sync and async variants.

        Node functions typically take ``state`` (a TypedDict) and return
        a partial state update dict (the keys that changed).
        """
        try:
            result = fn(state)
            if hasattr(result, "__await__"):
                # Async function — run in event loop
                import asyncio

                result = asyncio.run(result)
        except TypeError:
            # Some node functions may take **kwargs or have different signatures
            try:
                result = fn(**state)
            except TypeError:
                raise

        # Merge result into state (LangGraph convention: node returns partial updates)
        if isinstance(result, dict):
            merged = dict(state)
            merged.update(result)
            return merged

        # Non-dict result — wrap it
        return dict(state, **{"result": result})

    def _find_routing_function(
        self, compiled: Any, node_name: str, condition_name: str
    ) -> Optional[Callable]:
        """Find the conditional edge routing function.

        LangGraph stores conditional edges mapped to routing functions.
        This helper navigates the compiled graph internals to find them.
        """
        try:
            # Try various internal representations
            if hasattr(compiled, "conditional_edges"):
                edges_map = compiled.conditional_edges
                if node_name in edges_map:
                    # edges_map[node_name] might be a dict {"condition": fn}
                    return edges_map[node_name].get(condition_name)
        except Exception:
            pass
        return None

    @staticmethod
    def _extract_meta(compiled: Any) -> Dict[str, Any]:
        """Extract graph metadata (nodes, edges, conditional edges).

        Works with various LangGraph internal representations.
        """
        nodes: List[str] = []
        edges: List[Dict[str, str]] = []
        conditional_edges: List[Dict[str, str]] = []

        try:
            if hasattr(compiled, "nodes"):
                nodes = list(compiled.nodes.keys())
        except Exception:
            pass

        try:
            if hasattr(compiled, "edges"):
                for src, targets in compiled.edges.items():
                    src_name = str(src) if not hasattr(src, "name") else src.name
                    if isinstance(targets, list):
                        for tgt in targets:
                            tgt_name = (
                                str(tgt) if not hasattr(tgt, "name") else tgt.name
                            )
                            if src_name != tgt_name:  # skip self-edges
                                edges.append({"from": src_name, "to": tgt_name})
                    elif targets is not None:
                        tgt_name = (
                            str(targets)
                            if not hasattr(targets, "name")
                            else targets.name
                        )
                        edges.append({"from": src_name, "to": tgt_name})
        except Exception:
            pass

        try:
            if hasattr(compiled, "conditional_edges"):
                for src, cond_info in compiled.conditional_edges.items():
                    src_name = str(src) if not hasattr(src, "name") else src.name
                    conditional_edges.append(
                        {"from": src_name, "condition": str(cond_info)}
                    )
        except Exception:
            pass

        return {
            "nodes": nodes,
            "edges": edges,
            "conditional_edges": conditional_edges,
        }


# ===================================================================
# Workflow-Side Client (inside WASM)
# ===================================================================


class CleatLangGraph:
    """Cleat workflow client for LangGraph graph execution.

    Wraps ``HostCalls.cleat_call()`` into a convenient API for
    stepping through LangGraph graphs with per-node durability.

    Each call to ``step()`` or ``execute_node()`` is recorded as a
    separate event in Cleat's event log. On replay, cached results
    are returned without re-executing the node.

    Usage::

        from cleat_sdk import HostCalls
        from cleat_langgraph import CleatLangGraph

        @cleat_entry(name="my_workflow")
        def run(h: HostCalls, input_data: str) -> str:
            agent = CleatLangGraph(h, "my-graph")
            state = {"input": input_data}

            while not agent.is_done(state):
                state = agent.step(state)

            return state.get("result", "")
    """

    def __init__(self, h: Any, graph_name: str, service_name: str = "langgraph"):
        """Initialize the Cleat-LangGraph client.

        Args:
            h: The ``HostCalls`` instance from the ``@cleat_entry`` context.
            graph_name: Name of the registered LangGraph graph.
            service_name: Cleat host service name (default: ``"langgraph"``).
        """
        self._h = h
        self._graph_name = graph_name
        self._service_name = service_name
        self._step_count = 0
        self._node_results: list[str] = []

    def step(self, state: dict) -> dict:
        """Execute the next node in the graph.

        Combines routing + node execution into a single cleat_call.
        The current state is packed and sent to the host, which determines
        the next node, executes it, and returns the updated state.

        Args:
            state: Current workflow state dict.

        Returns:
            Updated state dict after executing the next node.

        Raises:
            RuntimeError: If the graph is not registered or the
                operation fails.
        """
        packed = CleatSerializer.pack(state)
        try:
            result = self._h.cleat_call(
                self._service_name,
                "step",
                {"graph_name": self._graph_name, "state": packed},
            )
        except RuntimeError as e:
            raise RuntimeError(
                f"LangGraph step failed for '{self._graph_name}': {e}"
            ) from e

        self._step_count += 1
        new_state = CleatSerializer.unpack(result.get("state", {}))

        if result.get("node_executed"):
            self._node_results.append(str(result["node_executed"]))

        return new_state

    def execute_node(self, node_name: str, state: dict) -> dict:
        """Execute a specific named node.

        Useful when the workflow knows exactly which node to run next
        (e.g., for human-in-the-loop patterns where execution resumes
        at a specific point).

        Args:
            node_name: Name of the node to execute.
            state: Current workflow state.

        Returns:
            Updated state after node execution.
        """
        try:
            result = self._h.cleat_call(
                self._service_name,
                "execute_node",
                {
                    "graph_name": self._graph_name,
                    "node_name": node_name,
                    "state": CleatSerializer.pack(state),
                },
            )
        except RuntimeError as e:
            raise RuntimeError(
                f"LangGraph node '{node_name}' execution failed: {e}"
            ) from e

        self._step_count += 1
        self._node_results.append(node_name)
        return CleatSerializer.unpack(result.get("state", {}))

    def route(self, state: dict) -> str:
        """Determine the next node to execute (routing only, no execution).

        Args:
            state: Current workflow state.

        Returns:
            Name of the next node, or ``"__end__"`` if the graph is done.
        """
        try:
            result = self._h.cleat_call(
                self._service_name,
                "route",
                {
                    "graph_name": self._graph_name,
                    "state": CleatSerializer.pack(state),
                },
            )
        except RuntimeError as e:
            raise RuntimeError(
                f"LangGraph routing failed for '{self._graph_name}': {e}"
            ) from e

        return result.get("next_node", "__end__")

    def invoke_full(self, state: dict) -> dict:
        """Execute the entire graph in a single durable call.

        Coarser granularity: the entire execution is one event in the log.
        On replay, the graph is re-executed from scratch. Use ``step()``
        for per-node durability.

        Args:
            state: Initial workflow state.

        Returns:
            Final state after full graph execution.
        """
        try:
            result = self._h.cleat_call(
                self._service_name,
                "invoke_graph",
                {
                    "graph_name": self._graph_name,
                    "state": CleatSerializer.pack(state),
                },
            )
        except RuntimeError as e:
            raise RuntimeError(
                f"LangGraph full invocation failed for '{self._graph_name}': {e}"
            ) from e

        return CleatSerializer.unpack(result.get("state", {}))

    def list_nodes(self) -> list[str]:
        """Return the list of node names in the registered graph.

        Useful for dynamic workflows that need to introspect the graph.
        """
        try:
            result = self._h.cleat_call(
                self._service_name,
                "list_nodes",
                {"graph_name": self._graph_name},
            )
        except RuntimeError as e:
            raise RuntimeError(
                f"Failed to list nodes for '{self._graph_name}': {e}"
            ) from e

        return result.get("nodes", [])

    def get_metadata(self) -> dict:
        """Return metadata about the registered graph."""
        try:
            result = self._h.cleat_call(
                self._service_name,
                "graph_info",
                {"graph_name": self._graph_name},
            )
        except RuntimeError as e:
            raise RuntimeError(
                f"Failed to get graph info for '{self._graph_name}': {e}"
            ) from e

        return result

    @staticmethod
    def is_done(state: dict) -> bool:
        """Check if the graph has reached its end state.

        Args:
            state: Current state dict (as returned by ``step()``).

        Returns:
            ``True`` if the graph is done.
        """
        return state.get("__node__") == "__end__" or state.get("done", False)

    @property
    def step_count(self) -> int:
        """Number of steps executed so far."""
        return self._step_count

    @property
    def executed_nodes(self) -> list[str]:
        """Names of nodes executed so far, in order."""
        return list(self._node_results)
