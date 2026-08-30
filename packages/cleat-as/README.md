# `@cleat/sdk` -- AssemblyScript SDK for cleat durable workflows

AssemblyScript SDK providing ABI-compatible bindings for writing cleat durable
workflows that compile to WebAssembly.  Re-exports from `assembly/index.ts`:

- `memory.ts` -- Memory layout constants, string I/O, bit-packing decoders
- `host-calls.ts` -- Raw `@external` import declarations and the `HostCalls` class
- `cleat-entry.ts` -- `@cleatEntry` marker decorator for workflow entry points
- `plugins.ts` -- Typed convenience wrappers for all 8 plugins (18 functions)

## Installation

```bash
npm install @cleat/sdk
```

Or from source:

```bash
cd packages/cleat-as/
npm link
```

The SDK also provides a transform plugin that generates ABI-compatible WASM
export wrappers from `@cleatEntry`-decorated functions:

```bash
npm install @cleat/transform
```

See the [Import resolution](#scoped-package-import-resolution-in-as-02732) section for
AssemblyScript compiler configuration.

## Quick start

Define a workflow by importing `HostCalls` and decorating your entry-point
function with `@cleatEntry`. The transform plugin generates the WASM export
wrapper automatically.

```ts
import { HostCalls, cleatEntry } from "@cleat/sdk";

@cleatEntry
export function helloWorkflow(h: HostCalls, name: string): string {
    h.cleatLog("Hello workflow started for " + name);
    let resp = h.cleatCall("greeter", "Greet",
        '{"name": "' + name + '"}');
    h.cleatLog("Got response: " + resp);
    return resp;
}
```

The `@cleatEntry` decorator triggers the cleat transform plugin, which
generates an ABI-compatible wrapper
`(argsPtr: usize, argsLen: i32, outPtr: usize, maxOutLen: i32) => i64`.
On replay, completed calls return cached results instead of re-executing.

Compile the workflow with the AS compiler and the transform plugin:

```bash
npx asc assembly/index.ts --target release \
    --transform ./node_modules/@cleat/transform/index.js \
    -o dist/workflow.wasm
```

## HostCalls overview

The `HostCalls` class wraps all WASM host function imports, grouped by category:

### Workflow Identity
- `currentWorkflowId(): string` -- the current workflow's unique ID
- `currentRunId(): string` -- the current run's unique ID

### Time & Random
- `now(): i64` -- wall-clock time in ms since epoch
- `random(): i64` -- deterministic random value (same on replay)
- `version(): i32` -- workflow definition version

### Durable Execution
- `cleatCall(service: string, operation: string, request: string): CleatCallOutcome` -- recorded API call
- `cleatCallWithTimeout(service: string, operation: string, request: string, timeoutMs: i64): CleatCallOutcome` -- call with timeout
- `cleatSleep(durationMs: i64): void` -- suspend for a duration (survives restarts)
- `log(message: string): void` -- emit a log message
- `cleatFetch(url: string, method: string, headers: string, body: string): string` -- durable HTTP fetch via host
- `cleatSend(service: string, operation: string, request: string): void` -- fire-and-forget (no response)

### Signals & Events
- `awaitSignals(signalNames: string[], timeoutMs: i64): SignalResult` -- wait for external signals
- `pollSignal(name: string): string` -- non-blocking signal check
- `pollCancellation(): string` -- check for cancellation

### Child Workflows
- `childWorkflow(name: string, input: string): string` -- start child workflow, returns run ID
- `awaitChild(runId: string): string` -- await a single child, returns result JSON
- `awaitAllChildren(runIds: string[]): string` -- await multiple children concurrently

### State & Promises
- `setQueryState(key: string, value: string): void` -- set externally queryable state
- `createPromise(name: string): string` -- create a durable promise, returns promise ID
- `awaitPromise(promiseId: string, timeoutMs: i64): PromiseResult` -- await a promise with timeout
- `resolvePromise(promiseId: string, value: string): void` -- resolve a promise

### Plugin Calls
- `pluginCall(pluginName: string, functionName: string, input: string): CleatCallOutcome` -- call a host plugin function

All methods are deterministic and replayed from event history on subsequent
executions. Side effects (calls, sleeps, logs) are recorded in the workflow
event history.

## Unit Differences (Cleat vs. Other Frameworks)

Cleat uses **milliseconds** for all time-related host calls. This is important when porting workflows from other frameworks:

| Framework | Sleep Unit | Example |
|-----------|-----------|---------|
| **Cleat** | milliseconds | `h.cleatSleep(5000)` = 5 seconds |
| **Temporal** | Go: `time.Duration`, Java: `Duration` | `sleep(Duration.ofSeconds(5))` |
| **DBOS** | seconds | `DBOS.sleepSeconds(5)` |
| **Restate** | SDK-dependent | `Duration.ofSeconds(5)` |

Cleat's `cleatSleep(durationMs)` always takes **milliseconds**, consistent with the WASM host ABI which uses `i64` milliseconds for all timing operations.

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
communicated via return values (`CleatCallOutcome.isError`,
`DurableResult.isError`, etc.).  Check these instead of using try/catch.

### Template literal limitations

Template literals (`` `...${expr}...` ``) are **not available in all AS versions**.
AS 0.27.x supports them, but older versions or alternative AS toolchains may not.
For maximum portability, use explicit string concatenation with `.toString()` on
non-string values:

```ts
// Portable (all AS versions):
let msg = "user " + userID + " has " + count.toString() + " items";

// AS 0.27+ only:
let msg = `user ${userID} has ${count} items`;
```

For complex string building, use `StringBuilder` from `utils.ts`:

```ts
import { StringBuilder } from "./utils";

let sb = new StringBuilder();
sb.append("user ");
sb.append(userID);
sb.append(" has ");
sb.append(count.toString());
sb.append(" items");
let msg = sb.toString();
```

`StringBuilder` avoids repeated WASM string concatenation allocations and works
with `--runtime stub` on all AS versions.

### No built-in JSON module

AssemblyScript 0.27.32 removed the standard library JSON module. The cleat SDK
provides a replacement at `json.ts` that works with `--runtime stub`:

```ts
import { JsonParser, JsonBuilder, jsonExtractString } from "./json";

// Parsing:
let parser = new JsonParser();
let val = parser.parse(input);
if (val !== null) {
    let name = parser.getString(val, "name");
}

// Building:
let builder = new JsonBuilder();
builder.startObject();
builder.addString("name", "Alice");
builder.addNumber("age", 30);
let json = builder.build();

// Quick field extraction (no full parse):
let name = jsonExtractString(input, "name");
```

The `JsonParser` does not use `try`/`catch` and is safe for `--runtime stub`.
See `assembly/json.ts` for the full API reference.

### No `async`/`await`

Cleat workflows use a synchronous-but-suspendable execution model.  Instead of
async/await, call blocking host functions (`cleatCall`, `cleatSleep`, etc.)
directly.  The host suspends and resumes the WASM instance transparently.

### String concatenation and memory churn in WASM

Every `+`-based string concatenation in AssemblyScript (e.g. `a + b + c`)
allocates a **new string** for each intermediate result:

```ts
// BAD -- three allocations, high memory churn:
let msg = "user " + userID + " has " + count.toString() + " items";
// Allocates: "user ", then "user u123", then "user u123 has ",
// then "user u123 has 5", then "user u123 has 5 items"
```

Each allocation grows WASM linear memory (via `memory.grow`) that is never
returned to the system.  In workflows with long replay histories or many
string operations, this can lead to significant memory growth over time.

**Safe pattern** -- use `StringBuilder` from `utils.ts`:

```ts
import { StringBuilder } from "./utils";

let sb = new StringBuilder();
sb.append("user ");
sb.append(userID);
sb.append(" has ");
sb.append(count.toString());
sb.append(" items");
let msg = sb.toString();
```

`StringBuilder` writes characters into a single pre-allocated buffer and
produces exactly one string allocation at the end.  This reduces memory
churn to O(1) allocations per build operation, regardless of how many
parts are concatenated.

For short, fixed concatenations (2-3 parts), inline `+` is acceptable.
For loops, building JSON, or multi-part messages, always use
`StringBuilder`.

#### Memory layout

The SDK defines three key memory regions used by all host calls:

| Region        | Address    | Size     | Purpose                              |
|---------------|------------|----------|--------------------------------------|
| `SCRATCH_BASE`  | `0xA00000` | variable | Input argument packing               |
| `OUTPUT_OFFSET` | `0xA10000` | 65536    | Output buffer for host responses     |
| `OUT_BUF_SIZE`  | —          | 65536    | Max bytes readable from output buffer|

These constants are in `memory.ts`.  All input strings are packed into the
scratch region starting at `SCRATCH_BASE` before each host call.  The host
writes results to `OUTPUT_OFFSET`.  The scratch region is overwritten on
every host call, so save any needed data before making the next call.

### Host service contract pattern

Workflows communicate with external systems via the **host service** pattern.
Services are registered on the host runtime (not in the WASM module) and
invoked through `cleatCall(service, operation, requestJSON)`:

```ts
// service is the logical service name, operation is the specific action
let resp = h.cleatCall("payment", "charge", paymentJSON);
let result = h.cleatCall("inventory", "reserve", itemsJSON);
```

#### Service registration

Services are registered on the host side (Go runtime or embedded runner).
They are **not** defined in the WASM module.  The workflow only sees the
service name, operation, and JSON request/response contract:

```
WASM workflow                  host runtime                    service
    |                              |                             |
    |--- cleatCall("inventory",  |                             |
    |    "reserve", requestJSON) -->|                             |
    |                              |--- HTTP/RPC/gRPC ---------->|
    |                              |<--- response JSON ----------|
    |<-- response JSON -----------|                             |
```

#### JSON request/response contract

All service calls use JSON strings for both request and response.  There is
no schema enforcement at the ABI level -- the contract is defined by the
service implementation:

```ts
// Example: inventory reservation contract
// Request:  {"sku":"s-42", "quantity":2}
// Response: {"reservation_id":"res-abc", "status":"confirmed"}
// Error:    {"error":"insufficient_stock", "available":0}

function reserveItem(h: HostCalls, sku: string, qty: i32): string {
    let req = "{"
        + "\"sku\":\"" + sku + "\","
        + "\"quantity\":" + qty.toString()
        + "}";
    let resp = h.cleatCall("inventory", "reserve", req);
    return resp; // caller parses the response
}
```

#### Error handling conventions

Services communicate errors through the `CleatCallOutcome` response:

```ts
let outcome = h.cleatCall("payment", "charge", requestJSON);
if (outcome.isError) {
    // The error is retryable if the host says so.
    // Check outcome.errCode for structured classification.
    h.log("payment failed: " + outcome.error + " (code=" + outcome.errCode.toString() + ")");
    return "{\"error\": \"payment failed\"}";
}
let response = outcome.response;
```

Per-call timeouts are available via the `timeoutMs` parameter on
`cleatCall()`:
```ts
// Abort after 5 seconds:
let outcome = h.cleatCall("slow-service", "query", requestJSON, 5000);
```

When the timeout is exceeded, the host returns an error with a timeout
error code.  The workflow should check `outcome.isError` and handle the
timeout (e.g., fall back to a cached value or retry).

#### Testing services with TestEnv

When using `durabletest.TestEnv`, register mock handlers for the services
your workflow calls.  This avoids needing a real service during unit tests:

```go
env := durabletest.NewTestEnv(t, "path/to/workflow.wasm")
env.RegisterService("payment", func(operation, requestJSON string) string {
    switch operation {
    case "charge":
        return `{"status":"ok","txn_id":"tx-123"}`
    default:
        return `{"error":"unknown operation"}`
    }
})
result := env.Call("place_order", `{"userID":"u1","amount":500}`)
```

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

A single AssemblyScript source file can define multiple workflow functions.
Each function becomes a named WASM export, and the host dispatches incoming
invocations by export name.

### Manual ABI exports

Each exported function must conform to the Cleat ABI signature:

```ts
// ABI export signature: (argsPtr: usize, argsLen: i32, outPtr: usize, maxOutLen: i32) => i64
export function placeOrder(argsPtr: usize, argsLen: i32, outPtr: usize, maxOutLen: i32): i64 {
  let h = new HostCalls();
  // ... parse input, call services, write output ...
  return encodeExportResult(0 /* success */, written);
}

export function cancelOrder(argsPtr: usize, argsLen: i32, outPtr: usize, maxOutLen: i32): i64 {
  let h = new HostCalls();
  // ... cancellation logic ...
  return encodeExportResult(0, written);
}
```

Each exported function becomes a named WASM export. The host dispatches
incoming workflow invocations by matching the export name to the workflow name.

### `@cleatEntry` decorator pattern

Alternatively, use the `@cleatEntry` decorator with the cleat-as transformer
plugin (requires the `--transform` flag in your `asc` command):

```ts
// assembly/index.ts
import { HostCalls, cleatEntry } from "@cleat/sdk";

@cleatEntry
export function placeOrder(h: HostCalls, input: string): string {
  // ... workflow body ...
  return result;
}

@cleatEntry
export function cancelOrder(h: HostCalls, input: string): string {
  // ... cancellation body ...
  return result;
}

@cleatEntry
export function getOrderStatus(h: HostCalls, input: string): string {
  // ... query logic ...
  return result;
}
```

The decorator eliminates the need to write boilerplate ABI export wrappers
manually. The transform plugin processes the decorated functions and generates
the ABI-compatible wrapper code.

### How the transform generates ABI wrappers

When `asc` runs with `--transform ./node_modules/@cleat/transform/index.js`, the
cleat transform plugin performs the following steps for each function decorated
with `@cleatEntry`:

1. **Parsing** -- The transform parses the AssemblyScript AST to find all exported
   functions annotated with `@cleatEntry`.

2. **Wrapper generation** -- For each decorated function, the transform generates
   an ABI export wrapper with the correct signature. The wrapper:
   - Reads the input arguments from WASM linear memory using `readString(argsPtr, argsLen)`
   - Calls the user's workflow function with a fresh `HostCalls` instance
   - Serialises the return value to JSON and writes it to the output buffer via `writeString(outPtr, maxOutLen, ...)`
   - Returns a bit-packed `i64` result code with the number of bytes written

3. **Export naming** -- The wrapper is exported under the decorated function's
   name. The original decorated function is renamed internally (prefixed with
   `__durable_impl_`) to avoid conflicts.

4. **Suspension handling** -- The wrapper wraps the function call in a
   `catch_unwind`-like mechanism. If the workflow suspends (via
   `cleatSleep`/`cleatCall` on a first execution), the wrapper returns
   the `SUSPEND_SENTINEL` value to the host runtime.

For the transform to run, your `asc` command must include the `--transform` flag:

```bash
npx asc assembly/index.ts --target release \
  --transform ./node_modules/@cleat/transform/index.js
```

### WASM export naming conventions

- **Manual exports**: The exported name is exactly the function name as written
  in the source: `placeOrder`, `cancelOrder`, etc.

- **Decorator exports**: The transform exports the wrapper under the decorated
  function's name. The internal implementation function is renamed to
  `__durable_impl_<functionName>`.

- **Name collision prevention**: If two decorated functions share the same name
  (across different files in the same compilation unit), the transform treats
  this as an error at compile time.

The host runtime resolves workflow invocations by matching the called workflow
name to the WASM export name:

| Host action | WASM export invoked |
|-------------|---------------------|
| Start `placeOrder` workflow | `exports.placeOrder(argsPtr, argsLen, outPtr, maxOutLen)` |
| Start `cancelOrder` workflow | `exports.cancelOrder(argsPtr, argsLen, outPtr, maxOutLen)` |
| Signal workflow | Handled by the host runtime, forwarded to the workflow's signal handlers |

### Example: multi-export workflow compiled with the transformer

Build command:

```bash
npx asc assembly/index.ts --target release \
  --transform ./node_modules/@cleat/transform/index.js \
  -o dist/workflow.wasm
```

Inspect the resulting exports:

```bash
wasm-objdump -x dist/workflow.wasm | grep Export
# Expected output:
#  Export[3]:
#   - func[0] <placeOrder> -> "placeOrder"
#   - func[1] <cancelOrder> -> "cancelOrder"
#   - func[2] <getOrderStatus> -> "getOrderStatus"
```

Each export corresponds to a workflow entry point that the Cleat runtime
can invoke by name.

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

### Scoped package import resolution in AS 0.27.32

In AssemblyScript 0.27.32, scoped package imports (`@cleat/sdk`) may **not
resolve** through normal `node_modules` traversal due to changes in the
compiler's module resolution. If you encounter `ERROR: Can not resolve
'@cleat/sdk'`, use relative imports instead:

```ts
// BAD -- may not resolve in AS 0.27.32:
import { HostCalls } from "@cleat/sdk";
import { Saga } from "@cleat/sdk";

// GOOD -- relative imports work in all AS versions:
import { HostCalls } from "./host-calls";
import { Saga } from "./saga";
import { JsonBuilder } from "./json";
import { StringBuilder } from "./utils";
```

The SDK source files are located under `assembly/` in the `@cleat/sdk` package.
All public types are available through their respective module files. Use the
relative paths shown above for maximum compatibility.

## Per-call timeout limitations

Per-call timeouts are **under development** and not yet enforced on the host
side during WASM execution. The `cleatCall` host import does not
accept a timeout parameter.

**Workaround:** Use `cleatSleep` + polling for timeout-aware patterns:

```ts
import { HostCalls } from "./host-calls";

function callWithTimeout(
    h: HostCalls,
    service: string,
    operation: string,
    requestJSON: string,
    timeoutMs: i64,
): string {
    let deadline: i64 = h.now() + timeoutMs;
    let lastError: string = "";

    while (h.now() < deadline) {
        let result = h.cleatCall(service, operation, requestJSON);
        if (!result.isError) {
            return result.response;
        }
        lastError = result.error;
        h.cleatSleep(1000); // poll interval
    }

    throw new Error("timed out: " + lastError);
}
```

Host-side timeout enforcement is on the roadmap.

## Why `package.json` pins `glob-promise` in `overrides`

`@as-pect/cli@8` depends on `glob-promise@^5`, which depends on
`npm-install-peers`, which depends on **the entire npm 6 CLI** as a library.
That one edge dragged 457 packages into this lock file — 84% of it — and every
vulnerability `npm audit` reported lived inside that vendored copy:

    before: 544 packages, 48 vulnerabilities (3 critical, 28 high, 17 moderate)
    after:   73 packages,  9 vulnerabilities (1 high, 8 moderate)

`glob-promise@6` has no dependencies at all and declares the same
`glob: ^8.0.3` peer as v5, so the override changes nothing about what the
package does. `npm-install-peers` existed to install peer dependencies on npm
6; npm 7+ does that itself.

**Do not drop this override** because a dependabot PR touches these packages.
Removing it restores the npm 6 subtree and all 48 findings. Re-derive with:

    cd packages/cleat-as && npm audit --package-lock-only

The 9 that remain are `lodash` inside `chevrotain` inside `@as-pect/snapshots`
inside `@as-pect/core@8`. Clearing them needs `@as-pect/core@9`, which requires
`assemblyscript@^0.28.19` against the `^0.27.32` pinned here — a compiler
upgrade, not a dependency bump. That is why dependabot #417, #423 and #424 all
failed `ERESOLVE`: each bumped `@as-pect/cli` to 9 and left `assemblyscript`
where it was.
