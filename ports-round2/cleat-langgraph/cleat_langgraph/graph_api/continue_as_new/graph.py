"""
Continue-as-new — LangGraph Graph API definition (host-side).

Demonstrates a multi-stage pipeline. In the Temporal version,
``continue-as-new`` terminates the current workflow execution and starts
a new one, with task result caching so previously-completed stages are
not re-executed.

In Cleat, there is no native ``continue-as-new`` equivalent. Instead, we
approximate it using Cleat's ``child_workflow`` pattern or by modelling
each pipeline stage as a separate workflow invocation.

Key design decision:
  The pipeline state is persisted in Cleat's durable state store
  (``h.set_state`` / ``h.get_state``). Each stage checks for cached
  results before executing. This avoids re-executing completed stages
  across workflow boundaries.

Graph topology::

    START → double → add_50 → triple → END
"""

from __future__ import annotations

from langgraph.graph import START, StateGraph
from typing_extensions import TypedDict


class PipelineState(TypedDict):
    """State for the pipeline graph.

    Attributes:
        value: The current numeric value being transformed.
    """
    value: int


def double(state: PipelineState) -> dict:
    """Stage 1: double the input."""
    return {"value": state["value"] * 2}


def add_50(state: PipelineState) -> dict:
    """Stage 2: add 50."""
    return {"value": state["value"] + 50}


def triple(state: PipelineState) -> dict:
    """Stage 3: triple the result."""
    return {"value": state["value"] * 3}


def make_pipeline_graph() -> StateGraph:
    """Build and return the pipeline StateGraph.

    Graph topology::

        START → double → add_50 → triple → END
    """
    g = StateGraph(PipelineState)
    g.add_node("double", double)
    g.add_node("add_50", add_50)
    g.add_node("triple", triple)
    g.add_edge(START, "double")
    g.add_edge("double", "add_50")
    g.add_edge("add_50", "triple")
    return g
