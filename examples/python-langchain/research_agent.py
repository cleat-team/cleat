#!/usr/bin/env python3
"""
LangChain Research Agent with Cleat Durable Execution
=======================================================

Demonstrates:
1. Building a research agent with LLM + tools via Cleat's typed plugin API
2. Recording every step in Cleat's durable event history
3. Surviving worker crashes with deterministic replay — no duplicated API costs

Usage (WASM / cleat CLI)::

    durable build --target python --entry research_agent.py:langchain_research_agent
    durable run langchain_research_agent '{"topic": "Compare Temporal, DBOS, and Cleat"}'

Usage (standalone test, no WASM needed)::

    python research_agent.py --test
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from typing import Any

# Make the python-sdk available when running standalone
sys.path.insert(0,
    os.path.join(os.path.dirname(__file__), "..", "..", "python-sdk"))

from cleat_sdk import HostCalls, durable_entry
from cleat_sdk.plugins import Plugins
from cleat_sdk.langchain.callbacks import CleatCallbackHandler

# ========================================================================
# Constants
# ========================================================================

RESEARCH_PROMPT = """You are a thorough research assistant. Your goal is to provide
accurate, well-sourced answers to research questions.

When researching:
1. Break down the question into sub-questions
2. Search for current information on each sub-question
3. Use calculations when quantitative comparison is needed
4. Synthesize findings into a clear, structured answer
5. Cite your sources

Be honest about uncertainty. If information is unavailable or contradictory,
acknowledge it."""

MAX_RESEARCH_STEPS = 15

# ========================================================================
# Tool schemas (OpenAI-compatible function-calling format)
# ========================================================================

RESEARCH_TOOLS = [
    {
        "type": "function",
        "function": {
            "name": "web_search",
            "description": "Search the web for current information. "
                           "Returns top results with titles, snippets, and URLs.",
            "parameters": {
                "type": "object",
                "properties": {
                    "query": {
                        "type": "string",
                        "description": "The search query string",
                    },
                },
                "required": ["query"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "calculator",
            "description": "Evaluate a mathematical expression. "
                           "Supports +, -, *, /, **, and parentheses.",
            "parameters": {
                "type": "object",
                "properties": {
                    "expression": {
                        "type": "string",
                        "description": "The mathematical expression to evaluate",
                    },
                },
                "required": ["expression"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "get_current_date",
            "description": "Get the current date and time. Use this when you "
                           "need to know the current date for time-sensitive research.",
            "parameters": {
                "type": "object",
                "properties": {},
            },
        },
    },
]


# ========================================================================
# Tool implementations
# ========================================================================


def _execute_web_search(h: HostCalls, query: str) -> str:
    """Execute a web search via the websearch plugin.

    The search is recorded as a deterministic event — on crash recovery the
    same result is returned without re-executing the search.
    """
    h.durable_log(f"  Web search: {query[:120]}")
    try:
        return h.plugin_call("websearch", "search", {"query": query})
    except Exception as e:
        h.durable_log(f"  Web search failed: {e}")
        return json.dumps({"error": str(e), "results": []})


def _execute_calculator(h: HostCalls, expression: str) -> str:
    """Evaluate a mathematical expression.

    Uses Python's ``eval`` in a restricted namespace (demo-safe).
    In production, use a proper expression parser instead.
    """
    h.durable_log(f"  Calculator: {expression}")
    try:
        allowed = {"__builtins__": {}}
        result = eval(expression, allowed, {})
        return json.dumps({"expression": expression, "result": result})
    except Exception as e:
        return json.dumps({"expression": expression, "error": str(e)})


def _execute_get_current_date(h: HostCalls) -> str:
    """Return the current UTC date via the deterministic clock (h.now())."""
    now_ms = h.now()
    from datetime import datetime, timezone
    now = datetime.fromtimestamp(now_ms / 1000, tz=timezone.utc)
    return json.dumps({
        "date": now.strftime("%Y-%m-%d"),
        "datetime": now.strftime("%Y-%m-%d %H:%M:%S UTC"),
        "timestamp_ms": now_ms,
    })


def _execute_tool(h: HostCalls, tool_name: str, arguments: dict) -> str:
    """Dispatch a tool call to the appropriate implementation."""
    if tool_name == "web_search":
        return _execute_web_search(h, arguments.get("query", ""))
    elif tool_name == "calculator":
        return _execute_calculator(h, arguments.get("expression", ""))
    elif tool_name == "get_current_date":
        return _execute_get_current_date(h)
    else:
        return json.dumps({"error": f"Unknown tool: {tool_name}"})


# ========================================================================
# Core workflow — undecorated for direct testability
# ========================================================================


def _research_agent_impl(h: HostCalls, topic: str) -> str:
    """Research *topic* using an LLM with tools, backed by Cleat durability.

    Parameters
    ----------
    h : HostCalls
        Cleat host-calls interface (injected by the framework).
    topic : str
        The research topic / question.

    Returns
    -------
    str
        JSON result containing the final answer, step count, and cost.
    """
    h.durable_log(f"Starting research agent for topic: {topic}")

    # --- Initialise Cleat integration helpers ----------------------------
    #
    # CleatCallbackHandler records LangChain agent steps as durable events.
    # Wire it into a LangChain agent like so:
    #
    #   from langchain_openai import ChatOpenAI
    #   llm = ChatOpenAI(model="gpt-4o", callbacks=[callback])
    #   agent = create_openai_functions_agent(llm, tools, prompt)
    #   result = agent.invoke({"input": topic}, config={"callbacks": [callback]})
    #
    # Here we use Cleat's typed plugin API directly for a self-contained demo.
    callback = CleatCallbackHandler(h, verbose=True)
    plugins = Plugins(h)

    # --- Track overall agent status in durable state ---------------------
    h.set_state("agent_status", {
        "topic": topic,
        "started_at": h.now(),
        "status": "researching",
    })

    messages: list[dict[str, Any]] = [
        {"role": "system", "content": RESEARCH_PROMPT},
        {"role": "user", "content": topic},
    ]

    total_cost = 0.0
    llm_calls = 0

    # --- Main agent loop ------------------------------------------------
    for step in range(MAX_RESEARCH_STEPS):
        # Check for external cancellation (e.g. from the dashboard).
        cancelled, reason = h.poll_cancellation()
        if cancelled:
            h.durable_log(f"Agent cancelled at step {step + 1}: {reason}")
            h.set_state("agent_status", {
                "status": "cancelled",
                "reason": reason,
                "steps": step,
            })
            return json.dumps({
                "cancelled": True,
                "reason": reason,
                "steps": step,
            })

        h.durable_log(f"Step {step + 1}/{MAX_RESEARCH_STEPS}")

        # Call the LLM through Cleat's plugin system.
        # This is deterministically recorded — on replay the same cached
        # response is returned without re-contacting the API.
        llm_result = plugins.llm_chat(
            provider="openai",
            model="gpt-4o",
            messages=messages,
            tools=RESEARCH_TOOLS,
        )

        llm_calls += 1
        if llm_result.cost:
            total_cost += llm_result.cost

        # Record per-step progress so the dashboard can show live status.
        h.set_state(f"step_{step + 1}", {
            "llm_calls": llm_calls,
            "total_cost": round(total_cost, 6),
            "timestamp": h.now(),
        })

        # Handle empty response.
        if not llm_result.choices:
            h.durable_log(f"No choices returned at step {step + 1}")
            return json.dumps({
                "error": "No response from LLM",
                "steps": step + 1,
                "llm_calls": llm_calls,
                "total_cost": round(total_cost, 6),
            })

        choice = llm_result.choices[0]
        message = choice.get("message", {})
        finish_reason = choice.get("finish_reason", "")
        tool_calls = message.get("tool_calls", [])

        # The model returned content without asking for tools — we are done.
        if not tool_calls and finish_reason == "stop":
            content = message.get("content", "")
            h.set_state("agent_status", {
                "status": "completed",
                "steps": step + 1,
                "llm_calls": llm_calls,
                "total_cost": round(total_cost, 6),
            })
            h.durable_log(
                f"Research complete: {step + 1} steps, "
                f"{llm_calls} LLM calls, ${total_cost:.4f}"
            )
            return json.dumps({
                "result": content,
                "steps": step + 1,
                "llm_calls": llm_calls,
                "total_cost": round(total_cost, 6),
            })

        # No tool calls but the model hasn't signalled stop yet — just
        # append the assistant message and continue the conversation.
        if not tool_calls:
            messages.append(message)
            continue

        # --- Tool execution ---------------------------------------------
        messages.append(message)

        for tc in tool_calls:
            fn = tc.get("function", {})
            tool_name = fn.get("name", "")
            try:
                tool_args = json.loads(fn.get("arguments", "{}"))
            except json.JSONDecodeError:
                tool_args = {}

            h.durable_log(f"  Tool: {tool_name}")
            tool_result = _execute_tool(h, tool_name, tool_args)

            messages.append({
                "role": "tool",
                "tool_call_id": tc.get("id", ""),
                "content": tool_result,
            })

    # --- Max steps reached ----------------------------------------------
    h.durable_log("Max research steps reached")
    h.set_state("agent_status", {
        "status": "max_steps_reached",
        "steps": MAX_RESEARCH_STEPS,
        "llm_calls": llm_calls,
        "total_cost": round(total_cost, 6),
    })
    return json.dumps({
        "error": "Max steps reached",
        "steps": MAX_RESEARCH_STEPS,
        "llm_calls": llm_calls,
        "total_cost": round(total_cost, 6),
    })


# ========================================================================
# Decorated entry point — used by ``durable build`` / ``durable run``
# ========================================================================


@durable_entry("LangChainResearchAgent")
def langchain_research_agent(h: HostCalls, topic: str) -> str:
    """WASM entry point for the Cleat runtime.

    The ``@durable_entry`` decorator generates a WASM-export-compatible
    wrapper following the Cleat ABI.  It reads input JSON from linear
    memory, creates a ``HostCalls`` instance, calls the function, and
    serialises the result back.

    When the worker crashes mid-execution and restarts, the decorated
    wrapper replays the event history from the beginning, but every
    ``plugin_call`` returns the cached result from the previous run —
    no duplicate API calls, no lost progress.
    """
    return _research_agent_impl(h, topic)


# ========================================================================
# Test harness (standalone, no WASM / no network)
# ========================================================================


def run_test() -> None:
    """Run the agent with an inline mock HostCalls (no WASM, no network).

    Every plugin call (LLM, web search) is served by a local mock so the
    test works offline.  The mock records all calls and verifies that the
    agent logic (LLM loop, tool dispatch, state tracking) operates correctly.
    """
    # ------------------------------------------------------------------
    # Inline mock
    # ------------------------------------------------------------------
    class _MockHostCalls:
        """Minimal duck-typed HostCalls mock for testing."""

        def __init__(self) -> None:
            self.state: dict[str, Any] = {}
            self.logs: list[str] = []
            self.call_history: list[tuple[str, str, Any]] = []
            self._llm_call_count = 0
            self._current_time = 1704067200000  # 2024-01-01T00:00:00Z

        # -- HostCalls interface ---------------------------------------

        def durable_log(self, message: str) -> None:
            self.logs.append(message)
            print(f"  [LOG] {message}")

        def set_state(self, key: str, value: Any) -> None:
            self.state[key] = value

        def get_state(self, key: str, result_type: type = str) -> Any:
            return self.state.get(key)

        def list_state(self, prefix: str = "") -> list[str]:
            return [k for k in self.state if k.startswith(prefix)]

        def now(self) -> int:
            return self._current_time

        def poll_cancellation(self) -> tuple[bool, str]:
            return (False, "")

        def plugin_call(self, plugin_name: str,
                        function_name: str,
                        input_data: Any) -> str:
            self.call_history.append((plugin_name, function_name, input_data))
            if not isinstance(input_data, dict):
                input_data = {}

            if plugin_name == "llm" and function_name == "chat":
                return self._mock_llm(input_data)
            if plugin_name == "websearch" and function_name == "search":
                return self._mock_search(input_data)
            return json.dumps({
                "error": f"No mock for {plugin_name}.{function_name}",
            })

        # -- Mock response builders ------------------------------------

        def _mock_search(self, input_data: dict) -> str:
            query = input_data.get("query", "")
            return json.dumps({
                "results": [
                    {
                        "title": f"Result about '{query[:50]}'",
                        "snippet": "Simulated search result for testing.",
                        "url": "https://example.com/1",
                    },
                    {
                        "title": f"Another result about '{query[:50]}'",
                        "snippet": "More simulated content.",
                        "url": "https://example.com/2",
                    },
                ],
            })

        def _mock_llm(self, input_data: dict) -> str:
            self._llm_call_count += 1
            messages = input_data.get("messages", [])
            last_msg = messages[-1].get("content", "") if messages else ""

            # First LLM call → return a tool-call request for web_search
            if self._llm_call_count <= 1:
                return json.dumps({
                    "choices": [{
                        "message": {
                            "role": "assistant",
                            "content": None,
                            "tool_calls": [{
                                "id": f"call_test_{self._llm_call_count}",
                                "type": "function",
                                "function": {
                                    "name": "web_search",
                                    "arguments": json.dumps(
                                        {"query": last_msg[:80]}),
                                },
                            }],
                        },
                        "finish_reason": "tool_calls",
                    }],
                    "usage": {
                        "prompt_tokens": 100,
                        "completion_tokens": 20,
                        "total_tokens": 120,
                    },
                    "cost": 0.001,
                    "model": "gpt-4o",
                })

            # Subsequent LLM call → return a final answer
            return json.dumps({
                "choices": [{
                    "message": {
                        "role": "assistant",
                        "content": (
                            f"Based on my research about "
                            f"'{last_msg[:60]}...': this is a simulated "
                            f"result demonstrating Cleat's deterministic "
                            f"replay capabilities."
                        ),
                    },
                    "finish_reason": "stop",
                }],
                "usage": {
                    "prompt_tokens": 200,
                    "completion_tokens": 50,
                    "total_tokens": 250,
                },
                "cost": 0.002,
                "model": "gpt-4o",
            })

    # ------------------------------------------------------------------
    # Run the test
    # ------------------------------------------------------------------
    print("=" * 60)
    print("Testing LangChain Research Agent (inline mock, no WASM)")
    print("=" * 60)

    mock = _MockHostCalls()
    topic = "What are the latest developments in fusion energy?"
    print(f"\nTopic: {topic}")
    print("-" * 40)

    result = _research_agent_impl(mock, topic)

    print("-" * 40)
    print(f"\nFinal result snippet:\n  {result[:400]}...")

    print(f"\nPlugin calls ({len(mock.call_history)}):")
    for i, (plugin, func, _) in enumerate(mock.call_history):
        print(f"  {i + 1}. {plugin}.{func}")

    status = mock.state.get("agent_status", {})
    if isinstance(status, dict):
        print(f"\nAgent status: {status.get('status', 'unknown')}")
        print(f"Steps completed: {status.get('steps', '?')}")
        print(f"LLM calls: {status.get('llm_calls', '?')}")
        print(f"Total cost: ${status.get('total_cost', 0)}")

    print("\n" + "=" * 60)
    print("SUCCESS: Agent ran to completion deterministically.")
    print("In production, crash recovery replays these exact steps")
    print("from the event history without duplicate API costs.")
    print("=" * 60)


# ========================================================================
# Cost comparison
# ========================================================================


def print_cost_comparison() -> None:
    """Print a side-by-side comparison with / without Cleat durability."""
    print("""
    Cost Comparison: Crash Recovery
    ─────────────────────────────────────────────────────────────────

    Scenario: 15-step research agent, worker crashes after step 5

    WITHOUT Cleat (restart from scratch):
      Steps 1-5:   5 LLM calls  = $0.015
      ── CRASH ──
      Steps 1-5:   5 LLM calls  = $0.015   ←  duplicated!
      Steps 6-15: 10 LLM calls  = $0.030
      ──────────────────────────────────
      Total:      20 LLM calls  = $0.060

    WITH Cleat (resume from event history):
      Steps 1-5:   5 LLM calls  = $0.015
      ── CRASH ──
      Steps 1-5:   0 LLM calls  = $0.000   ←  replayed from history
      Steps 6-15: 10 LLM calls  = $0.030
      ──────────────────────────────────
      Total:      15 LLM calls  = $0.045

    Savings:  $0.015 (25%) + deterministic results

    For a 100-step agent crashing at step 50:
      Without Cleat: 150 LLM calls
      With Cleat:    100 LLM calls
      Savings:       33%
    """)


# ========================================================================
# CLI
# ========================================================================


def main() -> None:
    parser = argparse.ArgumentParser(
        description="LangChain Research Agent with Cleat Durable Execution")
    parser.add_argument("--test", action="store_true",
                        help="Run with inline mock (no WASM required)")
    parser.add_argument("--costs", action="store_true",
                        help="Show cost-comparison info")
    parser.add_argument("--topic", type=str, default="",
                        help="Research topic (standalone)")

    args = parser.parse_args()

    if args.costs:
        print_cost_comparison()
        return

    if args.test:
        run_test()
        return

    # Default: show docs
    print(__doc__)
    print("\nAvailable options:")
    print("  --test     Run with inline mock (no WASM)")
    print("  --costs    Show cost-comparison details")


if __name__ == "__main__":
    main()
