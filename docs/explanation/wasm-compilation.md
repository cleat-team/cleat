# WASM Compilation

Workflows are compiled to WebAssembly through a five-stage transformer pipeline
in the `cleat build` command. This document describes each stage, the
compilation targets, and the WASM host interface.

## Transformer Pipeline

The pipeline is implemented in `internal/analyzer/`, `internal/callgraph/`,
`internal/closure/`, `internal/transform/`, and `internal/wasm/`.

```
Source Go Package
     |
     v
+-------------------+
| 1. analyzer.Load  |  Load Go packages via go/packages, parse AST,
|                   |  resolve types, identify exported functions as
|                   |  entry points.
+-------------------+
     |
     v
+-------------------+
| 2. callgraph.Build |  Build a static call graph of the target package
|                   |  using the callgraph analysis.
+-------------------+
     |
     v
+-------------------+
| 3. closure.Compute |  Compute the cleat closure: the set of functions
|                   |  reachable from entry points that make HostCalls.
|                   |  Validate that every path passes HostCalls correctly.
+-------------------+
     |
     v
+-------------------+
| 4. transform      |  Rewrite source files:
|                   |  - Add HostCalls params (auto-threading)
|                   |  - Insert import statements
|                   |  - Generate WASM export wrappers
|                   |  - Rewrite package to "main"
+-------------------+
     |
     v
+-------------------+
| 5. wasm.Compile   |  Generate WASM import declarations, host adapter
|                   |  code, and compile to wasip1 binary.
|                   |  Supports "go" and "tinygo" targets.
+-------------------+
     |
     v
   .wasm file
```

### Stage 1: analyzer.Load

**Package**: `internal/analyzer/`

Uses the `go/packages` driver to load the target Go package. For each file:

- Parses the AST to identify exported functions (capitalized names).
- Resolves types and determines which functions accept `cleat.HostCalls` as
  their first parameter.
- Produces a `PackageInfo` struct with:
  - `EntryPoints` -- exported functions that are workflow entry points.
  - `SideEffectFree` -- functions that do not call HostCalls (safe to skip).
  - `ImportPaths` -- import dependencies for the transformed output.

### Stage 2: callgraph.Build

**Package**: `internal/callgraph/`

Builds a static call graph starting from the identified entry points. Uses the
Go callgraph analysis package (`golang.org/x/tools/go/callgraph`). Produces a
directed graph with:

- Nodes: each function in the package.
- Edges: call sites from caller to callee.

The call graph includes:
- Direct function calls (both exported and unexported).
- Method calls on concrete types.
- Calls through function literals and closures (where statically resolvable).

It does NOT include:
- Reflection-based calls.
- Interface dispatch (dynamic calls are flagged as potential call graph gaps).
- Cgo calls.

### Stage 3: closure.Compute

**Package**: `internal/closure/`

Computes the transitive closure of functions reachable from entry points that
make HostCalls. This is the "cleat closure" -- the set of functions that must
be transformed to thread `HostCalls` through.

Validation checks:

1. Every path through the closure must pass `HostCalls` correctly -- if a
   function in the closure cannot reach `HostCalls` through its parameters, the
   pipeline reports an error.
2. Functions outside the closure that are called FROM the closure are flagged
   for auto-threading.
3. Functions that never make HostCalls and are never called FROM a HostCalls
   path are excluded from transformation.

### Stage 4: transform

**Package**: `internal/transform/`

Rewrites source files with AST transformations:

- **Auto-threading**: Adds `h cleat.HostCalls` as the first parameter to
  functions that need it, based on the closure analysis. The parameter is
  threaded through from caller to callee automatically.
- **Import injection**: Adds `cleat` import to files that need it.
- **Export wrappers**: Generates WASM export functions for each entry point.
- **Package rewrite**: Rewrites `package <name>` to `package main` (WASM
  modules require a `main` package).
- **Output buffer setup**: Inserts code to set up the linear memory scratch
  region for string passing.

### Stage 5: wasm.Compile

**Package**: `internal/wasm/`

Assembles the build directory and compiles:

1. **Pre-build**: Copies transformed source files, writes generated files
   (`gen_wasm_imports.go`, `gen_wasm_memory.go`, `gen_host_adapter.go`,
   `gen_wasm_exports.go`, `gen_main_stub.go`), creates a `go.mod` with a
   replace directive pointing to the project root.

2. **Generated files**:

   | File | Purpose |
   |------|---------|
   | `gen_wasm_imports.go` | WASM import declarations for all 15 host functions |
   | `gen_wasm_memory.go` | Memory buffer setup for string passing |
   | `gen_host_adapter.go` | Adapter code that bridges Go types to WASM i64 values |
   | `gen_wasm_exports.go` | Named WASM exports for each entry point |
   | `gen_main_stub.go` | `main()` that blocks forever (`select{}` for Go,
   `<-make(chan struct{})` for TinyGo). The `--target go` stub does not
   require a `.deps/` shim. |

3. **Compilation**:

   ```bash
   # Standard Go target
   GOOS=wasip1 GOARCH=wasm go build -o output.wasm .

   # TinyGo target (smaller binaries, ~60% size reduction)
   tinygo build -o output.wasm -target=wasi .
   ```

## Auto-Threading

The transformer's most important function is propagating `HostCalls` through
the call graph automatically. Consider this workflow code:

```go
func PlaceOrder(h cleat.HostCalls, input string) error {
    return processOrder(h, input)  // h is explicitly passed
}

func processOrder(h cleat.HostCalls, input string) error {
    // The transformer adds h cleat.HostCalls as the first parameter
    // and threads it through to called cleat leaves.
    return checkInventory(h, input)
}

func checkInventory(h cleat.HostCalls, input string) error {
    h.DurableCall("inventory", "Check", input)
    return nil
}
```

The transformer detects that `checkInventory` makes a `DurableCall` but does
not receive `HostCalls`. It adds the parameter and propagates it upward
through the call chain. The workflow author only needs to pass `HostCalls` from
the entry point -- the transformer handles the rest.

### How It Works

1. The closure analysis identifies all functions that call `HostCalls` methods
   (directly or transitively).
2. Functions that need `HostCalls` but don't have it get a new first parameter.
3. Callers of those functions get their calls updated to pass `HostCalls`.
4. The process is repeated until closure: functions that call functions that
   need `HostCalls` also get the parameter.

## Host Import Interface

The WASM module imports 15+ functions from the `env` module. These are
registered by the host runtime (`internal/host/runtime.go`) on the wazero
"env" host module.

### Import Declarations (from the WASM side)

```go
//go:wasmimport env cleat_call
//go:wasmimport env cleat_sleep
//go:wasmimport env cleat_now
//go:wasmimport env cleat_random
//go:wasmimport env cleat_log
//go:wasmimport env cleat_call_heartbeat
//go:wasmimport env cleat_call_retry
//go:wasmimport env cleat_version
//go:wasmimport env cleat_min_version
//go:wasmimport env cleat_defer
//go:wasmimport env cleat_poll_cancellation
//go:wasmimport env cleat_poll_signal
//go:wasmimport env cleat_continue_as_new
//go:wasmimport env cleat_child_workflow
//go:wasmimport env cleat_await_child
//go:wasmimport env cleat_await_signals
//go:wasmimport env cleat_set_query_state
//go:wasmimport env cleat_plugin_call
//go:wasmimport env cleat_create_promise
//go:wasmimport env cleat_await_promise
//go:wasmimport env cleat_register_update_handler
```

### Host Handler Interface (from the host side)

Each import maps to a method on `internal/host.HostHandler`:

```go
type HostHandler interface {
    DurableCall(ctx context.Context, m api.Module, service, operation,
        requestJSON string, responsePtr, responseMaxLen uint32) int64
    DurableSleep(ctx context.Context, m api.Module, durationMs int64) int64
    DurableAwaitSignals(ctx context.Context, m api.Module, signalNames string,
        timeoutMs int64, sigNamePtr, sigNameMaxLen, payloadPtr,
        payloadMaxLen uint32) int64
    DurableDefer(ctx context.Context, m api.Module, description string,
        deferIDPtr, deferIDMaxLen uint32) int64
    DurableLog(ctx context.Context, m api.Module, message string) int64
    PollCancellation(ctx context.Context, m api.Module,
        reasonPtr, reasonMaxLen uint32) int64
    PollSignal(ctx context.Context, m api.Module, signalName string,
        payloadPtr, payloadMaxLen uint32) int64
    ContinueAsNew(ctx context.Context, m api.Module,
        newInputJSON string) int64
    ChildWorkflow(ctx context.Context, m api.Module, name, inputJSON string,
        runIDPtr, runIDMaxLen uint32) int64
    AwaitChild(ctx context.Context, m api.Module, runID string,
        resultPtr, resultMaxLen uint32) int64
    AwaitAllChildren(ctx context.Context, m api.Module, runIDsJSON string,
        resultsPtr, resultsMaxLen uint32) int64
    DurableCallWithRetry(ctx context.Context, m api.Module,
        service, operation, requestJSON string,
        maxAttempts, initialIntervalMs, backoffCoefficient100x,
        maxIntervalMs int64, nonRetryableErrorsJSON string,
        responsePtr, responseMaxLen uint32) int64
    DurableCallWithHeartbeat(ctx context.Context, m api.Module,
        service, operation, requestJSON string,
        heartbeatIntervalMs int64,
        responsePtr, responseMaxLen uint32) int64
    Version(ctx context.Context) int64
    MinVersion(ctx context.Context) int64
    SetQueryState(ctx context.Context, m api.Module,
        key, value string) int64
    Now(ctx context.Context) int64
    Random(ctx context.Context) int64
    CreatePromise(ctx context.Context, m api.Module, name string,
        promiseIDPtr, promiseIDMaxLen uint32) int64
    AwaitPromise(ctx context.Context, m api.Module, promiseID string,
        timeoutMs int64, resultPtr, resultMaxLen uint32) int64
    PluginCall(ctx context.Context, m api.Module,
        pluginName, functionName, inputJSON string,
        responsePtr, responseMaxLen uint32) int64
    PluginCallStreaming(ctx context.Context, m api.Module,
        pluginName, functionName, inputJSON string,
        responsePtr, responseMaxLen uint32) int64
    RegisterUpdateHandler(ctx context.Context, m api.Module,
        name string) int64
    SendSignalAndWait(ctx context.Context, m api.Module,
        targetRunID, signalName, payload string, timeoutMs int64,
        responsePtr, responseMaxLen uint32) int64
}
```

### Return Value Encoding

Host functions return a packed `int64` that communicates both result and
error status:

- **Upper 32 bits**: actual length of data written to output buffer.
- **Lower 32 bits**: error code (0 = success, 1 = error).
- **Bit 62**: suspend sentinel (1 = workflow should suspend).

The encoding functions in `internal/plugin/host_helpers.go` handle this:

```go
EncodeOK()                    // errCode=0, len=0
EncodeOKWithLen(len uint32)   // errCode=0, len=actualLen
EncodeError(err)              // errCode=1
EncodeSuspend()               // suspend sentinel
```

## String Passing Protocol

Strings cross the WASM boundary through a pointer+length protocol using the
module's linear memory:

### Scratch Region

```
Linear Memory Layout:
[0 ... 10MB offset ... scratch region ... scratch+64KB]
                     |<--- output buffer (64KB default) -->|
```

### Passing Strings Into WASM

1. The host writes string data to linear memory at a known scratch offset.
2. The host passes `(ptr, len)` pairs as `uint32` parameters to the WASM
   function.
3. The WASM module reads string data from `memory[ptr:ptr+len]`.

### Reading Strings From WASM

1. The WASM module writes response data to the output buffer (starting at the
   scratch region).
2. The host passes `(responsePtr, responseMaxLen)` as `uint32` parameters.
3. After the call, the host reads from `memory[responsePtr:responsePtr+actualLen]`
   where `actualLen` is packed in the return value.

This protocol avoids WASM reference type overhead and keeps the import/export
interface to scalar types only (i32, i64).

## Compilation Targets

### Standard Go (`--target go`)

- Uses `GOOS=wasip1 GOARCH=wasm` (bundled with Go 1.22+) — fully implemented.
- Produces larger binaries (~2-5 MB for typical workflows) but full Go runtime
  and standard library support.
- No TinyGo required; uses the standard `go build` toolchain.
- `main()` blocks with `select{}` to keep the WASM instance alive.

### TinyGo (`--target tinygo`)

- Uses `tinygo build -target=wasi`.
- The default target is `go` (standard Go); use `--target tinygo` explicitly for
  smaller binaries.
- Produces smaller binaries (~60% size reduction over standard Go).
- Limited to Go 1.24 compatibility (TinyGo 0.36-0.37 constraint).
- Use `--target go` for full standard library support when binary size is not a
  concern.
- `main()` blocks on `<-make(chan struct{})` (TinyGo's asyncify scheduler
  handles exports while main is blocked).
- Requires a dependency shim in `.deps/` with an older `go.mod` for
  compatibility.

### Rust (`--target rust`)

- Uses the `cleat-sdk` crate with `#[cleat_entry]` proc-macro.
- Entry points are annotated with the macro instead of accepting `HostCalls`.
- Compiles to WASM via `cargo build --target wasm32-wasi`.
- See [plugin-developer-guide.md](../../docs/plugin-developer-guide.md).

## WASI Support

WASI preview 1 (`wasi_snapshot_preview1`) is instantiated alongside the `env`
module in the wazero runtime. WASI is required by Go `wasip1` modules for:

- Goroutine scheduling and stack management.
- `os.Stdout`/`os.Stderr` output capture.
- `time.Sleep` within the WASM module (though workflows should use
  `HostCalls.DurableSleep` for durable timers).

## Version Compatibility

Each WASM module is stored with a `version` and optional `min_version` in
`workflow_defs`. The worker loads the exact version requested by the workflow
instance, ensuring deterministic replay across worker upgrades. WASM modules
are cached in memory (keyed by `def_name:def_version`) to avoid repeated
database loads.
