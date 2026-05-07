"""
ReAct Agent — Cleat durable workflow (WASM-side).

Demonstrates the Cleat-LangGraph pattern for a tool-calling agent loop.

The workflow calls ``agent.step(state)`` which combines routing + node
execution into a single durable_call. Each step records one node execution
in Cleat's event log, enabling replay at per-node granularity.

Key difference from the Temporal version:
  - Temporal: ``temporal_graph("react-agent").compile().ainvoke(initial_state)``
    — the entire graph executes inside the Temporal workflow, with each node
    as a Temporal activity.
  - Cleat: The workflow drives the execution loop explicitly, calling
    ``step()`` for each node. This is because LangGraph cannot run inside
    the WASM sandbox — graph execution must happen on the host side.
"""

from __future__ import annotations

from cleat_sdk import HostCalls, durable_entry
from cleat_langgraph import CleatLangGraph


@durable_entry(name="react_agent")
def react_agent_workflow(h: HostCalls, query: str) -> str:
    """Run a tool-calling ReAct agent with per-node durability.

    Workflow loop::

        state = {input: query}
        while not done:
            state = agent.step(state)   # ← durable_call per node

    Each call to ``agent.step()`` is recorded in Cleat's event log.
    On replay, the cached result is returned without re-executing the node.

    Args:
        h: HostCalls context (injected by the Cleat runtime).
        query: The user's input query for the agent.

    Returns:
        The agent's final answer string.
    """
    # Initialize the LangGraph agent client
    agent = CleatLangGraph(h, "react-agent")

    # Initial state matching AgentState TypedDict
    # Note: messages is a list (not annotated with operator.add for transport)
    state: dict = {
        "input": query,
        "messages": [],
        "final_answer": "",
        "__node__": "__start__",
    }

    # Workflow loop: step through the graph until done
    max_steps = 10  # safety limit to prevent infinite loops
    step_count = 0

    while not CleatLangGraph.is_done(state) and step_count < max_steps:
        h.durable_log(f"React agent: step {step_count + 1}")
        state = agent.step(state)
        step_count += 1

    if step_count >= max_steps:
        h.durable_log("React agent: reached max steps without final answer")
        return "Max steps reached. Agent did not produce a final answer."

    h.durable_log(f"React agent: completed in {step_count} steps")
    return state.get("final_answer", "No final answer produced.")
