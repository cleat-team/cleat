# Python LangChain Research Agent

A complete example demonstrating Cleat's LangChain integration with durable
execution, crash recovery, and deterministic replay.

## What It Demonstrates

1. **Durable LangChain Agents** — Every LLM call and tool invocation is recorded
   in the Cleat event history.
2. **Crash Recovery** — Kill the worker mid-execution, restart, and the agent
   resumes from the last checkpoint without losing progress.
3. **No Duplicate API Costs** — Replayed steps return cached results from the
   event history instead of re-contacting the LLM provider.
4. **Deterministic Results** — Same inputs always produce the same outputs,
   guaranteed by the event log.

## Files

| File | Purpose |
|---|---|
| `research_agent.py` | The complete research agent — LLM loop, tool definitions, durable state tracking |
| `crash_demo.sh` | Bash script demonstrating a worker crash and recovery cycle |
| `README.md` | This file |

## Quickstart

### Prerequisites

```bash
# Install the Cleat Python SDK
pip install cleat-sdk

# For LangChain integration (optional — the demo uses Cleat's native plugin API)
pip install cleat-sdk[langchain] langchain-openai

# Set your API key
export OPENAI_API_KEY=sk-...
```

### Run with Test Harness (no WASM)

```bash
cd examples/python-langchain
python research_agent.py --test
```

This runs the agent with an inline mock — no WASM, no network, no API keys.
It verifies the agent logic end-to-end and prints the recorded call history.

### Run with Cleat Worker

```bash
# Build the WASM component
durable build --target python --entry research_agent.py:langchain_research_agent

# Run it
durable run langchain_research_agent \
    '{"topic": "Compare Temporal, DBOS, and Cleat"}'
```

To see costs only (no execution):

```bash
python research_agent.py --costs
```

## Demo: Crash Recovery

```bash
# Terminal 1: Start the agent
durable run langchain_research_agent \
    '{"topic": "Latest developments in fusion energy"}'

# During execution — after a few steps — kill the worker
kill -9 $(pgrep cleat-worker)

# Terminal 2: Check the dashboard (shows partial execution)
# Dashboard will show recorded events from completed steps

# Restart the worker — the agent resumes from the last checkpoint
durable worker start
```

## Architecture

```
research_agent.py
│
├── _research_agent_impl()     ← Core workflow logic (undecorated, testable)
│   ├── CleatCallbackHandler   ← Records LangChain agent steps (demonstrated)
│   ├── Plugins.llm_chat()     ← Deterministic LLM call (cleat-durable)
│   ├── set_state()            ← Progress persisted in durable state
│   ├── poll_cancellation()    ← Graceful cancellation support
│   └── _execute_tool()        ← Tool dispatch (also durable)
│
└── langchain_research_agent() ← @durable_entry wrapper (WASM export)

SDK modules used:
  cleat_sdk.host_calls         ← HostCalls (durable_log, set_state, now, ...)
  cleat_sdk.plugins            ← Plugins.llm_chat, Plugins.llm_embed, ...
  cleat_sdk.langchain          ← CleatCallbackHandler
  cleat_sdk.langgraph          ← CleatCheckpointer (for LangGraph state)
  cleat_sdk.test_harness       ← CleatTestHarness (for unit testing)
```

## Key Concepts

### Deterministic Replay

When a Cleat workflow executes, every `plugin_call`, `durable_call`, `set_state`,
and `durable_sleep` appends an event to the workflow history. If the worker
crashes and restarts, the runtime replays the history from the beginning.
Instead of making real API calls, every replayed event returns the result that
was recorded the first time. Execution continues from the last recorded event
as if nothing happened.

### No Double-Billing

Because replayed events return cached results, LLM API calls that completed
before the crash are **not re-executed**. Only new steps after the crash point
incur API charges.

### LangGraph Integration

For LangGraph-based agents, use `CleatCheckpointer` as the checkpointer:

```python
from cleat_sdk.langgraph import CleatCheckpointer

checkpointer = CleatCheckpointer(h)
graph = StateGraph(MyState)
app = graph.compile(checkpointer=checkpointer)
result = app.invoke({"input": topic},
                     config={"configurable": {"thread_id": "research-1"}})
```

This stores LangGraph checkpoints in Cleat's durable state, enabling the same
crash-recovery guarantees for LangGraph-based agents.
