"""
ReAct Agent — Functional API host-side definitions.

Defines LangGraph ``@task`` functions and the ``@entrypoint`` that
runs on the host side. The Cleat workflow calls each task via
``durable_call`` for per-task durability.
"""

from langgraph.func import entrypoint, task


@task
def agent_think(query: str, history: list[str]) -> dict:
    """The agent decides the next action based on query and tool history.

    In production, replace this with an LLM call.
    """
    tool_results = [h for h in history if h.startswith("[Tool]")]

    if len(tool_results) == 0:
        return {
            "action": "tool",
            "tool_name": "get_weather",
            "tool_input": "San Francisco",
        }
    elif len(tool_results) == 1:
        return {
            "action": "tool",
            "tool_name": "get_population",
            "tool_input": "San Francisco",
        }
    else:
        facts = "; ".join(tool_results)
        return {
            "action": "final",
            "answer": f"Here's what I found about San Francisco: {facts}",
        }


@task
def execute_tool(tool_name: str, tool_input: str) -> str:
    """Execute a tool by name."""
    tool_registry = {
        "get_weather": lambda inp: f"[Tool] Weather in {inp}: 72°F and sunny.",
        "get_population": lambda inp: f"[Tool] {inp} population: ~870,000 residents.",
    }
    handler = tool_registry.get(tool_name)
    if handler:
        return handler(tool_input)
    return f"[Tool] Unknown tool: {tool_name}"


@entrypoint()
async def react_agent_entrypoint(query: str) -> dict:
    """ReAct agent loop: think → act → observe → repeat.

    This is the reference implementation. In the Cleat version, this
    entrypoint runs on the host side when invoked via
    ``durable_call("langgraph", "invoke_entrypoint", ...)``.
    """
    history: list[str] = []

    while True:
        decision = await agent_think(query, history)

        if decision["action"] == "final":
            return {"answer": decision["answer"], "steps": len(history)}

        result = await execute_tool(decision["tool_name"], decision["tool_input"])
        history.append(result)


# List of all tasks for registration
all_tasks = [agent_think, execute_tool]
