# Cleat-LangGraph Bridge: Durable Execution for LangGraph Agents

A design study port of the [Temporal LangGraph plugin](https://github.com/temporalio/sdk-python/tree/main/temporalio/contrib/langgraph) to the [Cleat](https://cleat.dev) durable execution Python SDK. This project explores how LangGraph's agent-execution framework can benefit from Cleat's durability guarantees without running LangGraph inside the WASM sandbox.

## Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│                    Host Process (outside WASM)                    │
│                                                                  │
│  ┌──────────────────────────────┐  ┌─────────────────────────┐  │
│  │     LangGraph Runtime        │  │   LLM Host Service      │  │
│  │  (graph compilation, node    │  │   (proposed plugin)     │  │
│  │   execution, routing)        │  │   - Anthropic/OpenAI    │  │
│  └──────────┬───────────────────┘  │   - Cached responses   │  │
│             │                       └──────────┬──────────────┘  │
│             │ durable_call("langgraph", ...)    │                 │
│             ▼                                   ▼                 │
│  ┌──────────────────────────────────────────────────────────┐    │
│  │              Cleat Runtime (WASM Sandbox)                 │    │
│  │                                                          │    │
│  │  @durable_entry(name="react_agent")                       │    │
│  │  def react_agent_workflow(h: HostCalls, query: str):      │    │
│  │      agent = CleatLangGraph(h, "react-agent")             │    │
│  │      state = {"input": query, ...}                       │    │
│  │      while not done:                                      │    │
│  │          state = agent.step(state)  # ← durable_call     │    │
│  │      return state["final_answer"]                        │    │
│  └──────────────────────────────────────────────────────────┘    │
└──────────────────────────────────────────────────────────────────┘
```

### Key Design Decision: Hybrid Architecture

LangGraph cannot run inside the WASM sandbox (it requires async Python, third-party libraries, and LLM SDKs). Instead, we use a **hybrid architecture**:

| Layer | Location | Role | Async? |
|-------|----------|------|--------|
| LangGraph graph definition | Host (outside WASM) | Define StateGraph, `@task`, `@entrypoint` | Yes (async) |
| Graph node execution | Host | Run node functions, routing logic | Yes (async) |
| `LangGraphRuntime` | Host | Manage compiled graphs, dispatch operations | Sync wrapper |
| Cleat workflow (`@durable_entry`) | WASM sandbox | Orchestrate execution, call host via `durable_call` | No (sync) |
| `CleatLangGraph` client | WASM sandbox | Wrap `durable_call` into step/node API | No (sync) |

### Durability Model

The bridge provides **per-node durability**:

1. Each call to `agent.step(state)` becomes a `durable_call("langgraph", "step", ...)` which is recorded in Cleat's event log.
2. On replay (after crash), Cleat replays the event log. Cached `durable_call` results are returned without re-executing nodes.
3. The `LangGraphRuntime` on the host side is **stateless** — all execution state is passed through the request/response cycle.
4. The host caches compiled graphs by name but does not store execution state between calls.

**Granularity comparison:**

| Approach | Event Log Entries | Replay Efficiency | Complexity |
|----------|------------------|-------------------|------------|
| `invoke_graph` (whole graph) | 1 per execution | Lowest (re-runs all nodes) | Simplest |
| `step()` (per node) | N per execution (N = nodes) | High (skips completed nodes) | Moderate |
| `invoke_entrypoint` (whole function) | 1 per execution | Lowest | Simplest |
| `execute_task` (per task) | M per execution (M = tasks) | High (skips completed tasks) | Moderate |

The per-node/per-task approach is recommended for production agents.

## Ported Samples

### Graph API (StateGraph)

| Sample | File | Temporal Original | Key Cleat Pattern |
|--------|------|-------------------|-------------------|
| Hello World | `graph_api/hello_world/` | Single-node StateGraph | `agent.step(state)` for single node |
| ReAct Agent | `graph_api/react_agent/` | Conditional edges + tool loop | `while not done: agent.step(state)` |
| Human-in-the-loop | `graph_api/human_in_the_loop/` | `interrupt()` + Temporal signals | `h.await_signals()` instead of `interrupt()` |
| Continue-as-new | `graph_api/continue_as_new/` | `workflow.continue_as_new()` + cache | `h.set_state()` / `h.get_state()` for caching |

### Functional API (@task / @entrypoint)

| Sample | File | Temporal Original | Key Cleat Pattern |
|--------|------|-------------------|-------------------|
| Hello World | `functional_api/hello_world/` | Single `@task` | `durable_call("execute_task", ...)` |
| ReAct Agent | `functional_api/react_agent/` | `while` loop with `@task` | Per-task `durable_call` in workflow loop |
| Human-in-the-loop | `functional_api/human_in_the_loop/` | `interrupt()` + signals | `h.await_signals()` + task execution |
| Control Flow | `functional_api/control_flow/` | Parallel futures, `for`, `if/else` | Sequential durable_calls + conditional routing |
| Continue-as-new | `functional_api/continue_as_new/` | `continue_as_new` + cache | Per-task state caching |

## Architectural Decisions and Trade-offs

### 1. Sync vs Async Bridging

**Challenge:** LangGraph (and Temporal) are async-first. LangGraph node functions are `async def`, and `graph.ainvoke()` is async. Cleat workflows are synchronous (WASM sandbox).

**Solution:** The host-side `LangGraphRuntime` wraps async execution in `asyncio.run()` for each individual node call. This is safe because:
- Each node call is independent (no shared state across calls).
- The Cleat workflow drives the execution loop synchronously.
- Async node functions run to completion within each `durable_call`.

**Limitation:** True concurrent node execution (as in the Functional API's parallel futures pattern) is not possible with sync `durable_call`. Sequential processing is the default.

### 2. Checkpoint/Replay: PostgresSaver vs Event Sourcing

**Temporal:** LangGraph's `PostgresSaver` checkpoints the full graph state after each node. The checkpointer tracks thread_id, checkpoint_id, and parent_checkpoint_id.

**Cleat:** Uses event sourcing. Each `durable_call` records the operation and its parameters. On replay, Cleat replays from the event log.

**Implication:** Cleat's approach naturally supports per-call replay but does not provide a checkpoint tree for branching/experimentation. The `PostgresSaver` pattern would need to be implemented as a Cleat host service.

### 3. Streaming and Replay

**Temporal:** LangGraph streaming yields per-node events. Streaming tokens from LLMs can pass through Temporal's streaming channels.

**Cleat:** Replay replays from the event log. Streaming tokens produced during the original execution are not recorded — only the final result is.

**Solution:** LLM streaming should be disabled during replay. The LLM host service can detect replay by the presence of cached results and skip streaming. During first execution, streaming works normally but the full response is buffered for cache (as proposed in `llm_plugin_design.py`).

### 4. Complex Python Types

**Challenge:** LangGraph uses TypedDict, Pydantic models, dataclasses, and `Annotated` types (e.g., `Annotated[list[str], operator.add]`). Cleat's `durable_call` requires JSON-safe dicts.

**Solution:** The `serialization.py` module handles bidirectional conversion:
- `CleatSerializer.pack()` converts TypedDict/dataclass/Pydantic to JSON-safe dicts.
- `LangGraphSerializer.to_langgraph_state()` reconstructs state on the host side.
- `operator.add` annotations are stripped during transport; the workflow handles message accumulation manually.

### 5. Graph API vs Functional API: Which Maps Better?

| Aspect | Graph API | Functional API |
|--------|-----------|----------------|
| Cleat mapping | `agent.step()` per node | `durable_call("execute_task")` per task |
| Loop control | Workflow loop + host routing | Workflow loop + conditional logic |
| State management | TypedDict passed through | Task-specific arguments |
| Complexity | Higher (graph introspection needed) | Lower (explicit task calls) |
| Flexibility | Higher (conditional edges, dynamic routing) | Lower (fixed task signatures) |

**Verdict:** The Functional API maps more naturally to Cleat because each `@task` maps directly to a `durable_call`, and the `@entrypoint` body maps directly to the workflow function. The Graph API requires extra infrastructure for routing and node discovery.

## Running the Samples

Each sample can be run as a standalone module:

```bash
# Graph API samples
python -m cleat_langgraph.graph_api.hello_world.main
python -m cleat_langgraph.graph_api.react_agent.main
python -m cleat_langgraph.graph_api.human_in_the_loop.main
python -m cleat_langgraph.graph_api.continue_as_new.main

# Functional API samples
python -m cleat_langgraph.functional_api.hello_world.main
python -m cleat_langgraph.functional_api.react_agent.main
python -m cleat_langgraph.functional_api.human_in_the_loop.main
python -m cleat_langgraph.functional_api.control_flow.main
python -m cleat_langgraph.functional_api.continue_as_new.main
```

**Note:** The samples use a `MockCleatRuntime` for design study purposes. A real Cleat runtime would provide actual WASM sandbox execution.

## Project Structure

```
cleat-langgraph/
├── README.md                    # This file
├── ISSUES.md                    # Known issues and gaps
├── services_contract.md         # LangGraph host service API reference
├── pyproject.toml               # Python project config
├── cleat_langgraph/
│   ├── __init__.py              # Package exports
│   ├── bridge.py                # Core bridge SDK (LangGraphRuntime + CleatLangGraph)
│   ├── serialization.py         # Type serialization for WASM boundary
│   ├── host.py                  # Cleat host service registration
│   ├── llm_plugin_design.py     # Proposed LLM plugin design
│   ├── graph_api/               # Graph API samples
│   │   ├── hello_world/         # Minimal single-node graph
│   │   ├── react_agent/         # Tool-calling agent loop
│   │   ├── human_in_the_loop/   # Signal-based human review
│   │   └── continue_as_new/     # State caching pipeline
│   └── functional_api/          # Functional API samples
│       ├── hello_world/         # Single task
│       ├── react_agent/         # Task-based agent loop
│       ├── human_in_the_loop/   # Signal-based human review
│       ├── control_flow/        # Branching and loops
│       └── continue_as_new/     # Per-task caching
```

## Key Files

| File | Purpose |
|------|---------|
| `cleat_langgraph/bridge.py` | Core SDK: `LangGraphRuntime` (host) + `CleatLangGraph` (workflow) |
| `cleat_langgraph/serialization.py` | JSON-safe serialization for TypedDict, Pydantic, dataclasses |
| `cleat_langgraph/host.py` | Cleat runtime service registration |
| `cleat_langgraph/llm_plugin_design.py` | Proposal for Cleat LLM plugin |
| `services_contract.md` | Host service API contract |

## Related

- [Temporal LangGraph plugin](https://github.com/temporalio/sdk-python/tree/main/temporalio/contrib/langgraph)
- [Cleat Python SDK](https://github.com/cleat-dev/cleat)
- [LangGraph documentation](https://langchain-ai.github.io/langgraph/)
