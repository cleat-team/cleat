"""
Hello World — Cleat durable workflow (WASM-side).

Demonstrates the Cleat-LangGraph pattern for a minimal single-node graph.

Key pattern:
  1. Create ``CleatLangGraph(h, "hello-world")`` using the HostCalls context.
  2. Call ``agent.step(state)`` for each node in the graph.
  3. Each ``step()`` is a ``cleat_call()`` recorded in Cleat's event log.

On replay, the ``cleat_call`` returns the cached result without
re-executing the node function.
"""

from __future__ import annotations

from cleat_sdk import HostCalls, cleat_entry
from cleat_langgraph import CleatLangGraph


@cleat_entry(name="hello_world")
def hello_world_workflow(h: HostCalls, query: str) -> str:
    """Process a query string through a single-node LangGraph.

    This Cleat workflow:
    1. Initializes state with the input query.
    2. Executes the graph's single node via a durable step.
    3. Returns the processed result.

    Args:
        h: HostCalls context (injected by the Cleat runtime).
        query: The input string to process.

    Returns:
        The processed result string (e.g. ``"Processed: Hello!"``).
    """
    # Initialize the LangGraph agent client
    agent = CleatLangGraph(h, "hello-world")

    # Initial state matching HelloState TypedDict
    state: dict = {
        "value": query,
        "__node__": "__start__",
    }

    # Execute the single node
    state = agent.step(state)

    # Return the result
    return state.get("value", "")
