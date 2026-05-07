"""Research Agent — a cleat AI agent powered by Cleat and LangChain.

This agent researches a topic using an LLM with web search and calculator
tools. Every step is recorded in cleat's event history, so the agent
survives crashes and resumes deterministically without losing progress
or incurring duplicate API costs.

Usage:
    cleat build --target python --entry agent.py:research_agent
    cleat run research_agent '{"topic": "Compare Temporal, DBOS, and Cleat"}'
"""

from cleat_sdk import HostCalls, cleat_entry
from cleat_sdk.plugins import Plugins

SYSTEM_PROMPT = """You are a helpful research assistant. Use tools when you need
to look up current information or perform calculations. Be thorough and cite sources.

Available tools:
- web_search: Search the web for current information
- calculator: Perform mathematical calculations
"""

MAX_STEPS = 10


@cleat_entry("ResearchAgent")
def research_agent(h: HostCalls, topic: str) -> str:
    """Research a topic using an LLM with tools.

    Parameters
    ----------
    h : HostCalls
        The Cleat host calls interface (injected automatically).
    topic : str
        The research topic to investigate.

    Returns
    -------
    str
        The final research result.
    """
    plugins = Plugins(h)

    messages = [
        {"role": "system", "content": SYSTEM_PROMPT},
        {"role": "user", "content": topic},
    ]

    tools = [
        {
            "type": "function",
            "function": {
                "name": "web_search",
                "description": "Search the web for current information about a topic",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "query": {
                            "type": "string",
                            "description": "The search query",
                        }
                    },
                    "required": ["query"],
                },
            },
        },
        {
            "type": "function",
            "function": {
                "name": "calculator",
                "description": "Perform a mathematical calculation",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "expression": {
                            "type": "string",
                            "description": "The mathematical expression to evaluate",
                        }
                    },
                    "required": ["expression"],
                },
            },
        },
    ]

    for step in range(MAX_STEPS):
        h.cleat_log(f"ResearchAgent step {step + 1}/{MAX_STEPS}")

        result = plugins.llm_chat(
            provider="openai",
            model="gpt-4o",
            messages=messages,
            tools=tools,
        )

        # Check if the model returned a final answer (no tool calls)
        if not result.choices:
            h.cleat_log(f"ResearchAgent finished at step {step + 1}")
            return "No response from LLM"

        choice = result.choices[0]
        message = choice.get("message", {})
        tool_calls = message.get("tool_calls", [])

        if not tool_calls:
            content = message.get("content", "")
            h.set_state("research_result", {"topic": topic, "result": content, "steps": step + 1})
            h.cleat_log(f"ResearchAgent returned final answer at step {step + 1}")
            return content

        # Add assistant message to conversation
        messages.append(message)

        # Execute each tool call
        for tc in tool_calls:
            fn = tc.get("function", {})
            tool_name = fn.get("name", "")
            tool_args_str = fn.get("arguments", "{}")

            import json
            try:
                tool_args = json.loads(tool_args_str)
            except json.JSONDecodeError:
                tool_args = {}

            # Execute the tool via cleat's cleat_call
            if tool_name == "web_search":
                query = tool_args.get("query", "")
                tool_result = execute_web_search(h, query)
            elif tool_name == "calculator":
                expression = tool_args.get("expression", "")
                tool_result = execute_calculator(h, expression)
            else:
                tool_result = f"Unknown tool: {tool_name}"

            messages.append({
                "role": "tool",
                "tool_call_id": tc.get("id", ""),
                "content": tool_result,
            })

    h.cleat_log("ResearchAgent reached max steps")
    return "Max steps reached without final answer"


def execute_web_search(h: HostCalls, query: str) -> str:
    """Execute a web search via cleat's cleat_call.

    The search result is recorded in event history for deterministic replay.
    """
    try:
        result = h.cleat_call("websearch", "search", {"query": query})
        return result
    except Exception as e:
        h.cleat_log(f"Web search failed: {e}")
        return f"Search error: {e}"


def execute_calculator(h: HostCalls, expression: str) -> str:
    """Execute a calculation via cleat's cleat_call.

    The calculation result is recorded in event history.
    """
    try:
        result = h.cleat_call("calculator", "eval", {"expression": expression})
        return result
    except Exception as e:
        h.cleat_log(f"Calculator failed: {e}")
        return f"Calculation error: {e}"
