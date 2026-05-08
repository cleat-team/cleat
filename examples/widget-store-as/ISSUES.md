# Issues Found Porting DBOS Widget-Store to cleat (AS + Java)

## Summary

This document catalogs every problem encountered while porting the DBOS
widget-store TypeScript e-commerce app to cleat's durable execution framework
using the AssemblyScript SDK (primary) and Go host runtime.

## 1. AssemblyScript SDK Compilation Issues

### 1.1 `@cleat/sdk` package fails to compile with AS 0.27.32

**Impact:** Critical. The SDK cannot be imported under `--runtime stub`.

**Root Cause:** The SDK assembly files (`memory.ts`, `host-calls.ts`) have type
errors under AssemblyScript 0.27.32:

1. `String.UTF8.encodeUnsafe()` changed its signature in AS 0.27.32.
   Old: `encodeUnsafe(str: string, ptr: usize, len: usize): void`
   New: `encodeUnsafe(str: usize, len: i32, buf: usize): usize`
   The SDK still uses the old three-argument form with `(str, buf, len)`.

2. `usize` vs `i32` comparisons are type errors in strict mode. The SDK compares
   `usize` and `i32` values without explicit casts (memory.ts line 80,
   host-calls.ts line 392).

3. The `@json` decorator and `JSON.parse<T>()` are not available in AS 0.27.32
   standard library. They were removed from the core AS distribution. The
   `json-as` community package claims to provide them, but it has its own
   compatibility issues (see 1.3).

**Workaround Used:** Created a local `cleat-runtime.ts` file that copies the
relevant SDK code and fixes the type errors for AS 0.27.32.

### 1.2 `--runtime stub` has no try/catch support

**Impact:** High. All error handling must avoid try/catch blocks.

**Details:** With `--runtime stub`, exceptions are not implemented.
`try { ... } catch { ... }` produces `ERROR AS100: Not implemented: Exceptions`.

This means:
- `JSON.parse<T>()` (if it worked) could not be wrapped in try/catch
- Any `throws` in library code will crash the WASM module irrecoverably
- Manual string-based validation is the only safe approach

**Workaround:** Removed all try/catch blocks. Validate JSON fields by checking
field lengths rather than catching parse errors. For input parsing, we assume
well-formed JSON from the host side.

### 1.3 `json-as` package incompatible with AS 0.27.32

**Impact:** High. No JSON deserialization support.

**Root Cause:** `json-as` 1.3.4 (latest) uses `inline.always()`, a function
that does not exist in AS 0.27.32's type definitions. The package's string
deserialization code references `inline.always(...)` which is not exposed by
the current AS standard library.

**Workaround:** Wrote manual JSON field extraction (string indexOf/substring
based pattern matching) instead of typed deserialization.

### 1.4 `@durableEntry` transformer does not handle suspension

**Impact:** High. Any workflow using `durableSleep`, `awaitSignals`, or other
suspending operations cannot use the `@durableEntry` decorator wrappers.

**Details:** The `@cleat/transform` generates ABI wrapper functions that invoke
the user function in a try/catch and always call `JSON.stringify()` on the
result. They do NOT check return values against `SUSPEND_SENTINEL`. When a
workflow calls `h.durableSleep(1000)`, the return value `true` (indicating
suspension needed) is ignored by the wrapper, and the workflow continues
executing past the sleep point.

Compare with the Go SDK, where the WASM adapter panics with
`durable.SuspendSentinel{}` on first execution, and the generated wrapper
catches this and returns the sentinel i64 value.

**Workaround:** Wrote ABI exports manually instead of using `@durableEntry`.
Each export function directly handles the four-argument ABI signature
`(argsPtr, argsLen, outPtr, maxOutLen) => i64` and explicitly returns
`SUSPEND_SENTINEL` when `durableSleep()` returns `true`.

### 1.5 `@scoped/package` import resolution broken in AS 0.27.32

**Impact:** Medium. Scoped npm packages cannot be imported directly.

**Details:** The statement `import { HostCalls } from "@cleat/sdk"` produces
`ERROR TS6054: File '~lib/@cleat/sdk.ts' not found`. The AS compiler does not
resolve scoped package names (`@scope/package`) through the `~lib/` module
resolution mechanism, even when `ascMain: "assembly/index.ts"` is set in the
package.json.

**Workaround:** Use a relative import path from the AS source file to the
SDK assembly directory instead of the scoped package name.

### 1.6 DurableCallOutcome API mismatch between example and SDK

**Impact:** Low. Existing documentation/code is misleading.

**Details:** The existing AS example in `examples/as-workflow/` uses
`reserveResult.ok` and `reserveResult.value` when accessing `DurableCallOutcome`
properties. However, the SDK defines `DurableCallOutcome` with `.response`,
`.error`, and `.isError` (no `.ok`, no `.value`). The `DurableResult<T>` class
has `.value` but is not used by `durableCall()`.

This means the existing example code would NOT compile even if the SDK import
worked, because it references non-existent properties.

### 1.7 No built-in JSON escaping

**Impact:** Low. Error messages with special characters can produce malformed
output.

**Details:** AssemblyScript has no `JSON.stringify()` for arbitrary values and
no string escaping utility. Manual JSON construction via string concatenation
breaks when the value contains double quotes, backslashes, or control
characters. For error messages containing user-provided or host-provided
strings, this is a correctness risk.

## 2. AS Constraints vs. the Original DBOS App

### 2.1 No closures/lambdas

**Impact:** Medium. The original DBOS code uses inline arrow functions for
callbacks. The AS port must use regular named functions or inline code.

**Details:** The `dispatchOrder` loop in the original uses a clean closure-based
pattern. In AS, the loop body must be inline (which is fine for simple cases
but limits abstraction).

### 2.2 No async/await

**Impact:** Low (by design). cleat's workflow execution model is synchronous
with explicit suspension via `durableSleep` returning `true`. Go SDK workflows
use the same pattern via panics; AS workflows must check the boolean return
and return `SUSPEND_SENTINEL`.

### 2.3 Limited string interpolation

**Impact:** Low. `${}` in template strings works with simple identifiers but
not with dotted access or expressions.

**Workaround:** Use explicit string concatenation with `+` operator and
`.toString()` for numeric values.

### 2.4 No `any` type

**Impact:** Low. All types must be explicit, which is good practice anyway.

## 3. Missing cleat Features

### 3.1 No AS-native test framework

**Impact:** Medium. Tests must be written in Go (using `durabletest.TestEnv` or
`localdev.LocalRunner`) rather than directly testing the AS WASM module.

Currently, the only way to test AS workflows is:
1. Compile AS to WASM
2. Load the WASM in a Go WASM runtime (wazero)
3. Wire up HostCalls and drive the workflow from Go

There is no lightweight AS-level testing equivalent of Go's `durabletest`.

### 3.2 No `setEvent` / `getEvent` equivalents

**Impact:** Medium. The original DBOS app uses `setEvent` to communicate
payment details from the workflow to the HTTP handler. cleat provides
`setQueryState` for this, but query state has different semantics (it is not
event-ordered, and reading query state requires polling).

DBOS's event mechanism allows the HTTP handler to `await ctxt.getEvent()` and
block until the workflow sets the event. cleat offers no equivalent of this
workflow-to-external-caller blocking communication.

**Workaround:** Either use `setQueryState` and poll from the HTTP handler, or
use promises (`createPromise` + `awaitPromise`) with an external resolver.

### 3.3 No Saga/compensation framework in AS SDK

**Impact:** Low. The Go SDK has `durable.NewSaga()` with first-class
compensation support, but the AS SDK has no equivalent. Compensation must be
manually coded (checking return values and calling inverse operations).

This is acceptable for simple compensation chains like the widget-store, but
for complex multi-step sagas it would be error-prone.

### 3.4 `localdev.LocalRunner` does not expose query state

**Impact:** Low. The `QueryState` method on `localdev.LocalRunner` does not
provide access to state set via `SetQueryState`, making it difficult to test
query state read-back in local dev mode.

**Workaround:** Use `durabletest.TestEnv.QueryState()` for tests that need to
verify query state.

## 4. Go SDK Issues

### 4.1 No built-in HTTP server for workflow execution

**Impact:** Medium. The cleat project provides the `cleat-worker` CLI tool
for production deployment, but there is no library/package for embedding a
workflow runner in a custom Go HTTP server.

For the widget-store host, we had to:
1. Create a custom `WorkflowRunner` that manages `localdev.LocalRunner`
   instances
2. Create a custom HTTP router to map REST endpoints to workflow operations
3. Manage signal delivery to the right workflow instance manually

A higher-level API for embedding cleat workflows in Go servers would reduce
boilerplate.

### 4.2 durabletest.TestEnv does not support ChildWorkflow stubbing

**Impact:** Low. The `TestEnv.childWorkflowImpl` generates a deterministic
run ID but the test has no way to customize the child workflow's behavior or
result. For workflows that need to test child workflow output processing, the
test must either accept the default stub or work around it.

## 5. Build and Tooling Issues

### 5.1 No `--transform` flag in example build scripts

**Impact:** Low. The existing example's `package.json` build script does not
include `--transform ./node_modules/@cleat/transform/index.js`, meaning the
`@durableEntry` decorators are purely decorative in the built WASM.

### 5.2 Existing AS example does not compile

**Impact:** High for documentation. The canonical AS example at
`examples/as-workflow/` does not compile due to the `@cleat/sdk` import
resolution issue (1.5). This means there is no working AS reference
implementation.

### 5.3 No guidance on multi-export WASM modules

**Impact:** Low. The existing examples only demonstrate single-workflow WASM
modules. The widget-store port needs two exports (`checkoutWorkflow` and
`dispatchOrder`) which works correctly, but there is no official documentation
on multi-workflow WASM modules.

## Summary Table

| Issue | Area | Severity | Workaround |
|-------|------|----------|------------|
| 1.1 SDK type errors | AS SDK | Critical | Local fixed copy of SDK |
| 1.2 No try/catch | AS Runtime | High | Remove try/catch, manual validation |
| 1.3 json-as broken | AS Tooling | High | Manual JSON parsing |
| 1.4 No suspend handling | Transform | High | Manual ABI exports |
| 1.5 Scoped import resolution | AS Tooling | Medium | Relative import paths |
| 1.6 API mismatch | AS Docs | Low | Use correct SDK API |
| 1.7 No JSON escaping | AS SDK | Low | Careful string construction |
| 2.1 No closures | AS Lang | Medium | Named functions |
| 3.1 No AS test framework | Testing | Medium | Test Go equivalents instead |
| 3.2 No setEvent/getEvent | cleat API | Medium | setQueryState + polling |
| 3.3 No Saga in AS SDK | AS SDK | Low | Manual compensation |
| 4.1 No embedded runner | Go SDK | Medium | Custom WorkflowRunner |
| 5.1 Missing transform flag | Build | Low | Add to build script |
| 5.2 Example doesn't compile | Docs | High | N/A - needs SDK fix |
