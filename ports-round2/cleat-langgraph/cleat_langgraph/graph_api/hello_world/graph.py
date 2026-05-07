"""
Hello World — LangGraph Graph API definition (host-side).

The simplest possible LangGraph graph: a single node that processes input.

This file runs on the HOST side (outside the WASM sandbox) and defines
the LangGraph graph structure using the standard LangGraph API.
"""

from __future__ import annotations

from typing_extensions import TypedDict

from langgraph.graph import START, StateGraph


# ---------------------------------------------------------------------------
# State definition
# ---------------------------------------------------------------------------


class HelloState(TypedDict):
    """State for the hello world graph.

    A single field ``value`` that gets transformed through the graph.
    """
    value: str


# ---------------------------------------------------------------------------
# Node functions
# ---------------------------------------------------------------------------


def process_query(state: HelloState) -> dict:
    """Process a query and return a response.

    This is a synchronous node function (runs on the host side).
    For production, this could call an LLM, database, or external API.
    """
    return {"value": f"Processed: {state['value']}"}


# ---------------------------------------------------------------------------
# Graph builder
# ---------------------------------------------------------------------------


def make_hello_graph() -> StateGraph:
    """Build and return the hello world StateGraph.

    Graph topology::

        START → process_query → END

    Returns:
        An uncompiled ``StateGraph`` ready for registration with
        ``LangGraphRuntime.register_graph()``.
    """
    g = StateGraph(HelloState)
    g.add_node("process_query", process_query)
    g.add_edge(START, "process_query")
    return g
