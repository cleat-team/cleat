# Transformer Implementation Plan

## Goal

Build a tool that reads a Go package containing workflow functions, analyzes the
call graph to find the transitive closure of durable functions, generates WASM
import/export bindings, and compiles the result to a WASM binary ready for
`INSERT INTO workflow_defs (name, version, wasm_bytes)`.

## Prerequisites

- **Go 1.24+** or **tinygo 0.35+** for `//go:wasmimport` / `//go:wasmexport` support.
  Standard Go 1.23's `wasip1` target does not support custom host function imports.
  The plan assumes Go 1.24 for the initial build; tinygo support is added in Phase 10.
- **wazero v1.9+** for loading and testing the compiled WASM modules.
- **PostgreSQL** (optional in early phases; WASM blobs can be written to files
  during development).

If Go 1.24 is unavailable, an escape hatch exists: compile with tinygo instead
(Phase 10), which supports `//go:wasmimport` on older Go toolchains by using
tinygo's own compiler. The workflow source is identical either way.

## Architecture of the compiled output

Before diving into phases, it's worth understanding the target output. Given a
user workflow like:

```go
func PlaceOrder(h *cleat.HostCalls, userID string, cart []CartItem) (string, error) {
    reservation, err := validateAndReserve(h, userID, cart)
    if err != nil {
        return "", err
    }
    // ...
}
```

The transformer produces a WASM module with three layers:

```
┌──────────────────────────────────────────────────┐
│  User's workflow code (unchanged)                │
│  PlaceOrder(), validateAndReserve(), etc.        │
├──────────────────────────────────────────────────┤
│  Generated host adapter (gen_host_adapter.go)    │
│  Converts HostCalls methods → wasmimport calls   │
├──────────────────────────────────────────────────┤
│  Generated WASM exports (gen_wasm_exports.go)    │
│  Entry points the host runtime calls             │
├──────────────────────────────────────────────────┤
│  Generated WASM imports (gen_wasm_imports.go)    │
│  Low-level host function stubs                   │
├──────────────────────────────────────────────────┤
│  Go runtime (minimal for tinygo, ~2MB for std)   │
└──────────────────────────────────────────────────┘
```

Only the bottom three layers are generated. User code is never modified.

---

## Phase 1: Package loading and type resolution (Week 1)

**Goal:** Given a Go package path, load all source files, parse them, type-check
them, and produce a data structure that the rest of the transformer can work with.

### Tasks

1.1 **Set up the transformer project.**
   - Create `cmd/cleat/` CLI skeleton (cobra or plain `flag`).
   - Command: `cleat build ./workflows/` — accepts a package pattern.
   - Use `golang.org/x/tools/go/packages` to load packages in `packages.LoadMode`
     that includes `NeedName`, `NeedFiles`, `NeedCompiledGoFiles`, `NeedTypes`,
     `NeedTypesInfo`, `NeedSyntax`, and `NeedDeps`.

1.2 **Resolve the target package and its dependencies.**
   - Load the package matching the user's pattern.
   - Also load any sub-packages that are part of the same module (to build the
     full call graph across package boundaries).
   - Distinguish between "user packages" (analyzed, subject to restrictions) and
     "external packages" (stdlib, third-party — not analyzed, checked only for
     WASM compatibility).

1.3 **Build internal representation.**
   - `type Package struct { Name, Path string; Files []*ast.File; Types *types.Package; Info *types.Info }`
   - `type FuncDecl struct { Name string; Pkg *Package; Ast *ast.FuncDecl; Type *types.Signature; RecvType types.Type }` (RecvType is non-nil for methods).
   - Walk all `*ast.FuncDecl` and `*ast.FuncLit` in user packages and populate
     a `map[string]*FuncDecl` keyed by fully-qualified name (e.g.,
     `workflows.PlaceOrder`, `workflows.validateAndReserve`).

1.4 **Detect workflow entry points.**
   - A function is an entry point if its first parameter is `*cleat.HostCalls`
     AND it is exported (capitalized name) AND it is in the root of the target
     package. Alternative: use a `//cleat:workflow` comment directive.
   - Report an error if no entry points found.

### Deliverable
A CLI that loads a Go package and prints:
```
$ cleat build ./testdata/basic/
  Package: testdata/basic
  Functions: 12 (3 exported, 9 unexported)
  Entry points: PlaceOrder, CancelOrder, ApprovalWorkflow
  Durable leaves: 5 (reserveInventory, chargeCustomer, createShipment, releaseReservation, sendNotification)
```

### Risks
- `go/packages` can be slow on large codebases. Mitigation: limit to the target
  package and its direct sub-packages, not the entire module.
- Generics: `*types.Signature` has `TypeParams()` and `TypeArgs()`. The call
  graph must handle instantiated generic functions. Start with non-generic
  code only; add generics in Phase 1b if needed.

---

## Phase 2: Call graph construction (Week 2)

**Goal:** Build a directed graph of function calls within the user's packages.
Identify which functions directly call `HostCalls` methods (durable leaves).

### Tasks

2.1 **Build call graph from AST.**
   - Walk every `*ast.CallExpr` in the function bodies of user packages.
   - Resolve the callee using `*types.Info`:
     - Direct calls: `foo(args)` → `Info.Uses[ident]` gives the `*types.Func`.
     - Method calls: `h.DurableCall(args)` → `Info.Selections[sel]` gives the
       method.
     - Interface calls: `reader.Read(args)` → harder; see 2.2.
   - Populate `CallGraph.Calls[caller][callee] = true` and
     `CallGraph.CalledBy[callee][caller] = true`.
   - Also track call sites (the `*ast.CallExpr` node) for error reporting.

2.2 **Handle indirect calls.**
   - **Interface method calls:** The callee's concrete type might not be
     statically known. Strategy: if the interface type is from the user's
     packages, find all concrete types that implement it and consider all of
     them as potential callees. If the interface type is external
     (e.g., `io.Reader`), reject with a clear error: "cannot statically
     resolve interface call to external type io.Reader."
   - **Function values / closures:** `f := someFunc; f(args)` — track the
     assignment via SSA or simple dataflow analysis. For the initial
     implementation: reject function-value calls with a clear error.
     Most workflow code doesn't use higher-order functions.

2.3 **Identify durable leaves.**
   - A function is a durable leaf if it directly calls any of:
     `DurableCall`, `DurableSleep`, `DurableAwaitSignals`, `DurableDefer`,
     `DurableLog`, `DurablePollCancellation`, `DurablePollSignal`,
     `DurableContinueAsNew`, `DurableChildWorkflow`, `DurableAwaitChild`,
     `DurableVersion`, `SetQueryState`.
   - These are methods on `*cleat.HostCalls`. Match by looking for
     `*ast.CallExpr` where the selector resolves to a method of
     `*cleat.HostCalls`.

### Deliverable
```
$ cleat build ./testdata/basic/
  Call graph: 12 functions, 34 edges
  Durable leaves: reserveInventory, chargeCustomer, createShipment, ...
```

### Risks
- Go's type system makes static call resolution imperfect. The transformer must
  be conservative: if it can't determine the callee, reject with a clear error
  rather than silently producing incorrect WASM. This is acceptable because
  workflow code is typically straightforward — sequenced API calls, conditionals,
  loops — not metaprogramming-heavy.
- `*types.Info.Selections` can be nil for unexported methods. Use
  `*types.Info.Uses` as a fallback.

---

## Phase 3: Durable closure computation (Week 2-3, overlaps with Phase 2)

**Goal:** Compute the set of all functions that are in the durable closure
(transitively reach a durable leaf). These functions are subject to WASM
restrictions and must have access to `*HostCalls`.

### Tasks

3.1 **Compute transitive closure.**
   - Start with durable leaves (Phase 2.3).
   - Iterate: for each function in the closure, add all its callers (from
     `CallGraph.CalledBy`). Repeat to fixed point.
   - Functions NOT in the closure are "pure" — no restrictions, no `*HostCalls`
     required, no WASM-specific limitations. They can use any Go feature.

3.2 **Detect unsupported constructs in the durable closure.**
   - Walk the AST of every function in the closure looking for:
     - `go` statements → error: "goroutines are not allowed in durable functions"
     - `<-` channel operations → error: "channels are not allowed"
     - `time.Now()` → error: "use h.Now() instead"
     - `time.Sleep()` → error: "use h.DurableSleep() instead"
     - `math/rand` calls → error: "use h.Random() if randomness is needed"
     - `net/http`, `database/sql`, `os/exec`, `syscall` imports → error
   - Warnings (not errors):
     - `map` iteration with conditionals on key/value → warning: "map iteration
       order is non-deterministic; use sorted slices for deterministic replay"

3.3 **Annotate the call graph with durability.**
   - Each function gets a tag: `DurableLeaf`, `DurableClosure`, or `Pure`.
   - This annotation drives the rest of the pipeline.

### Deliverable
```
$ cleat build ./testdata/basic/
  Durable functions: 8 (2 leaves + 6 closure)
  Pure functions: 4
  Warnings: order.go:42 — map iteration with conditional on key
```

---

## Phase 4: HostCalls threading verification (Week 3)

**Goal:** Verify that every function in the durable closure has access to
`*cleat.HostCalls` through its parameter list or through a caller that
passes it.

### Tasks

4.1 **Track `*HostCalls` flow.**
   - For each function in the durable closure, check whether its first
     parameter is `*cleat.HostCalls`.
   - If yes: the function has direct access. Mark it as "threaded."
   - If no: check each caller. If any caller has `*HostCalls` as a parameter
     AND passes it as an argument to this function, the function has indirect
     access.
   - Start from entry points (which always have `*HostCalls` as first param)
     and work outward through the call graph.

4.2 **Detect `*HostCalls` passed through struct fields.**
   - Track assignments like `orderProcessor.h = h`.
   - Then method calls like `orderProcessor.Process()` have access to `h` via
     the receiver.
   - This requires a simple intra-procedural dataflow analysis. For the
     initial implementation, handle the common patterns:
     - Direct parameter: `func foo(h *cleat.HostCalls, ...)`
     - Struct field: `type S struct { h *cleat.HostCalls }` with
       `func (s *S) Method(...)` where `s.h` is set from a parameter
     - Closure capture: `h := h; func() { h.DurableCall(...) }()` — the
       closure captures `h` from the enclosing scope

4.3 **Report errors with source locations.**
   - Error format: `shipping.go:15: createShipment is in the durable closure
     (it calls DurableCall) but has no access to *cleat.HostCalls. Add
     'h *cleat.HostCalls' as the first parameter or pass it from a caller.`
   - Include the call chain that leads to the error.

### Deliverable
```
$ cleat build ./testdata/threading-error/
  Error: shipping.go:15: createShipment is in the durable closure
         but has no access to *cleat.HostCalls.
         Call chain: PlaceOrder → fulfillOrder → createShipment
         createShipment is missing the HostCalls parameter.
```

---

## Phase 5: WASM import generation (Week 4)

**Goal:** Generate `//go:wasmimport` stubs for each host function that the
durable closure actually uses. These stubs are low-level functions that the
generated host adapter calls.

### Tasks

5.1 **Determine which host functions are used.**
   - Scan the durable closure for calls to each `HostCalls` method.
   - Only generate imports for methods that are actually called. If a workflow
     never calls `DurableSleep`, omit the `cleat_sleep` import.
   - This produces smaller WASM binaries and fewer host functions to register.

5.2 **Generate import stubs.**
   - Write `gen_wasm_imports.go` with a `//go:build wasip1` constraint.
   - Each import has the signature:
     ```go
     //go:wasmimport env cleat_call
     func cleatCallImport(
         svcPtr unsafe.Pointer, svcLen uint32,
         opPtr unsafe.Pointer, opLen uint32,
         reqPtr unsafe.Pointer, reqLen uint32,
     ) (respPtr unsafe.Pointer, respLen uint32, errCode uint32)
     ```
   - String arguments are passed as `(pointer, length)` pairs. The pointer
     is an offset into WASM linear memory. The host reads the bytes from that
     offset.
   - Return values are also `(pointer, length)` pairs. The host writes the
     response into WASM linear memory and returns the offset and length.

5.3 **Generate linear memory utilities.**
   - Write `gen_wasm_memory.go` with `//go:build wasip1`.
   - Provide `readString(ptr unsafe.Pointer, len uint32) string` and
     `writeString(dst unsafe.Pointer, maxLen uint32, s string) uint32`.
   - The workflow uses these to convert between Go strings and WASM memory
     pointers. The memory allocator is Go's standard allocator (the WASM
     module owns its linear memory; Go's runtime manages it).

### Deliverable
A `gen_wasm_imports.go` file that imports exactly the host functions used by
the durable closure. A `gen_wasm_memory.go` file with string conversion utilities.

### Risks
- `unsafe.Pointer` usage is fragile. The memory layout must match between the
  generated imports and the host adapter. Write a conformance test that
  round-trips strings through WASM memory.
- Go 1.24's `//go:wasmimport` ABI is still evolving. Pin the Go version and
  test with a specific wazero version.

---

## Phase 6: Host adapter generation (Week 5-6)

**Goal:** Generate the adapter that bridges the user's clean `HostCalls`
interface to the low-level `//go:wasmimport` calls. This is the glue between
"write normal Go" and "cross the WASM boundary."

### Tasks

6.1 **Generate the adapter struct.**
   - Write `gen_host_adapter.go` with `//go:build wasip1`.
   - The adapter is a function `makeHostCalls(mem *wasmMemory) *cleat.HostCalls`
     that returns a `HostCalls` struct populated with closures. Each closure
     calls the corresponding `//go:wasmimport` function.

6.2 **Implement each HostCalls method.**
   - `DurableCall(service, operation, request) → (response, error)`:
     1. Allocate string in WASM memory: `svcPtr, svcLen := mem.AllocString(service)`
     2. Call import: `respPtr, respLen, errCode := cleatCallImport(svcPtr, svcLen, ...)`
     3. If errCode != 0: read error string from memory, return error
     4. Read response string from memory, return response
   - `DurableSleep(durationMs)`:
     1. Call `cleatSleepImport(durationMs)`. The host handles the actual
        sleep by releasing the workflow and setting `next_wake_at`.
   - `DurableAwaitSignals(signalNames, timeoutMs) → (signalName, payload, timedOut, error)`:
     1. JSON-serialize signalNames, allocate in WASM memory.
     2. Call `cleatAwaitSignalsImport(...)`.
     3. The host suspends the workflow. On resume (signal arrived or timeout),
        the import returns.
   - `DurableLog(message)`:
     1. Allocate message in WASM memory, call `cleatLogImport(...)`.
   - `Now() int64`:
     1. Call `cleatNowImport()`. The host provides the current wall-clock
        time in milliseconds since epoch.
   - (And similarly for DurableDefer, DurablePollCancellation, etc.)

6.3 **Implement the wasmMemory allocator.**
   - The WASM module owns its linear memory. The `wasmMemory` type provides:
     - `AllocString(s string) (ptr unsafe.Pointer, len uint32)` — allocates
       memory in the WASM heap and copies the string.
     - `ReadString(ptr unsafe.Pointer, len uint32) string` — reads a string
       from WASM memory.
   - For tinygo: use `malloc`/`free` from the C standard library (available
     in WASM targets).
   - For standard Go: Go's WASM runtime provides a heap. `unsafe.Slice` can
     read from it directly.

### Deliverable
A `gen_host_adapter.go` file that, when compiled with the user's workflow,
produces a `HostCalls` implementation backed by WASM host imports.

### Risks
- The allocator is the trickiest part. If the WASM module and host disagree
  on memory layout, you get silent corruption. Mitigation: write a
  round-trip test that allocates a string, writes it, reads it back, and
  compares.
- Go's WASM target may change the allocator interface between versions.
  Pin the Go version and the wazero version.

---

## Phase 7: WASM export generation (Week 6-7, overlaps with Phase 6)

**Goal:** Generate `//go:wasmexport` functions for each workflow entry point.
These are the functions the host runtime calls to start or resume a workflow.

### Tasks

7.1 **Generate one export per entry point.**
   - For each entry point function (Phase 1.4), generate:
     ```go
     //go:wasmexport place_order
     func placeOrder(argsPtr unsafe.Pointer, argsLen uint32) (resultPtr unsafe.Pointer, resultLen uint32, errCode uint32) {
         mem := getMemory()
         h := makeHostCalls(mem)
         argsJSON := mem.ReadString(argsPtr, argsLen)
         var args struct { UserID string; Cart []CartItem }
         json.Unmarshal([]byte(argsJSON), &args)
         result, err := workflows.PlaceOrder(h, args.UserID, args.Cart)
         if err != nil {
             return mem.WriteError(err)
         }
         return mem.WriteJSON(result)
     }
     ```
   - The export name is derived from the function name (snake_case).
   - The export deserializes the input from JSON (passed by the host as a
     byte array in WASM memory), calls the user's function, and serializes
     the result back to JSON.

7.2 **Generate query exports (if needed).**
   - For query functions (Section 8.2), generate exports that the host can
     call to read workflow state. These are read-only — no durable side
     effects.
   - Queries are optional. If no query functions are defined, skip.

7.3 **Handle the defer runner.**
   - If the durable closure uses `DurableDefer`, the export must set up the
     defer execution mechanism:
     ```go
     h.DeferRunner = newDeferRunner(mem)
     defer h.DeferRunner.executeAll(err != nil)
     ```
   - The `DeferRunner` is generated code that manages the LIFO defer stack
     and calls back into the exported `execute_defer(deferID)` function when
     the host requests defer execution.

### Deliverable
A `gen_wasm_exports.go` file with one `//go:wasmexport` function per entry point,
plus optional query exports and defer runner setup.

---

## Phase 8: Compilation pipeline (Week 8)

**Goal:** Tie everything together into a single command that produces a WASM
binary from the user's Go source.

### Tasks

8.1 **Assemble generated files with user source.**
   - The transformer writes generated files (`gen_*.go`) into a temporary
     directory alongside a copy of the user's source.
   - The combined package is the compilation unit.
   - The transformer verifies that the combined package is self-consistent
     (no duplicate definitions, no missing imports).

8.2 **Compile with Go 1.24+.**
   - Use `go build` with `GOOS=wasip1 GOARCH=wasm`:
     ```
     GOOS=wasip1 GOARCH=wasm go build -o place_order.wasm ./tmp_build_dir/
     ```
   - The output is a single WASM binary.
   - Validate the binary:
     - Check that it exports the expected functions (`place_order`, etc.)
     - Check that it imports the expected host functions (`cleat_call`, etc.)
     - Check that the binary size is reasonable (< 5MB for standard Go,
       < 500KB for tinygo)

8.3 **Run a smoke test.**
   - Load the compiled WASM in a minimal wazero host.
   - Call the entry point with a known input.
   - Verify that the host intercepts the `cleat_call` import.
   - Verify that the workflow produces the expected output.
   - This is the same pattern as the WASM boundary demo in `cmd/host/`.

8.4 **CLI polish.**
   - `cleat build ./workflows/` — compiles and writes `.wasm` to the
     current directory.
   - `cleat build ./workflows/ -o /path/to/output.wasm` — explicit output.
   - `cleat build ./workflows/ --target tinygo` — use tinygo instead of
     standard Go.
   - `cleat build ./workflows/ --json` — machine-readable output with
     diagnostics (call graph, durable closure, entry points, warnings).

### Deliverable
A working `cleat build` command that produces a WASM binary from Go source.

---

## Phase 9: Validation rules and clear error messages (Week 9)

**Goal:** Make the transformer give excellent error messages for unsupported
Go constructs. The error message quality is a key part of the developer
experience — it's the difference between "write Go with restrictions" and
"fight the compiler."

### Tasks

9.1 **Categorize all restrictions with error codes.**
   - `E001`: Goroutine in durable function
   - `E002`: Channel operation in durable function
   - `E003`: `time.Now()` in durable function (use `h.Now()`)
   - `E004`: `time.Sleep()` in durable function (use `h.DurableSleep()`)
   - `E005`: Direct `net/http` call in durable function
   - `E006`: Direct `database/sql` call in durable function
   - `E007`: `math/rand` in durable function (use `h.Random()`)
   - `E008`: Unresolvable function call (interface/virtual)
   - `E009`: Function value call (closures as values)
   - `E010`: `*HostCalls` not threaded to durable function
   - `E011`: Import of WASM-incompatible package
   - `W001`: Map iteration with order-dependent control flow
   - `W002`: Floating-point used in control flow condition

9.2 **Each error includes:**
   - Error code (for searching documentation)
   - Source location (file:line)
   - The unsupported construct
   - The suggested fix
   - The call chain that leads to the error (for threading errors)

9.3 **Implement `cleat vet`.**
   - A read-only analysis command that checks for violations without
     generating code or compiling. Suitable for CI, pre-commit hooks,
     and editor integration.
   - `cleat vet ./workflows/` — runs only Phases 1-4 (load, call graph,
     closure, threading). No WASM compilation.
   - Output format: machine-readable (JSON) + human-readable (colored
     terminal output).

### Deliverable
Comprehensive error checking with clear, actionable messages. The `cleat vet`
command for CI integration.

### Example output
```
$ cleat vet ./workflows/
  workflows/order.go:42: E001: goroutine in durable function
    → goroutines are not allowed in durable functions.
    → Use child workflows (h.DurableChildWorkflow) for parallelism.
  workflows/order.go:88: E003: time.Now() in durable function
    → use h.Now() instead for deterministic time.
  workflows/shipping.go:15: E010: createShipment is in the durable closure
    but has no access to *cleat.HostCalls.
    → call chain: PlaceOrder → fulfillOrder → createShipment
    → add 'h *cleat.HostCalls' as the first parameter.
  3 errors, 0 warnings
```

---

## Phase 10: Tinygo support (Week 10)

**Goal:** Add support for compiling with tinygo for smaller WASM binaries
(~50-200KB vs ~2-3MB for standard Go).

### Tasks

10.1 **Detect tinygo installation.**
   - `cleat build --target tinygo` checks for `tinygo` in `$PATH`.
   - If not found, print installation instructions.

10.2 **Adjust generated code for tinygo.**
   - Tinygo's `//go:wasmimport` uses the same syntax as Go 1.24, so generated
     imports are identical.
   - Tinygo's `malloc`/`free` are from the C standard library. The
     `wasmMemory` allocator uses `//go:linkname` or tinygo's built-in
     allocator.
   - Tinygo's `encoding/json` is a custom implementation that may behave
     differently from standard Go's. Test round-trip serialization.

10.3 **Invoke tinygo build.**
   - `tinygo build -target=wasip1 -o output.wasm ./tmp_build_dir/`
   - Validate the binary (same checks as Phase 8.2).

### Deliverable
`cleat build --target tinygo` produces a ~50-200KB WASM binary.

---

## Phase 11: Module storage and deployment (Week 11)

**Goal:** Wire the transformer output into the database. `cleat deploy`
INSERTs the WASM blob into `workflow_defs`.

### Tasks

11.1 **`cleat deploy` command.**
   - Reads the WASM binary produced by `cleat build`.
   - Connects to PostgreSQL (configured via `CLEAT_DATABASE_URL` env var or
     `--db` flag).
   - `INSERT INTO workflow_defs (namespace, name, version, wasm_bytes) VALUES ($1, $2, $3, $4)`
   - Auto-increments the version number (SELECT MAX(version) + 1).
   - Records metadata: call graph, entry points, durable leaves, dependencies
     (services/operations used), generated at timestamp, Go version, transformer
     version.
   - `--namespace` flag for multi-tenant deployments.
   - `--rollout-percent` flag to set the initial rollout percentage for canary
     deployments (Section 10.1a).

11.2 **`cleat versions` command.**
   - Lists all versions of a workflow: `cleat versions PlaceOrder`
   - Shows: version, created_at, rollout_percent, deprecated, active instances
   - `cleat versions PlaceOrder --deprecate v3` — marks a version as deprecated.
   - `cleat versions PlaceOrder --rollout v5=10%` — adjusts rollout percentage.

11.3 **`cleat rollback` command.**
   - `cleat rollback PlaceOrder --to v4` — sets v4's rollout_percent to 100%,
     sets v5's to 0%, marks v5 as deprecated.
   - This is the operational rollback — it's an UPDATE, not a deployment.

### Deliverable
End-to-end: `cleat build && cleat deploy` puts a WASM blob in the database,
ready for workers to load and execute.

---

## Phase 12: Testing the transformer (Week 12)

**Goal:** A test suite that verifies the transformer produces correct WASM
for a variety of workflow patterns.

### Tasks

12.1 **Test fixture repository.**
   - Create `testdata/` directory with example workflows:
     - `basic/` — simple linear workflow (4 durable calls)
     - `branching/` — if/else branches with different API calls
     - `loops/` — for-range loops with durable calls
     - `compensation/` — error handling with compensation calls
     - `nested/` — deeply nested function calls (3+ levels)
     - `struct_methods/` — HostCalls passed through struct fields
     - `signals/` — DurableAwaitSignals usage
     - `child/` — child workflow spawning
     - `errors/` — invalid workflows (goroutines, channels, missing HostCalls)
     - `generics/` — generic functions in durable closure

12.2 **Transformer output tests.**
   - For each fixture: verify the transformer correctly identifies entry
     points, durable leaves, and the durable closure.
   - For error fixtures: verify the transformer produces the expected error
     codes and messages.

12.3 **Compiled WASM conformance tests.**
   - For valid fixtures: compile to WASM, load in wazero, execute with a
     test host that records all `cleat_call` invocations.
   - Verify the sequence of calls matches expectations.
   - Verify replay determinism: run twice with the same input, compare
     the sequence of calls — must be identical.
   - Verify replay from partial history: seed the test host with a partial
     event history, verify the workflow replays cached steps and executes
     fresh steps.
   - Verify divergence detection: seed with a history from a different
     input, verify the host detects divergence.

12.4 **Performance benchmarks.**
   - Compilation time: how long does `cleat build` take for a 50-function
     workflow? Target: < 30 seconds for standard Go, < 10 seconds for tinygo.
   - WASM binary size: standard Go vs. tinygo for a representative workflow.
   - Execution time: how long does a 10-step workflow take to execute fresh?
     To replay from cache? Compare to the simulated host in `host/main.go`.
   - Memory: how much RAM does wazero use per workflow instance?

### Deliverable
A test suite with >80% code coverage on the transformer, conformance tests
for common workflow patterns, and baseline performance numbers.

---

## Phase 13: Defer execution engine (Week 13-14, post-MVP)

**Goal:** Implement the full `DurableDefer` mechanism as designed in Section
8.6b, including the host callback for defer execution.

### Tasks

13.1 **Defer registration.**
   - When the workflow calls `h.DurableDefer(fn)`, the host adapter:
     1. Assigns a unique `deferID`.
     2. Stores the closure in a side table (in WASM memory).
     3. Calls `cleatDeferRegister(deferID, description)` — an import that
        records `defer_registered` in the event history.

13.2 **Defer execution on exit.**
   - When the workflow function returns, the WASM export runner checks if
     any defers are registered.
   - If yes: enters defer execution mode.
   - For each defer in LIFO order:
     1. Records `defer_executing` in event history.
     2. Calls the exported `execute_defer(deferID)` function.
     3. The deferred closure runs; its `DurableCall`/`DurableLog` calls are
        handled normally by the host.
     4. When the closure returns, records `defer_executed`.
   - This requires the host to be in a mode where it accepts `DurableCall`
     from a defer context.

13.3 **Edge cases.**
   - Grace period expiry: host kills WASM runtime, records
     `cancellation_forced`.
   - Worker crash mid-defer: on replay, host sees which defers were
     registered, which were executed, resumes from the first unexecuted.
   - Nested defers: defer inside defer is allowed, pushed onto the stack.

### Risks
- This is the most complex phase. The host must call back into WASM to
  execute deferred functions, which means the WASM module must be in a
  callable state (not mid-DurableCall). This requires careful state
  management in the host.
- Mitigation: defer the defer engine if it's blocking the overall
  transformer MVP. Compensation via explicit error handling (the current
  demo approach) is sufficient for the initial release.

---

## Overall timeline

| Phase | What | Weeks | Depends on |
|---|---|---|---|
| 1 | Package loading + type resolution | 1 | — |
| 2 | Call graph construction | 1 | 1 |
| 3 | Durable closure computation | 0.5 | 2 |
| 4 | HostCalls threading verification | 1 | 2, 3 |
| 5 | WASM import generation | 1 | 3, 4 |
| 6 | Host adapter generation | 1.5 | 5 |
| 7 | WASM export generation | 1 | 3, 6 |
| 8 | Compilation pipeline + CLI | 1 | 5, 6, 7 |
| 9 | Validation rules + error messages | 1 | 3, 4 |
| 10 | Tinygo support | 1 | 8 |
| 11 | Module storage + deployment | 1 | 8 |
| 12 | Testing + benchmarks | 1 | 8, 9 |
| 13 | Defer execution engine | 1.5 | 7 (optional) |
| **Total** | | **~13 weeks** | |

Phases 2-4 can overlap partially. Phases 5-7 can run in parallel once the
durable closure is computed. Phase 13 is optional for MVP.

## Critical path

The critical path is: 1 → 2 → 3 → 5 → 6 → 7 → 8 → 12 = ~9 weeks to a
working end-to-end build (Go source → WASM binary → wazero execution).

Phases 9 (validation), 10 (tinygo), 11 (deployment), and 13 (defer engine)
are not on the critical path.

## Milestones

| Week | Milestone |
|---|---|
| 1 | `cleat build` loads a package and lists entry points |
| 2 | Call graph shows durable leaves and their callers |
| 3 | Durable closure computed; threading violations flagged |
| 4 | WASM imports generated for a simple workflow |
| 6 | Host adapter bridges HostCalls to WASM imports |
| 7 | WASM exports generated; end-to-end compilation works |
| 8 | `cleat build` produces a valid WASM binary |
| 9 | `cleat vet` catches all known error patterns |
| 10 | `cleat build --target tinygo` works |
| 11 | `cleat deploy` INSERTs into PostgreSQL |
| 12 | Test suite passes; conformance tests verify replay determinism |
| 14 | (Optional) Defer execution engine works |

## What "done" looks like

A developer writes:

```go
//cleat:workflow
func PlaceOrder(h *cleat.HostCalls, userID string, cart []CartItem) (string, error) {
    reservation, err := validateAndReserve(h, userID, cart)
    if err != nil {
        return "", err
    }
    charge, err := processPayment(h, userID, reservation.TotalCents)
    if err != nil {
        releaseReservation(h, reservation.ReservationID)
        return "", fmt.Errorf("payment failed: %w", err)
    }
    trackingID, err := fulfillOrder(h, reservation, charge)
    if err != nil {
        refundPayment(h, charge.ChargeID)
        releaseReservation(h, reservation.ReservationID)
        return "", fmt.Errorf("fulfillment failed: %w", err)
    }
    return trackingID, nil
}
```

They run:

```
$ cleat build ./workflows/
  Analyzing package workflows...
  Found 12 functions, 1 entry point, 8 in durable closure.
  Durable leaves: catalogLookup, reserveInventory, chargeCustomer,
                   createShipment, releaseReservation, refundPayment
  Verifying HostCalls threading... OK
  Generating WASM imports (6 host functions used)... OK
  Generating host adapter... OK
  Generating WASM exports (1 entry point)... OK
  Compiling WASM module (standard Go)... OK
  Wrote place_order.wasm (2.3 MB)

$ cleat deploy ./place_order.wasm
  Deployed PlaceOrder v7 (2.3 MB, rollout 100%)
  → INSERT INTO workflow_defs (namespace, name, version, wasm_bytes) VALUES (...)

$ cleat versions PlaceOrder
  VERSION  ROLLOUT  ACTIVE  CREATED
  v7       100%     0       2026-05-15 14:32 UTC
  v6       0%       12      2026-05-08 09:15 UTC
  v5       0%       0       2026-05-01 11:00 UTC (deprecated)
```

And the WASM binary, loaded by a wazero worker, executes the workflow
correctly — replaying from event history, detecting divergence, recording
new events, and checkpointing progress.
