# `@cleat/sdk` -- AssemblyScript SDK for cleat durable workflows

AssemblyScript SDK providing ABI-compatible bindings for writing cleat durable
workflows that compile to WebAssembly.  Re-exports from `assembly/index.ts`:

- `memory.ts` -- Memory layout constants, string I/O, bit-packing decoders
- `host-calls.ts` -- Raw `@external` import declarations and the `HostCalls` class
- `durable-entry.ts` -- `@durableEntry` marker decorator for workflow entry points
- `plugins.ts` -- Typed convenience wrappers for all 8 plugins (18 functions)

## AssemblyScript constraints

The `--runtime stub` flag (required for cleat workflows) disables the full AS
runtime.  This imposes several constraints on your workflow source code:

### No closures or lambdas

Arrow functions (`() => {}`) and closures capture their environment via heap
allocation, which the stub runtime does not support.  Use named top-level
functions instead:

```ts
// BAD -- arrow function:
let items = arr.map<CartItem>((x) => x);

// GOOD -- named function:
function transformItem(x: CartItem): CartItem { return x; }
let items = arr.map<CartItem>(transformItem);
```

### No `any` type

The stub runtime does not include the dynamic type checks needed for `any`.
Use explicit, concrete types everywhere.

### No `try`/`catch`

The stub runtime has no exception handling.  All errors from host functions are
communicated via return values (`DurableCallOutcome.isError`,
`DurableResult.isError`, etc.).  Check these instead of using try/catch.

### Limited string interpolation

Template literals (`` `...${expr}...` ``) compile successfully in AS 0.27, but
for maximum compatibility with stub runtime constraints, use explicit string
concatenation with `.toString()` on non-string values:

```ts
// OK (compiles):
let msg = "user " + userID + " has " + count.toString() + " items";

// Also OK (AS 0.27 supports template literals):
let msg = `user ${userID} has ${count} items`;
```

### No `async`/`await`

Cleat workflows use a synchronous-but-suspendable execution model.  Instead of
async/await, call blocking host functions (`durableCall`, `durableSleep`, etc.)
directly.  The host suspends and resumes the WASM instance transparently.

## Testing

Compile your workflow to WASM with `asc` (as in the example build scripts),
then load the `.wasm` binary into the Go test harness:

1. **Unit testing with `durabletest.TestEnv`** -- In Go, create a `TestEnv`
   that wraps your compiled WASM module.  Call your exported workflow functions
   with input JSON and assert the response JSON.

2. **Integration testing with `localdev.LocalRunner`** -- For multi-workflow
   scenarios, start a `LocalRunner` that manages the full cleat runtime
   (durable storage, signal/query channels, child workflows).  Drive the
   workflow lifecycle from Go test code.

Example Go test skeleton:

```go
import (
    "testing"
    "github.com/cleat/cleat/durabletest"
)

func TestPlaceOrder(t *testing.T) {
    env := durabletest.NewTestEnv(t, "path/to/workflow.wasm")
    result := env.Call("place_order", `{"userID":"u1","items":[{"sku":"s1","quantity":2}]}`)
    if result.ErrCode != 0 {
        t.Fatalf("workflow failed: %s", result.Error)
    }
}
```

## Multi-export: defining multiple workflow entry points

A single AS source file can define multiple workflow functions.  Each function
must be `export`ed with the cleat ABI signature:

```ts
// ABI export signature: (argsPtr, argsLen, outPtr, maxOutLen) => i64
export function placeOrder(argsPtr: usize, argsLen: i32, outPtr: usize, maxOutLen: i32): i64 { ... }
export function cancelOrder(argsPtr: usize, argsLen: i32, outPtr: usize, maxOutLen: i32): i64 { ... }
```

Each exported function becomes a named WASM export.  The host dispatches
incoming workflow invocations by export name.

Alternatively, use the `@durableEntry` decorator with the cleat-as-transformer
plugin (requires `--transform` in your asc command).  The transformer renames
the decorated function and generates an ABI-compatible wrapper for it.

## Import resolution

When using scoped package imports like `import { HostCalls } from "@cleat/sdk"`,
the AS compiler resolves the package via `node_modules` using the `ascMain`
field in `@cleat/sdk/package.json`.  For local development (e.g., with
`file:` links in package.json), you may need to add a `paths` mapping in
`asconfig.json` so the compiler can find the assembly sources:

```json
{
  "options": {
    "paths": {
      "@cleat/sdk": "../../packages/cleat-as/assembly"
    }
  }
}
```

This is only needed when the scoped package is linked via `file:` protocol
and the compiler cannot resolve it through normal `node_modules` traversal.
