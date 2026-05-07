"""
Human-in-the-loop chatbot — LangGraph Graph API definition (host-side).

Demonstrates using LangGraph's ``interrupt()`` to pause a workflow for human
input. In the Temporal version, ``interrupt()`` pauses execution and the
Temporal signal mechanism delivers human feedback. In the Cleat version,
``await_signals()`` replaces the interrupt pattern.

Key design decision:
  LangGraph's ``interrupt()`` is designed for Python-async runtimes.
  Since Cleat workflows are synchronous and run in WASM, we use Cleat's
  native ``await_signals(timeout_ms)`` and ``poll_signal(name)`` APIs
  instead, with manual workflow-level state management.

Graph topology::

    START → generate_draft → human_review → END

The ``human_review`` node pauses via Cleat signal (not LangGraph interrupt).
"""

from __future__ import annotations

from langgraph.graph import START, StateGraph
from typing_extensions import TypedDict


# ---------------------------------------------------------------------------
# State definition
# ---------------------------------------------------------------------------


class ChatState(TypedDict):
    """State for the chatbot graph.

    Attributes:
        value: The current message or draft response.
        feedback: Human feedback received via signal (None until received).
        approved: Whether human approved the draft.
    """
    value: str
    feedback: str | None
    approved: bool


# ---------------------------------------------------------------------------
# Node functions
# ---------------------------------------------------------------------------


def generate_draft(state: ChatState) -> dict:
    """Generate a draft response.

    In production, replace this with an LLM call.

    Args:
        state: Current state with the user's input in ``value``.

    Returns:
        Partial state update with the draft response.
    """
    draft = (
        f"Here's my response to '{state['value']}': "
        "The answer is 42. Let me know if this helps!"
    )
    return {"value": draft}


def human_review(state: ChatState) -> dict:
    """Process human feedback.

    This node does NOT use LangGraph's ``interrupt()``. Instead, the
    Cleat workflow handles the pause-and-resume via ``await_signals()``
    or ``poll_signal()`` before calling this node.

    Args:
        state: Current state with ``feedback`` from the human.

    Returns:
        Partial state update: if approved, keeps the draft; otherwise
        incorporates feedback into a revised response.
    """
    feedback = state.get("feedback")
    approved = state.get("approved", False)

    if approved or feedback == "approve":
        return {"value": state["value"]}

    return {
        "value": (
            f"[Revised] {state['value']} "
            f"(incorporating feedback: {feedback})"
        )
    }


# ---------------------------------------------------------------------------
# Graph builder
# ---------------------------------------------------------------------------


def make_chatbot_graph() -> StateGraph:
    """Build and return the chatbot StateGraph.

    Graph topology::

        START → generate_draft → human_review → END

    The Cleat workflow pauses between ``generate_draft`` and
    ``human_review`` to wait for human input via signals.

    Returns:
        An uncompiled ``StateGraph``.
    """
    g = StateGraph(ChatState)
    g.add_node("generate_draft", generate_draft)
    g.add_node("human_review", human_review)
    g.add_edge(START, "generate_draft")
    g.add_edge("generate_draft", "human_review")
    return g
