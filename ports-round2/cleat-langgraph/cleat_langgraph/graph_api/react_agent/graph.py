"""
ReAct Agent — LangGraph Graph API definition (host-side).

Demonstrates the most common LangGraph pattern: a tool-calling agent that
loops between "thinking" (deciding the next action) and "acting" (executing
a tool), using conditional edges to control the loop.

Graph topology::

    START → agent → (tools → agent)* → END

This file runs on the HOST side. The Cleat workflow (workflow.py) calls
into this graph via the bridge layer, with per-node durability.
"""

from __future__ import annotations

import operator
from typing import Annotated, Any, TypedDict

from langgraph.graph import END, START, StateGraph


# ---------------------------------------------------------------------------
# State definition
# ---------------------------------------------------------------------------

# Using operator.add so each node appends to the messages list rather
# than replacing it, accumulating the full conversation history.
_AnnotatedMessages = Annotated[list[str], operator.add]


class AgentState(TypedDict):
    """State for the ReAct agent.

    Attributes:
        input: The original user query.
        messages: Accumulated conversation history (appended by each node).
        final_answer: The agent's final answer after tool execution.
    """
    input: str
    messages: _AnnotatedMessages  # type: ignore[valid-type]
    final_answer: str


# ---------------------------------------------------------------------------
# Node functions
# ---------------------------------------------------------------------------


def agent(state: AgentState) -> dict[str, Any]:
    """The agent decides what to do next based on the conversation history.

    In production, replace this with an LLM call (e.g., Claude with tools).
    This stub simulates a 2-step research process.

    Args:
        state: Current agent state with messages history.

    Returns:
        Partial state update with new messages and optionally final_answer.
    """
    messages = state.get("messages", [])
    tool_results = [m for m in messages if m.startswith("[Tool]")]

    if len(tool_results) == 0:
        return {
            "messages": [
                "[Agent] I need weather data. Calling get_weather for San Francisco."
            ]
        }
    elif len(tool_results) == 1:
        return {
            "messages": [
                "[Agent] Now I need population data. "
                "Calling get_population for San Francisco."
            ]
        }
    else:
        facts = "; ".join(tool_results)
        return {
            "messages": ["[Agent] I have all the information I need."],
            "final_answer": f"Here's what I found about San Francisco: {facts}",
        }


def tools(state: AgentState) -> dict[str, Any]:
    """Execute the tool requested by the agent.

    In production, dispatch to real tool implementations. This stub
    simulates weather and population tools.

    Args:
        state: Current agent state containing the agent's latest message.

    Returns:
        Partial state update with tool result message.
    """
    last_msg = state["messages"][-1] if state.get("messages") else ""

    if "get_weather" in last_msg:
        return {"messages": ["[Tool] Weather in San Francisco: 72°F and sunny."]}
    elif "get_population" in last_msg:
        return {
            "messages": ["[Tool] San Francisco population: ~870,000 residents."]
        }
    else:
        return {"messages": ["[Tool] Unknown tool requested."]}


def should_continue(state: AgentState) -> str:
    """Route: if the agent requested a tool, go to 'tools'. Otherwise, end.

    This is a conditional edge function that LangGraph calls to determine
    the next node. In the Cleat bridge, this is called via the ``route``
    operation to determine which node to execute next.

    Args:
        state: Current agent state.

    Returns:
        ``"tools"`` if the agent wants to call a tool, ``"__end__"`` otherwise.
    """
    last_msg = state["messages"][-1] if state.get("messages") else ""
    if last_msg.startswith("[Agent]") and "Calling" in last_msg:
        return "tools"
    return END


# ---------------------------------------------------------------------------
# Graph builder
# ---------------------------------------------------------------------------


def make_agent_graph() -> StateGraph:
    """Build and return the ReAct agent StateGraph.

    Graph topology::

        START → agent
        agent → tools (if agent called a tool)
        agent → END  (if agent has final answer)
        tools → agent

    Returns:
        An uncompiled ``StateGraph`` ready for registration with
        ``LangGraphRuntime.register_graph()``.
    """
    g = StateGraph(AgentState)
    g.add_node("agent", agent)
    g.add_node("tools", tools)
    g.add_edge(START, "agent")
    g.add_conditional_edges("agent", should_continue)
    g.add_edge("tools", "agent")
    return g
