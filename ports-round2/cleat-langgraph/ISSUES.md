# Known Issues and Gaps

This document catalogs the issues encountered during the Cleat-LangGraph port.
These represent gaps, design tensions, and workarounds that would need to be
resolved for a production-quality integration.

## Critical Gaps

### 1. No Cleat LLM Plugin (Issues #49 Python, #86 AS)

**Status:** Known gap, no implementation exists.

**Impact:** Without an LLM plugin, LLM calls inside LangGraph nodes cannot be
individually checkpointed. The entire node execution is the unit of durability.
If an LLM call takes 30 seconds and the process crashes at 29 seconds, the
entire node (including the LLM call) is re-executed.

**Proposed solution:** See `cleat_langgraph/llm_plugin_design.py` for a design
proposal. The LLM plugin would wrap each LLM API call in a `durable_call`,
recording the prompt and response in Cleat's event log. On replay, cached
responses are returned without re-invoking the LLM.

**Workaround:** Keep LangGraph nodes small (one LLM call per node) so the cost
of re-execution is minimal. Use the workflow loop to checkpoint state between
nodes.

### 2. WASM Sandbox Incompatibility with LangGraph

**Status:** Fundamental architecture constraint.

**Impact:** LangGraph (and its dependencies: LangChain, Pydantic, async Python,
LLM SDKs) cannot run inside the WASM sandbox where Cleat workflows execute.
This prevents the "clean" approach of running the entire LangGraph graph inside
the workflow, as the Temporal plugin does.

**Workaround:** The hybrid architecture described in README.md. LangGraph runs
on the host side; the Cleat workflow orchestrates via `durable_call`. This adds
complexity and limits the dynamic graph introspection that LangGraph provides.

### 3. Async/Sync Mismatch

**Status:** Design tension.

**Impact:** LangGraph is async-first (all node functions, graph invocation,
streaming). Cleat workflows are synchronous. The bridge must wrap async
execution on the host side while keeping the workflow side synchronous.

**Specific issues:**
- `graph.ainvoke()` is async → must be wrapped in `asyncio.run()` on the host.
- Async node functions → same wrapping needed.
- LangGraph's streaming API (`graph.astream()`) is async → cannot stream to the
  synchronous Cleat workflow in real time.
- The Functional API's `@entrypoint` is `async def` → forced to run via
  `asyncio.run()`.

**Workaround:** Each node/task call is an independent async execution. The
`asyncio.run()` pattern works because there is no shared async state across
calls. The limitation is that streaming (per-token) is not possible during
replay.

## Design Tensions

### 4. Streaming Tokens vs Replay Model

**Issue:** LLM streaming produces tokens incrementally. Cleat's event sourcing
records the final result, not the token stream. During replay, the client
receives the full cached response rather than a token stream.

**Possible approaches:**
1. **Buffer-and-replay**: During first execution, buffer all tokens. During
   replay, emit the buffered tokens at natural speed (requires host-side support).
2. **Disable streaming during replay**: The LLM host service detects replay and
   returns the cached full response. Streaming only on first execution.
3. **Record each chunk**: Record each streaming chunk as a separate event (high
   event log overhead).

**Recommendation:** Approach 2 for simplicity. The LLM plugin proposal documents
this as a known limitation.

### 5. Continue-as-New vs Cleat's Event Model

**Issue:** Temporal's `continue-as-new` terminates the current workflow execution
and starts a new one, truncating the event history. This prevents unbounded
history growth for long-running agents. Cleat does not have a native equivalent.

**Workaround in this port:** Each pipeline stage checkpoints its result in Cleat's
durable state store (`h.set_state`). If the workflow restarts, completed stages
are detected via `h.get_state` and skipped. This provides the caching benefit
of `continue-as-new` without truncating history.

**Limitation:** The event log grows unboundedly for agents that make many LLM
calls. Cleat would need a history compaction mechanism or explicit `continue-as-new`
support to match Temporal's behavior.

### 6. Human-in-the-Loop: interrupt() vs Signal

**Issue:** LangGraph's `interrupt()` is tightly coupled to the graph execution
engine. It pauses execution mid-graph and stores the interrupt state in the
checkpointer. Cleat does not have `interrupt()`.

**Workaround:** The Cleat workflow uses `h.await_signals(["signal_name"])` to
pause execution. This is functionally equivalent but different in mechanism:
- `interrupt()` saves state and raises an exception that the runtime catches.
- `await_signals()` is an explicit blocking call in the workflow.

**Limitation:** The signal-based approach requires the workflow to explicitly
manage the pause/resume lifecycle. The graph definition no longer drives the
pause — the workflow does.

### 7. Complex Type Serialization Across WASM Boundary

**Issue:** LangGraph uses complex Python types that don't serialize cleanly:
- `TypedDict` with `Annotated` types (e.g., `Annotated[list[str], operator.add]`)
- Pydantic `BaseModel` with validators
- Nested dataclasses with `field()` defaults
- `datetime`, `Decimal`, `Enum`, etc.

**Current approach:** Strip annotations during transport, convert to JSON-safe
dicts, reconstruct on the host side. This loses type information that LangGraph
uses for state management (e.g., `operator.add` for message accumulation).

**Impact:** The `operator.add` reducer cannot be used across the bridge. Message
accumulation must be managed manually in the workflow loop. This makes the state
management less elegant than the LangGraph native approach.

### 8. Graph API Node Function Discovery

**Issue:** `LangGraphRuntime` needs to discover and call individual node functions
from a compiled graph. LangGraph's internal graph representation varies between
versions, making node function extraction fragile.

**Workaround:** The bridge tries multiple strategies to find node functions:
1. Registered node functions (most reliable — explicit registration).
2. `compiled.get_node(name).fn`
3. `compiled.nodes[name]["fn"]`
4. `compiled.nodes[name]` (direct callable)

**Limitation:** Node function discovery may fail with non-standard graph
configurations or future LangGraph versions. Explicit registration is recommended.

## Cleat SDK Gaps

### 9. Nonexistent _host_calls.py

**Issue:** The `cleat_sdk.__init__` imports from `cleat_sdk._host_calls` but this
module does not exist in the reference SDK. `HostCalls` is only referenced as a
type annotation in decorators and workflow files. The mock implementations in
this project define the expected interface.

**Impact:** The reference SDK is incomplete. `HostCalls` must be implemented by
the Cleat runtime.

### 10. No Explicit durable_entry Return Type

**Issue:** The `@durable_entry` decorator's return type is unconstrained (`Any`).
Since the workflow runs in WASM, the return value must be serializable, but this
is not enforced by the SDK.

**Recommendation:** Add a `Serializable` protocol or return type constraint that
ensures JSON serializability.

### 11. No Native Parallel Execution

**Issue:** The Functional API's parallel futures pattern (create N futures, await
all) cannot be directly ported to Cleat because `durable_call` is synchronous.
True parallelism requires the host runtime to support concurrent operations.

**Workaround:** Sequential processing is safe but slower for independent tasks.
A host-side batch operation could provide parallelism at the cost of coarser
granularity.

## Functional API Specific Issues

### 12. Task Registration and Discovery

**Issue:** The Functional API's `@task` decorator registers tasks in LangGraph's
internal registry. In the Cleat port, tasks must be explicitly registered with
the runtime:

```python
cleat.register_task("validate_item", validate_item)
```

This is more manual than LangGraph's implicit `@task` registration.

### 13. Task Argument Serialization

**Issue:** Functional API tasks can accept arbitrary Python objects. The
`durable_call` request must be JSON-serializable. Complex task arguments
(structured dicts, nested objects) need explicit serialization.

## Missing Features (Not in Scope)

### 14. LangSmith Tracing

**Issue:** The original Temporal samples demonstrate `@traceable` decorators
for LLM observability. This is a Temporal plugin feature, not part of the
LangGraph integration itself.

**Cleat equivalent:** A Cleat observability plugin would be needed. Not in scope
for this port.

### 15. Checkpoint Trees

**Issue:** LangGraph's `PostgresSaver` supports checkpoint trees (branching
execution for experimentation). Cleat's linear event log does not.

**Not in scope:** Checkpoint trees require fundamental Cleat runtime support.

### 16. Dynamic Graph Compilation

**Issue:** The Temporal plugin compiles the graph once and caches it for the
worker's lifetime. The Cleat bridge does the same, but dynamic graph
construction (e.g., varying graph structure per invocation) is not supported.

## Summary of Recommendations

| Priority | Issue | Action |
|----------|-------|--------|
| P0 | No LLM plugin | Implement LLM host service per `llm_plugin_design.py` |
| P0 | WASM + LangGraph | Hybrid architecture (this port's design) |
| P1 | Async/sync bridge | Use `asyncio.run()` per node call |
| P1 | Type serialization | Use `CleatSerializer` with explicit packing |
| P2 | Continue-as-new | State-based caching via `h.set_state`/`h.get_state` |
| P2 | Streaming | Buffer-and-replay pattern in LLM plugin |
| P3 | Graph API node discovery | Explicit registration preferred over introspection |
| P3 | Parallel tasks | Sequential with host-side batching |
