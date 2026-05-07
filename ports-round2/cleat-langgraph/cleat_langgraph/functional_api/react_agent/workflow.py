"""
ReAct Agent — Cleat durable workflow (WASM-side).

The Functional API ReAct loop ported to Cleat. Each task execution
is a separate ``cleat_call``, giving per-task durability.

Two granularity levels are demonstrated:
1. ``invoke_entrypoint`` — full agent loop as a single durable event
   (coarser, simpler, fewer events in the log).
2. ``execute_task`` — per-task durable events (finer granularity,
   better replay efficiency for long-running agents).

The per-task approach is recommended for production agents that make
multiple LLM calls, so each LLM call is individually recorded.
"""

from __future__ import annotations

from cleat_sdk import HostCalls, cleat_entry
from cleat_langgraph import CleatLangGraph


@cleat_entry(name="react_agent_functional")
def react_agent_functional_workflow(h: HostCalls, query: str) -> dict:
    """Run a ReAct agent using per-task durable calls.

    Workflow:
    1. Call ``agent_think`` task via cleat_call.
    2. If agent wants a tool, call ``execute_tool`` via cleat_call.
    3. Repeat until agent produces a final answer.
    4. Return the result.

    Each call to ``agent_think`` or ``execute_tool`` is a separate
    durable event. On replay, cached results are returned.

    Args:
        h: HostCalls context.
        query: The user's input query.

    Returns:
        dict with ``answer`` and ``steps``.
    """
    history: list[str] = []
    max_iterations = 10

    for iteration in range(max_iterations):
        h.cleat_log(f"Functional agent: iteration {iteration + 1}")

        # Call agent_think task (durable)
        decision = h.cleat_call(
            "langgraph",
            "execute_task",
            {
                "task_name": "agent_think",
                "input": query,
                "history": history,
            },
        )

        if decision.get("action") == "final":
            result = {
                "answer": decision.get("answer", ""),
                "steps": len(history),
            }
            h.cleat_log(f"Functional agent complete: {result}")
            return result

        # Call execute_tool task (durable)
        tool_name = decision.get("tool_name", "")
        tool_input = decision.get("tool_input", "")
        tool_result = h.cleat_call(
            "langgraph",
            "execute_task",
            {
                "task_name": "execute_tool",
                "tool_name": tool_name,
                "tool_input": tool_input,
            },
        )

        history.append(tool_result)

    return {"answer": "Max iterations reached", "steps": len(history)}


@cleat_entry(name="react_agent_functional_simple")
def react_agent_simple_workflow(h: HostCalls, query: str) -> dict:
    """Simpler version: run the full entrypoint as a single durable call.

    The entire agent loop executes on the host side as one event.
    Simpler but coarser granularity.

    Args:
        h: HostCalls context.
        query: The user's input query.

    Returns:
        dict with ``answer`` and ``steps``.
    """
    h.cleat_log("Running full agent entrypoint (single durable call)")

    result = h.cleat_call(
        "langgraph",
        "invoke_entrypoint",
        {
            "entrypoint_name": "react-agent",
            "input": query,
        },
    )

    return result
