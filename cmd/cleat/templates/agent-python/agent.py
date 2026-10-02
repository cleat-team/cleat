"""Research Agent -- a Python workflow that runs cleat's shipped agent.

THE LOOP IS NOT IN THIS FILE, and that is the change cleat#1983 made. A library
that runs inside the guest has to be rewritten in every language, so this file
and its Go sibling each carried a hand-written copy of the same ReAct loop, and
neither was tested. The loop now ships as a workflow (`cleat/agentworkflow`),
and what a Python workflow does instead is declare its tools and call
`run_agent`.

EVERY LLM TURN AND EVERY TOOL CALL INSIDE THE AGENT IS A DURABLE STEP, so this
agent survives a crash mid-conversation and resumes without asking the model
again for turns it already completed, and without repeating a tool call whose
effect already happened. The hand-written loop claimed the same property in
prose; here it is the engine's, and it comes to every SDK at once.

Requires the agent workflow deployed under the name ``agent``:

    cleat deploy --name agent ./out/agent.wasm

Usage:
    cleat build --target python --entry agent.py:research_agent
    cleat run --wasm research_agent.wasm --entry-point research_agent --input '{"topic": "Compare Temporal, DBOS, and Cleat"}'
"""

from cleat_sdk import HostCalls, cleat_entry
from cleat_sdk.agent import AgentConfig, Tool, run_agent

SYSTEM_PROMPT = """You are a helpful research assistant. Use tools when you need
to look up current information or perform calculations. Be thorough and cite sources.

Available tools:
- web_search: Search the web for current information
- calculator: Perform mathematical calculations
"""

# TOOLS ARE DATA. Each entry says what the model sees (name, description,
# parameters) and how the call is dispatched (kind, plus the target). A service
# tool resolves at the worker via --service-endpoints, the same way the
# hand-written executors in this file used to.
TOOLS = [
    Tool(
        name="web_search",
        kind="service",
        service="websearch",
        operation="search",
        description="Search the web for current information",
        parameters={
            "type": "object",
            "properties": {"query": {"type": "string"}},
            "required": ["query"],
        },
    ),
    Tool(
        name="calculator",
        kind="service",
        service="calculator",
        operation="eval",
        description="Evaluate a mathematical expression",
        parameters={
            "type": "object",
            "properties": {"expression": {"type": "string"}},
            "required": ["expression"],
        },
    ),
]


@cleat_entry("research_agent")
def research_agent(h: HostCalls, topic: str) -> str:
    """Research a topic, through the shipped agent workflow."""
    result = run_agent(
        h,
        AgentConfig(system_prompt=SYSTEM_PROMPT, tools=TOOLS, max_steps=10),
        f"Research this topic and report what you find: {topic}",
    )
    return result["answer"]
