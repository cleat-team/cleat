# Python/WASM FFI + LangChain Integration Plan

May 2026. How to make cleat workflows runnable from Python, and how to integrate
with LangChain/LangGraph so AI developers can write durable agents in Python.

---

## Gate: When to Start This Stream

The ai-ready.md plan is explicit: only invest here after the Go-native AI launch
generates Python demand. The signals to start:

- Multiple "do you support Python?" questions on launch posts
- A design partner asking for Python
- LangChain community interest

**Current status (May 2026):** Go-native AI stream is 100% complete. The Python
SDK has AI plugin wrappers (llm_chat, pgvector_search, etc.) and 6 new HostCalls
methods. The launch has not yet happened — this plan is written so it's ready
when the gate opens.

---

## Current State

| Component | Status |
|-----------|--------|
| Python SDK (`cleat_sdk/`) | ~2,500 lines, 32 WASM import stubs |
| `@durable_entry` decorator | Working — generates WASM ABI export wrappers |
| `HostCalls` class | 46+ methods, all backed by `_import_*` stubs |
| `CleatTestHarness` | Working — WASM-free testing with stubs and assertions |
| Memory module | Working — bytearray-backed linear memory, bit-packing |
| AI plugin wrappers | 6 methods in `plugins.py` (llm_chat, pgvector_search, etc.) |
| `_import_*` stubs | 32 stubs raise `NotImplementedError` — the core gap |
| WASM compilation | No pipeline exists |
| LangChain integration | No code exists |
| Python agent template | No template exists |

The SDK is well-structured and testable without WASM. The gap is the compilation
pipeline that turns `@durable_entry` functions into runnable WASM modules.

---

## Workstream D1: Wire WASM FFI (P0, ~3 weeks)

### The Problem

All 32 `_import_*` stubs raise `NotImplementedError`. The `@durable_entry`
decorator generates WASM ABI-compatible export wrappers, but the imports
are Python stubs that don't actually call the host. When compiled to WASM
and loaded by a cleat worker, the stubs must become real WASM imports.

### Approach: componentize-py

`componentize-py` (Bytecode Alliance) compiles a CPython interpreter + Python
source into a WASM component using the component model. It uses WIT (Wasm
Interface Types) files to declare imports and exports.

The workflow:
```
Python source (.py)
    │
    ▼
componentize-py + WIT bindings
    │
    ▼
WASM component (.wasm)
    │
    ▼
cleat worker (wazero + component model adapter)
```

### Step 1: Define the WIT Interface (~3 days)

Create `python-sdk/wit/cleat.wit` describing cleat's host imports:

```wit
package cleat:host-calls;

interface durable-call {
    /// Bit-packed result: (response_len << 40) | (call_error_code << 8) | err_code
    durable-call: func(service-ptr: u32, service-len: u32,
                       operation-ptr: u32, operation-len: u32,
                       request-ptr: u32, request-len: u32,
                       response-ptr: u32, response-max-len: u32) -> u64;
}

interface durable-sleep {
    /// Bit-packed result: (status << 56) | duration_ms
    durable-sleep: func(duration-ms: u64) -> u64;
}

// ... 30 more interfaces following the same pattern
```

The WIT file maps 1:1 to the existing `_import_*` stubs. Each function
signature is already documented in the Python memory module and the Go
WASM adapter (`internal/wasm/adapter.go`).

**Key decision:** Use a single WIT interface with all 32 imports vs. separate
interfaces per category. Recommendation: group by category (durable-call,
signals, promises, state, plugin) for readability.

**Files touched:**
- `python-sdk/wit/cleat.wit` — new file (~200 lines)

### Step 2: Generate Python Bindings from WIT (~2 days)

Use `componentize-py`'s bindings generator to create Python wrapper modules
that replace the `_import_*` stubs:

```bash
componentize-py bindings --world cleat-workflow python-sdk/wit/ --output python-sdk/cleat_sdk/_wit/
```

This generates `python-sdk/cleat_sdk/_wit/cleat_host_calls.py` with actual
WASM import functions. Then update `host_calls.py` to use these instead of
the stubs when running in WASM:

```python
# In host_calls.py:
try:
    from ._wit.cleat_host_calls import (
        durable_sleep as _import_durable_sleep,
        durable_call as _import_durable_call,
        # ... all 32 imports
    )
    _USING_WASM = True
except ImportError:
    # Fall back to stubs for local testing
    _USING_WASM = False
```

**Files touched:**
- `python-sdk/wit/cleat.wit` — may need refinement
- `python-sdk/cleat_sdk/host_calls.py` — replace stubs with conditional WIT imports
- `python-sdk/cleat_sdk/_wit/` — generated (gitignored or committed)

### Step 3: Build the Compilation Pipeline (~4 days)

Create the tooling that compiles Python workflows to WASM:

**a) `componentize-py` configuration**

Create `python-sdk/pyproject.toml` with componentize-py settings. The workflow
app is a Python module that `componentize-py` compiles with CPython embedded:

```bash
componentize-py componentize \
  --wit python-sdk/wit/ \
  --world cleat-workflow \
  -o my_workflow.wasm \
  my_workflow.py
```

Binary size: 15-25 MB (CPython WASM + stdlib). Acceptable for server-side.

**b) CLI integration**

Add `--language python` to `cleat build`:

```bash
cleat build --target python --entry my_workflow.py:research_agent
```

This wraps the componentize-py invocation and produces a `.wasm` file.

**c) Worker loading**

The cleat worker uses wazero, which can load WASM components. However, the
component model adds complexity. The simpler approach for MVP:

Use `componentize-py` to produce a core WASM module (not a component) with
standard WASM imports/exports. This avoids needing full component model
support in the worker.

**Alternative approach (if componentize-py is too heavy):**

Write a lightweight Python-to-WASM adapter that:
1. Parses the `@durable_entry` decorated functions from Python source
2. Generates a thin WASM module that:
   - Imports the 32 host functions
   - Embeds the Python source as a string
   - At runtime, runs a bundled MicroPython or CPython WASM interpreter
   - Calls the decorated function via the interpreter

This is more work but avoids dependency on the evolving component model
ecosystem. The `py2wasm` project or MicroPython compiled to WASM are options.

**Recommendation:** Start with `componentize-py` (it's the standard path).
If it proves unreliable, fall back to the embedded interpreter approach.
The WIT interface definition work (Step 1) is needed for both paths.

**Files touched:**
- `python-sdk/pyproject.toml` — new file
- `cmd/cleat-build/` — add Python target
- `internal/wasm/build.go` — Python WASM build support

### Step 4: End-to-End Validation (~3 days)

Prove the pipeline works end to end:

1. Write `examples/python-hello/hello_workflow.py`:
   ```python
   from cleat_sdk import HostCalls, durable_entry

   @durable_entry("Hello")
   def hello(h: HostCalls, name: str) -> str:
       greeting = h.durable_call("greeter", "greet", {"name": name})
       return greeting
   ```

2. Compile: `cleat build --target python --entry hello_workflow.py:hello`
3. Load in cleat worker
4. Execute fresh
5. Kill worker mid-execution, restart, verify replay
6. Verify event history is correct

**Files touched:**
- `examples/python-hello/` — new example directory (~4 files)
- `python-sdk/tests/test_wasm_compilation.py` — new test file

### Step 5: Handle the Remaining 26 Stubs (~3 days)

After the first import works end-to-end, wire the remaining 31 stubs. The
pattern is identical for all of them — this is mechanical work:

| Category | Imports | Complexity |
|----------|---------|------------|
| Core calls | durable_call, durable_call_retry, durable_call_heartbeat | Medium (bit-packing) |
| Sleep/timers | durable_sleep, durable_now, durable_random | Simple |
| Signals | await_signals, poll_signal, poll_cancellation, send_signal_and_wait | Medium |
| Children | child_workflow, await_child, await_all_children | Medium |
| Promises | create_promise, await_promise, resolve_promise, reject_promise | Medium |
| State | set_query_state | Simple |
| Lifecycle | durable_defer, continue_as_new | Medium |
| Plugin | plugin_call | Medium |
| Handlers | register_update_handler, register_query_handler | Medium |
| Messaging | durable_send, schedule_invoke, signal_workflow, reply_to_signal | Medium |
| Identity | workflow_id, run_id | Simple |
| Logging | durable_log | Simple |
| Versioning | durable_version, durable_min_version | Simple |

**Validation per import:** Each wired import must pass a test that:
1. Calls the function in a test workflow
2. Verifies the event is recorded in history
3. Kills and replays the workflow, verifying deterministic replay

---

## Workstream D2: Remaining Feature Gaps (P1, ~3 days)

Most gaps were closed in the SDK expansion. What's left:

### D2a: State Enumeration (already implemented in HostCalls, needs WASM wiring)

- `has_state(key)` — ✅ implemented in HostCalls, needs WASM import
- `list_state(prefix)` — ✅ implemented in HostCalls, needs WASM import

### D2b: Streaming Support in Python SDK (~1 day)

The Go SDK now has `PluginCallStreaming`. Add the equivalent to the Python SDK:

```python
# In host_calls.py:
def plugin_call_streaming(self, plugin_name: str, function_name: str,
                          input_json: str) -> Iterator[StreamEvent]:
    """Call a plugin function that returns a stream of events."""
    # Calls _import_plugin_call_streaming, reads events from scratch buffer
    ...
```

Requires a new WASM import: `_import_plugin_call_streaming`.

### D2c: Python Agent Template (~1 day)

Add `durable init --template agent --language python`:

```
my-python-agent/
├── agent.py           # Agent workflow
├── requirements.txt   # cleat-sdk, langchain, openai, etc.
├── cleat.toml         # Project config
└── README.md          # Quickstart
```

The agent.py template:
```python
from cleat_sdk import HostCalls, durable_entry
from cleat_sdk.plugins import Plugins

SYSTEM_PROMPT = """You are a helpful research assistant. Use tools when needed."""

@durable_entry("ResearchAgent")
def research_agent(h: HostCalls, topic: str) -> str:
    plugins = Plugins(h)
    messages = [{"role": "system", "content": SYSTEM_PROMPT},
                {"role": "user", "content": topic}]
    
    for step in range(10):
        result = plugins.llm_chat(
            provider="openai", model="gpt-4o",
            messages=messages,
            tools=[...]
        )
        if not result.tool_calls:
            return result.message["content"]
        
        for tc in result.tool_calls:
            tool_result = execute_tool(h, tc)
            messages.append({"role": "tool", "content": tool_result})
    
    return "Max steps reached"
```

**Files touched:**
- `cmd/cleat/templates/agent-python/` — new directory (~5 files)
- `python-sdk/cleat_sdk/host_calls.py` — add streaming support
- `python-sdk/cleat_sdk/memory.py` — add streaming decoder

---

## Workstream D3: LangChain Integration (P1, ~3 weeks)

### The Strategy

Cleat doesn't replace LangChain — it wraps it. AI developers write LangChain
agents normally, and cleat provides durability transparently through
callbacks and checkpoints.

### D3a: CleatCallbackHandler (~1 week)

LangChain's `BaseCallbackHandler` fires events for every LLM call, tool
invocation, chain step, and agent action. The `CleatCallbackHandler` records
these as cleat events:

```python
# cleat_sdk/langchain/callbacks.py
from langchain.callbacks.base import BaseCallbackHandler
from cleat_sdk.host_calls import HostCalls

class CleatCallbackHandler(BaseCallbackHandler):
    """Records LangChain steps as cleat durable events."""

    def __init__(self, h: HostCalls):
        self.h = h
        self.step_counter = 0

    def on_llm_start(self, serialized, prompts, **kwargs):
        self.step_counter += 1
        # LLM call is made through cleat's plugin_call, which records it

    def on_llm_end(self, response, **kwargs):
        # Response is already recorded in event history
        pass

    def on_tool_start(self, serialized, input_str, **kwargs):
        self.step_counter += 1
        self.h.durable_call(
            serialized.get("name", "tool"), "execute",
            json.dumps({"input": input_str})
        )

    def on_agent_action(self, action, **kwargs):
        # Record agent decision in event history
        self.h.set_state(f"step_{self.step_counter}_action", action.log)

    def on_agent_finish(self, finish, **kwargs):
        self.h.set_state("final_answer", finish.log)
```

The developer experience:

```python
from cleat_sdk import HostCalls, durable_entry
from cleat_sdk.langchain.callbacks import CleatCallbackHandler
from langchain.agents import create_openai_functions_agent
from langchain_openai import ChatOpenAI

@durable_entry("ResearchAgent")
def research_agent(h: HostCalls, topic: str) -> str:
    callback = CleatCallbackHandler(h)
    llm = ChatOpenAI(model="gpt-4o", callbacks=[callback])
    agent = create_openai_functions_agent(llm, tools, prompt)
    
    result = agent.invoke({"input": topic}, config={"callbacks": [callback]})
    return result["output"]
```

Key insight: the LLM calls flow through cleat's `plugin_call`, so they're
automatically recorded in event history. The callback handler adds the
LangChain-specific metadata (tool calls, agent actions, chain steps).

### D3b: CleatCheckpointer for LangGraph (~1 week)

LangGraph uses a checkpointer interface to save/restore agent state.
Implement `CleatCheckpointer` that uses cleat event history:

```python
# cleat_sdk/langgraph/checkpoint.py
from langgraph.checkpoint.base import BaseCheckpointSaver, CheckpointTuple

class CleatCheckpointer(BaseCheckpointSaver):
    """Uses cleat event history as LangGraph's checkpoint backend."""

    def __init__(self, h: HostCalls):
        self.h = h

    def get_tuple(self, config):
        # Read checkpoint from cleat durable state
        thread_id = config["configurable"]["thread_id"]
        raw = self.h.get_state(f"langgraph_checkpoint_{thread_id}")
        if raw is None:
            return None
        return _deserialize_checkpoint(raw)

    def put(self, config, checkpoint, metadata, new_versions):
        # Write checkpoint to cleat durable state
        thread_id = config["configurable"]["thread_id"]
        self.h.set_state(
            f"langgraph_checkpoint_{thread_id}",
            _serialize_checkpoint(checkpoint, metadata)
        )

    def put_writes(self, config, writes, task_id):
        # Intermediate writes go to state too
        ...
```

This means LangGraph's state graph is backed by cleat's durable state.
When the worker crashes and restarts, the LangGraph agent resumes from
the last checkpoint — no lost state, no duplicate LLM calls.

### D3c: Example — LangChain Research Agent (~3 days)

Build a complete example that demonstrates the value:

```python
# examples/python-langchain/research_agent.py
@durable_entry("ResearchAgent")
def research(h: HostCalls, topic: str) -> str:
    callback = CleatCallbackHandler(h)
    
    llm = ChatOpenAI(model="gpt-4o", callbacks=[callback])
    tools = [
        WebSearchTool(callbacks=[callback]),
        CalculatorTool(callbacks=[callback]),
    ]
    
    agent = create_openai_functions_agent(llm, tools, RESEARCH_PROMPT)
    
    # This entire agent run survives crashes
    result = agent.invoke({"input": topic}, config={"callbacks": [callback]})
    return result["output"]
```

**Demo script:**
1. Start research agent on "Compare Temporal, DBOS, and Cleat"
2. Mid-execution (after 3 LLM calls), kill the worker
3. Restart worker
4. Agent continues — same LLM responses replayed, no duplicate API costs
5. Dashboard shows: $0.87 actual cost vs $1.74 if starting from scratch

### D3d: LlamaIndex Integration (~3 days, optional)

Follow the same callback/checkpoint pattern for LlamaIndex. Lower priority
than LangChain but addresses the second-largest Python AI framework.

---

## Workstream D4: PyPI Publishing (P2, ~3 days)

Package and publish:

### Package structure
```
python-sdk/
├── pyproject.toml
├── README.md
├── cleat_sdk/
│   ├── __init__.py
│   ├── host_calls.py
│   ├── entry.py
│   ├── memory.py
│   ├── types.py
│   ├── plugins.py
│   ├── client.py
│   ├── test_harness.py
│   ├── langchain/
│   │   ├── __init__.py
│   │   └── callbacks.py
│   └── langgraph/
│       ├── __init__.py
│       └── checkpoint.py
└── tests/
    ├── test_host_calls.py
    ├── test_plugins.py
    ├── test_test_harness.py
    └── test_langchain.py
```

### pyproject.toml
```toml
[project]
name = "cleat-sdk"
version = "0.2.0"
description = "Python SDK for cleat — durable execution framework"
requires-python = ">=3.10"
dependencies = []

[project.optional-dependencies]
langchain = ["langchain>=0.3.0", "langchain-openai>=0.2.0"]
langgraph = ["langgraph>=0.2.0"]

[project.urls]
Repository = "https://github.com/cleat-team/cleat"
```

### Publishing
```bash
cd python-sdk
python -m build
twine upload dist/cleat-sdk-0.2.0.tar.gz
```

---

## Timeline

| Week | Workstream | Deliverable |
|------|-----------|-------------|
| 1 | D1 Steps 1-2 | WIT interface defined, bindings generated |
| 2 | D1 Steps 3-4 | Compilation pipeline, hello workflow validates end-to-end |
| 3 | D1 Step 5, D2 | All 32 imports wired, streaming support, agent template |
| 4 | D3a | CleatCallbackHandler for LangChain |
| 5 | D3b | CleatCheckpointer for LangGraph |
| 6 | D3c, D3d | Research agent example, LlamaIndex (optional) |
| 7 | D4 | PyPI publishing, docs, quickstart |

**Total: 7 weeks** (reduced from original 11-week estimate because the Python SDK
is more complete than when the original estimate was made — AI plugin wrappers,
6 HostCalls methods, and CleatTestHarness are already done).

---

## Dependencies

```
D1: WASM FFI ──────┬──────────────────────┐
                    │                      │
D2: Feature gaps ──┤                      │
                    │                      │
                    ▼                      ▼
              D3: LangChain          D4: PyPI
              integration            publishing
```

- D2 can overlap with D1 Step 5 (they touch different files)
- D3 requires D1 complete (LangChain callbacks need working plugin_call via WASM)
- D4 can happen any time after D3a (can publish without LangGraph initially)

---

## Risks and Mitigations

| Risk | Severity | Mitigation |
|------|----------|------------|
| `componentize-py` is unreliable or too immature | HIGH | Fall back to embedded CPython-in-WASM approach. The WIT interface work is reusable for both paths. POC the hello workflow in week 1 before committing to the full pipeline. |
| CPython WASM binary too large (15-25 MB) | MEDIUM | Acceptable for server-side. Document the trade-off. Investigate `py2wasm` for smaller binaries if user feedback demands it. |
| LangChain API changes break integration | MEDIUM | Pin LangChain version. The callback interface is stable; the checkpointer interface is newer but LangGraph's commit rate is slowing. |
| WASM component model support in wazero | MEDIUM | wazero has partial component model support. If insufficient, use core WASM modules (not components) — `componentize-py` can produce both. |
| Python WASM performance (CPython in WASM is slower than native) | LOW | Workflow code is I/O-bound (waiting for LLM responses). WASM overhead is negligible compared to LLM latency. |
| Nobody asks for Python after launch | EXISTENTIAL | This is why the gate exists. Don't start this work until market demand is proven. |

---

## Success Criteria

**Week 3:** `cleat build --target python` compiles a Python workflow to WASM.
The workflow executes on a cleat worker. Kill/restart replays correctly.

**Week 5:** LangChain agent survives a worker crash and resumes from the last
checkpoint. Event history shows all LLM calls, tool invocations, and agent actions.

**Week 7:** `pip install cleat-sdk[langchain]` works. The research agent example
runs end-to-end. Demo: 30-second video showing crash recovery with cost comparison.

---

## What This Enables

Once Python/WASM + LangChain ships, cleat's addressable market expands from
"Go shops with PostgreSQL" to "any AI developer who wants durable agent
execution." The key unlock:

- Python is the #1 language for AI/ML
- LangChain is the most popular agent framework (20K+ GitHub stars)
- LangGraph is the fastest-growing agent orchestration library
- No existing product provides durable execution for LangChain agents
- The "survive the crash" demo with Python code speaks directly to the AI
  developer's pain point

The Go-native AI demo proved the technology works. The Python/LangChain
integration makes it accessible to the developers who actually build AI agents.
