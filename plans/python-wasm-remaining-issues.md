# Python/WASM: Implementation Plan — Phase 1 (Fix) + Phase 2 (wasmtime-go)

**Date:** 2026-05-12
**Branch:** tmp/plugin-python
**Target:** Single cleat binary supporting Go (wazero) + Python (wasmtime-go) side-by-side

---

## Architecture Overview (end state)

```
cleat worker
  ├─ Engine (dispatch, replay, event history) — unchanged core logic
  ├─ HostHandler / execSession — unchanged business logic for all host functions
  ├─ Runtime abstraction (new interface)
  │   ├─ wazeroBackend  — Go, Rust, AssemblyScript, Java workflows
  │   └─ wasmtimeBackend — Python workflows (Component Model native)
  ├─ PluginLoader — unchanged (only compiles, doesn't instantiate)
  ├─ WASMCache — unchanged (caches raw []byte)
  └─ build pipeline
       ├─ Go:   go build GOOS=wasip1 → core WASM → wazero
       └─ Python: componentize-py → Component Model → wasmtime (no decompose!)
```

Key insight: We do NOT abstract `wazero.CompiledModule` and `api.Module` at the type
level. Instead we abstract at the **execution** level — a single method that takes
raw WASM bytes + entry point + input, and returns result + suspend state. Each
backend owns its own compilation, instantiation, and host function wiring.

---

## Phase 1: Fix Current Pipeline (P0)

### Task 1.1: Implement Stream R stubs in engine.go

**File:** `internal/host/engine.go` (lines 3038-3072)

Replace the 8 placeholder methods on `execSession` with real implementations:

- **SetState/GetState/DeleteState/IncrState/HasState/ListState** — delegate to a new
  `stateStore` field on execSession (or reuse the existing `state` WorkflowState field).
  On fresh execution, record in event history and apply. On replay, read from event history.
  These follow the same fresh/replay dispatch pattern as every other method.

- **RunDetached** — delegate to Engine's existing child workflow spawner
  (`childWfStore`) with `Detached` parent close policy.

- **Fetch** — implement as a recorded durable call. Requires a new `Fetcher` interface
  (similar to `ServiceCaller`) injected into Engine. On fresh execution, call the
  fetcher, record response in event history. On replay, return recorded response.

The `ServiceCaller` already exists as:
```go
type ServiceCaller interface {
    Call(ctx context.Context, service, operation, requestJSON string) (responseJSON string, err error)
}
```

Add:
```go
type Fetcher interface {
    Fetch(ctx context.Context, method, url, headersJSON, body string) (responseJSON string, err error)
}
```

Estimated: ~300 lines in engine.go, ~10 lines in imports.go (no changes needed, already wired).

### Task 1.2: Harden wasm-tools decompose in build_wasm.py

**File:** `python-sdk/scripts/build_wasm.py` (lines 316-333)

Change the decompose step from best-effort to required:
- If `wasm-tools` is not found, fail with a clear error message including install instructions
- Add `--skip-decompose` flag for debugging/CI environments where wasm-tools is not available
- Add `--keep-component` flag to preserve the original Component Model binary alongside the core module
- Check the exit code properly and report the specific error

Estimated: ~30 lines changed in build_wasm.py.

### Task 1.3: Add EventRecord fields for state/fetch/detached

**File:** `internal/host/engine.go` (EventRecord struct, line 61)

Add fields to EventRecord for the new event types:
- State mutations: `StateKey`, `StateValue`, `StateDelta`, `StateKeys` (for list)
- Fetch: `FetchMethod`, `FetchURL`, `FetchHeaders`, `FetchBody`, `FetchResponse`
- Detached run: `DetachedName`, `DetachedInput`, `DetachedRunID`

These must be tagged with `json:"...,omitempty"` to keep event history compact.

Estimated: ~20 lines.

### Task 1.4: Replay-safe local mode for Python SDK tests

**File:** new file `python-sdk/cleat_sdk/local_host.py`

Create a `LocalHostCalls` class that implements all host function methods in-process:
- Each call records to an in-memory event log
- A `replay` mode replays from a saved event log
- Matches the Go host behavior exactly (same return types, same error semantics)
- Used by `test_wasm_compilation.py` to enable testing without WASM

Estimated: ~400 lines.

---

## Phase 2: wasmtime-go for Python (co-existing with wazero)

### Design: Runtime Backend Abstraction

Rather than abstract at the type level (wazero.CompiledModule vs wasmtime.Module),
we abstract at the execution level:

```go
// ExecResult holds the result of a WASM function call.
type ExecResult struct {
    Result        string
    Suspended     bool
    SuspendReason string
}

// WasmBackend executes compiled WASM modules. Each backend
// owns its own compilation, instantiation, and host wiring.
type WasmBackend interface {
    // Execute runs a WASM module with the given entry point and input.
    // The session provides the HostHandler for the execution.
    Execute(ctx context.Context, wasmBytes []byte, entryPoint string, input json.RawMessage, session HostHandler) (*ExecResult, error)

    // Close releases all resources.
    Close(ctx context.Context) error
}
```

The `Engine` holds a map of `WasmBackend` keyed by language target:
```go
type Engine struct {
    // ... existing fields ...
    backends map[string]WasmBackend  // "go" -> wazeroBackend, "python" -> wasmtimeBackend
}
```

Dispatch: detect language from WASM metadata or build-time target, call the right backend.

### Task 2.1: Add language detection to WASM metadata

**File:** `internal/wasm/metadata.go`

Add a `Language` field to the `Metadata` struct. The `cleat.metadata` custom section
already carries `WorkflowName`, `WorkflowVersion`, `ABIVersion`, `PluginDeps`.
Add `Language` and stamp it during `BuildPythonWasm`.

Also read the WASM import section to detect language at runtime:
- If the module imports from `cleat:durable-call/*` (Component Model style) → Python
- If the module imports from `env` with `cleat_*` flat names → Go/Rust/etc.

Estimated: ~40 lines in metadata.go, ~10 lines in build.go.

### Task 2.2: Define WasmBackend interface and refactor Engine

**File:** new file `internal/host/backend.go`

Define the `WasmBackend` interface as above.

**File:** `internal/host/engine.go`

- Add `backends map[string]WasmBackend` to Engine struct
- Modify `Execute()` to detect language and dispatch to the right backend
- Modify `ExecuteCompiled()` — this currently takes `wazero.CompiledModule`. Keep it
  for Go callers but add `ExecuteRaw(wasmBytes, entryPoint, input)` for the general case
- Keep `execSession` and all host function implementations unchanged — they're the
  business logic both backends need

**Critical constraint:** The `execSession` methods take `api.Module` (wazero type)
for memory access. The wasmtime backend needs its own memory access helpers.
Solution: the wasmtime backend wraps wasmtime.Memory with a thin adapter that
matches the subset of `api.Memory` used by execSession, OR we add the memory
operations to the HostHandler interface.

Actually, the cleanest approach: extract memory ops from execSession into the
backend. The execSession should only deal with Go values (strings, ints), not
raw WASM memory pointers. The backend handles all ptr/len encoding.

**Revised HostHandler:** strip `api.Module` from all methods. The backend handles
writing results back to WASM memory.

This is a larger refactor of imports.go but makes the separation clean.

Estimated: ~200 lines for backend.go, ~100 lines changed in engine.go.

### Task 2.3: Create wazeroBackend (wrapping existing Runtime)

**File:** new file `internal/host/backend_wazero.go`

Move existing Runtime methods (CompileModule, InstantiateModule, CallExport, etc.)
into a `wazeroBackend` struct that implements `WasmBackend`. The existing Runtime
becomes the wazeroBackend's internal implementation.

Key points:
- Compilation cache stays in the backend (wazero.CompiledModule)
- Host functions registered via existing `registerHostFunctions` in imports.go
- Keep WASI, TeaVM, and env module setup unchanged
- Handle Go wasip1 `_start` initialization for Go modules

Estimated: ~300 lines (mostly moving existing code from runtime.go).

### Task 2.4: Create wasmtimeBackend for Python

**File:** new file `internal/host/backend_wasmtime.go`

This is the core new code. The wasmtime-go backend:

1. **Engine & Linker setup:**
   - Create a `wasmtime.Engine` (shared, created once)
   - Create a `wasmtime.Linker` for host function registration
   - Define host functions matching the WIT interface signatures

2. **Component Model loading:**
   - Load the component binary directly (no decompose needed!)
   - `wasmtime.NewModule(engine, wasmBytes)` — supports Component Model natively
   - `linker.Instantiate(store, module)` — instantiates with host imports

3. **Host function registration:**
   Instead of manual ptr/len adapters, use wasmtime's typed host functions:
   ```go
   linker.FuncWrap("cleat:durable-call/durable-call", func(
       caller wasmtime.Caller,
       service string, operation string, requestJSON string,
   ) (string, string, error) {
       // Call the existing execSession.DurableCall logic
       // But with typed string parameters, not ptr/len
   })
   ```

4. **Memory management:**
   - wasmtime handles all canonical ABI lifting/lowering
   - No manual memory offsets, no outBufSize, no scratch space
   - String parameters arrive as Go strings, return values go back as Go strings

5. **Export calling:**
   - `instance.GetFunc(store, "run")` — typed export
   - `runFunc.Call(store, inputJSON)` — returns `(string, error)`

6. **Replay safety:**
   - Same execSession (HostHandler) powers both backends
   - Fresh/replay dispatch logic is unchanged — it's in execSession, not the backend
   - No new non-determinism sources introduced by wasmtime

**Key dependencies to add:**
```
require github.com/bytecodealliance/wasmtime-go/v44 v44.0.1
```

This adds CGO dependency (wasmtime ships precompiled .so/.dylib for linux/mac/windows x86_64).

Estimated: ~500 lines for backend_wasmtime.go + host function registration.

### Task 2.5: Wire language dispatch into Engine

**File:** `internal/host/engine.go`

Modify `Execute()`:
```go
func (e *Engine) Execute(ctx context.Context, wasmBytes []byte, entryPoint string, input json.RawMessage) (...) {
    lang := detectLanguage(wasmBytes)
    backend := e.backends[lang]
    if backend == nil {
        // Fall back to wazero for unknown languages
        backend = e.backends["go"]
    }
    // ... create execSession, wrap context ...
    result, err := backend.Execute(ctx, wasmBytes, entryPoint, input, session)
    // ... return ...
}
```

Language detection order:
1. Try to read `cleat.metadata` custom section → `Language` field
2. Fall back: scan import section for Component Model signatures (`cleat:durable-call/*`)
3. Default: wazero backend

**Important for multiple deployments:** If a deployment has NO Python workflows,
the wasmtime backend can be nil and the engine works identically to today.

Estimated: ~80 lines in engine.go.

### Task 2.6: Build pipeline changes

**File:** `internal/wasm/build.go`

The `BuildPythonWasm` function currently shells out to `build_wasm.py` which
runs `componentize-py` then `wasm-tools decompose`. With wasmtime, we can
**skip the decompose step** for the wasmtime path.

However, we still need the decompose step as long as wazero is an option.
Add a `--keep-component` flag to `build_wasm.py` that preserves the original
Component Model binary. The build outputs:
- `workflow.wasm` — decomposed core module (for wazero compatibility, or skip if not needed)
- `workflow.component.wasm` — original Component Model binary (for wasmtime)
- Or just one file with a config flag controlling which to produce

For MVP: produce both. The wazero path uses the decomposed core module; the
wasmtime path uses the component binary. This is wasteful but simple.

Better: add a `--runtime` flag to the build command:
```
cleat build --target python --runtime wasmtime  → produces component binary only
cleat build --target python --runtime wazero    → produces decomposed core module
cleat build --target python                     → produces both (default)
```

Estimated: ~30 lines in build.go, ~20 lines in build_wasm.py.

### Task 2.7: Remove wazero dependency from Python execution path

Once wasmtime is the Python backend, the Python execution path never touches
wazero. Python WASM compilation produces Component Model binaries, wasmtime
loads them directly. The `wasm-tools decompose` step is only needed if someone
explicitly chooses `--runtime wazero` for Python (which we can deprecate).

### Task 2.8: CI and testing

1. Add `wasmtime-go` to `go.mod`
2. Install wasmtime C library in CI (precompiled .so)
3. Update `TestPythonWasmEndToEnd` to:
   - Skip `wasm-tools decompose` when using wasmtime backend
   - Load Component Model binary directly
   - Test both backends: `TestPythonWasmEndToEnd/wazero` and `TestPythonWasmEndToEnd/wasmtime`
4. Add test that verifies Go workflows still work on wazero after wasmtime is added

---

## Subagent Work Breakdown (8 agents)

### Agent 1: Stream R stubs + EventRecord fields (Phase 1, Tasks 1.1 + 1.3)
- Implement SetState/GetState/DeleteState/IncrState/HasState/ListState/RunDetached/Fetch in engine.go
- Add Fetcher interface
- Add EventRecord fields for state/fetch/detached events
- Follow existing fresh/replay dispatch pattern

### Agent 2: Harden build pipeline (Phase 1, Task 1.2)
- Make wasm-tools decompose required (fail instead of warn)
- Add --skip-decompose and --keep-component flags
- Improve error messages

### Agent 3: Python SDK local mode (Phase 1, Task 1.4)
- Create local_host.py with LocalHostCalls
- Event log recording and replay
- Update tests to use it

### Agent 4: WasmBackend interface + Engine refactor (Phase 2, Tasks 2.2 + 2.5)
- Define WasmBackend interface in backend.go
- Refactor Engine to hold backends map
- Add language detection to metadata.go (Task 2.1)
- Wire dispatch in Execute()

### Agent 5: wazeroBackend (Phase 2, Task 2.3)
- Extract existing Runtime code into wazeroBackend implementing WasmBackend
- Keep all existing functionality unchanged
- Ensure Go/Rust/AS workflows continue working

### Agent 6: wasmtimeBackend (Phase 2, Task 2.4)
- Create backend_wasmtime.go
- wasmtime Engine/Linker/Store setup
- Component Model host function registration matching WIT
- Export calling
- Memory management

### Agent 7: Build pipeline + go.mod (Phase 2, Tasks 2.6 + 2.8)
- Add wasmtime-go dependency to go.mod
- Modify BuildPythonWasm for dual output
- Add --runtime flag
- CI configuration

### Agent 8: Integration tests + validation (Phase 2, Task 2.8)
- Update TestPythonWasmEndToEnd for wasmtime
- Add regression test for Go workflows on wazero
- Cross-backend test: both backends produce same event history for same input
- Verify replay determinism on wasmtime backend

---

## Execution Order

Phase 1 tasks can run in parallel (Agents 1, 2, 3).
Phase 2 tasks: Agent 4 first (interface definition), then Agents 5+6 in parallel (backends), then Agent 7 (build), then Agent 8 (tests).

```
Phase 1 (parallel):  Agent 1 ─┬─ Agent 2 ─┬─ Agent 3
                               │           │
Phase 2 (sequential):         │           │
                      Agent 4 (interface + dispatch)
                           │
              ┌────────────┴────────────┐
              │                         │
          Agent 5 (wazero)       Agent 6 (wasmtime)
              │                         │
              └────────────┬────────────┘
                           │
                      Agent 7 (build + deps)
                           │
                      Agent 8 (integration tests)
```

---

## Risks and Mitigations

| Risk | Mitigation |
|------|-----------|
| wasmtime-go CGO breaks cross-compilation | Gate with build tag `//go:build wasmtime` |
| wasmtime precompiled binaries not available for ARM64 | wasmtime supports ARM64; verify precompiled availability or document CGO build |
| Component Model host function signatures differ from current flat ABI | The WIT file is the source of truth for both; match host impls to WIT |
| Two runtimes increase binary size | wasmtime .so is ~15MB; document tradeoff, make optional via build tag |
| execSession uses api.Module for memory access | Refactor: backend handles memory, execSession only uses Go values |

---

## Definition of Done

1. All 8 Stream R functions have correct fresh/replay implementations
2. event_history records include state/fetch/detached events
3. wasm-tools decompose fails with clear error when not installed
4. Python SDK has replay-safe LocalHostCalls for testing
5. Engine dispatches to wazero or wasmtime based on WASM module language
6. Python workflows load via wasmtime (Component Model native, no decompose)
7. Go workflows load via wazero (unchanged)
8. Both backends produce identical event history for equivalent workflows
9. Single `cleat` binary, both runtimes compiled in
10. Existing tests pass, new e2e test covers wasmtime path
