# Issues Found: TypeScript widget-store port to AssemblyScript (Round 2)

## Summary

Porting the DBOS TypeScript widget-store to AssemblyScript via the cleat SDK.
This verifies fixes from Round 1 (Issues #1-7, #13, #18-19, #21-24, #27) and
identifies any remaining gaps.

| Status | Count |
|--------|-------|
| FIXED (Round 1 fixes verified) | 5 |
| PARTIALLY FIXED | 2 |
| STILL BROKEN | 1 |
| NEW (Round 2) | 3 |

---

## 1. FIXED: SDK compiles with AS 0.27.32 (was Critical #1)

**Status: FIXED**

The SDK (`packages/cleat-as/`) now compiles successfully with AssemblyScript 0.27.32.
Both `--runtime stub` and `--runtime minimal` produce valid WASM.

Verified by:
- Compiling `assembly/index.ts` with `--runtime stub`: 11876 bytes WASM
- Compiling with `--target release` (runtime minimal): 13753 bytes WASM
- Compiling the existing `examples/as-workflow/` example: success
- Compiling with the `@cleat/transform`: success (both with and without @durableEntry)

The type errors (String.UTF8.encodeUnsafe signature, usize vs i32 comparisons, @json decorator)
that plagued Round 1 have been fixed in the SDK.

---

## 2. PARTIALLY FIXED: @durableEntry transform handles SUSPEND_SENTINEL (was High #2)

**Status: PARTIALLY FIXED**

The transform now checks `isWorkflowSuspended()` after calling the user function
and returns `SUSPEND_SENTINEL` when set. The generated wrapper code:

```ts
resetWorkflowSuspended();
const _result: string = __durable_inner_myFunc(h, input);
if (isWorkflowSuspended()) {
  return SUSPEND_SENTINEL;
}
Memory.writeString(outPtr, maxOutLen, _result);
return Memory.encodeExportResult(0, _written);
```

Additionally fixed: the transform now imports `SUSPEND_SENTINEL`, `isWorkflowSuspended`,
and `resetWorkflowSuspended` from the SDK (was missing in Round 1).

**Remaining concern**: Only tested with simple @durableEntry functions. Not yet tested
with workflows using `durableSleep` or `awaitSignals` through the decorator. The
transform correctly generates the suspension check, but the interaction with host-call
suspension inside the decorated function needs end-to-end testing.

---

## 3. STILL BROKEN: No try/catch with --runtime stub (was High #3)

**Status: STILL BROKEN**

Both `--runtime stub` and `--runtime minimal` produce:

```
ERROR AS100: Not implemented: Exceptions
```

This is an AssemblyScript 0.27.32 constraint. The WASM exception handling proposal
(`exception-handling`) is not implemented in AS's codegen.

**Impact**: All error handling must use return-value checks (if/else on `.isError`, etc.).
Patterns from the original TypeScript using `try { ... } catch { ... }` must be
rewritten as conditional checks.

**Workaround**: Use the Saga framework (which doesn't require exceptions), or
check `DurableCallOutcome.isError`, etc.

---

## 4. FIXED: JSON parsing available (was High #4)

**Status: FIXED**

The SDK now bundles `assembly/json.ts` providing:
- `JsonParser` class with `parse()`, `getString()`, `getNumber()`, `getArray()`, `getBool()`
- `JsonBuilder` class with `startObject()`, `addString()`, `addNumber()`, `build()`, etc.
- Standalone helpers: `jsonExtractString()`, `jsonExtractNumber()`, `jsonExtractBool()`

All work with `--runtime stub` (no try/catch needed). Verified by compilation test.

**Note**: The SDK's `JsonParser` returns `null` on parse errors (no exceptions).
The `JsonBuilder` handles all state internally through method calls.

---

## 5. FIXED: Existing AS example compiles (was High #5)

**Status: FIXED**

The example at `examples/as-workflow/` compiles successfully:

```
npx asc assembly/index.ts --target release
  --transform ./node_modules/@cleat/transform/index.js
```

Output: `dist/workflow.wasm` (11596 bytes)

The example uses manual ABI exports (not @durableEntry), which was the correct
workaround for Issue #2 in Round 1. Since Issue #2 is now partially fixed, the
example could be updated to use @durableEntry.

---

## 6. PARTIALLY FIXED: Scoped package import resolution (was Medium #6)

**Status: PARTIALLY FIXED**

**Root cause**: AssemblyScript 0.27.32's module resolver doesn't use the `ascMain`
field in `package.json` when resolving scoped packages (`@scope/name`). The resolver
looks for `<package>/index.ts` at the package root, but the SDK's entry point is
at `assembly/index.ts`.

**Fix applied**: Added `packages/cleat-as/index.ts` that re-exports from `./assembly/index`:
```ts
export * from "./assembly/index";
```

After this fix, `import { HostCalls } from "@cleat/sdk"` resolves correctly.
The transform-generated code (`from "@cleat/sdk"`) now works.

**Remaining concern**: The `--path` and `--lib` CLI options in `asconfig.json` still
don't work for scoped packages. The `paths` config option is not processed by the
compiler. Fixed resolution requires either:
  1. The root-level `index.ts` in the SDK (fix applied above)
  2. Relative imports (`from "../../../packages/cleat-as/assembly/index"`)

---

## 7. FIXED: DurableCallOutcome API matches (was Medium #7)

**Status: FIXED**

The existing example at `examples/as-workflow/` uses the current SDK API:
- `DurableCallOutcome` with `.response`, `.error`, `.isError`
- `encodeExportResult(errCode, actualLen)`
- HostCalls methods match SDK implementation

No mismatch between example code and SDK API.

---

## 8. NEW: awaitSignals does not check for host suspension (bit 62)

**Severity: High**

**Detail**: The `HostCalls.awaitSignals()` method does not check bit 62 of the
return value for suspension, unlike `awaitChild()` which does:

```ts
// awaitChild checks for suspension:
if ((result as u64 & (1 << 62)) != 0) {
  setWorkflowSuspended();
  return new DurableResult<string>("", "");
}

// awaitSignals does NOT check:
export function decodeAwaitSignalsResult(result: i64): AwaitSignalsResult {
  // bits 48-63 = sigNameLen (16 bits) -- bit 62 overlaps with this field
  let sigNameLen: u16 = ((r >> 48) & 0xFFFF) as u16;
  // ...
}
```

If the host sets bit 62 to signal "no signal available, please suspend," the
SDK interprets it as a valid signal name length (0x4000 = 16384). This could
cause a read of 16384 bytes from the output buffer, or silently return garbage.

**Impact**: Any workflow using `awaitSignals` with a workflow engine that signals
suspension via bit 62 may behave incorrectly. The workflow may busy-loop, crash,
or read out-of-bounds memory.

**Suggested fix**: Add bit 62 check to `awaitSignals()` and `decodeAwaitSignalsResult()`,
reserving one bit for suspension:

```ts
if ((result as u64 & (1 << 62)) != 0) {
  setWorkflowSuspended();
  return new AwaitSignalsOutcome("", "", false, null);
}
```

---

## 9. NEW: Saga framework uses closures (function references) which may not compile

**Severity: Medium**

**Detail**: The `Saga` class defines `SagaStep` with function reference types:

```ts
public readonly forward: (h: HostCalls) => string,
public readonly compensate: (h: HostCalls) => void | null,
```

Using `Saga.addStep()` requires passing function references. In AssemblyScript,
only top-level exported functions can be used as function references across
module boundaries. Inline anonymous functions may fail to compile.

**Impact**: Ports that attempt to use the Saga with inline arrow functions
(as in the TypeScript original) will fail. Workflow authors must define
separate named functions for each step.

**Workaround**: Define top-level functions for saga steps:

```ts
function reserveStep(h: HostCalls): string { ... }
function reserveCompensate(h: HostCalls): void { ... }
saga.addStep("reserve", reserveStep, reserveCompensate);
```

---

## 10. NEW: WASM export class warnings with --runtime stub

**Severity: Low**

**Detail**: Compiling any file that imports from the SDK with `--runtime stub`
produces numerous AS235 warnings:

```
WARNING AS235: Only variables, functions and enums become WebAssembly module exports.
  in assembly/memory.ts(96,14): export class ExportDecode
  in assembly/memory.ts(104,14): export class CallResult
  ...
```

These are harmless (the classes can be used in AS code even though they can't
be WASM-exported), but they are noisy. With the `--target release` (`--runtime minimal`)
target, these warnings don't appear.

**Suggested fix**: Either suppress these warnings in `asconfig.json` or add
a `@suppress` annotation to the SDK's exported classes.

---

## 11. NEW: No setEvent/getEvent equivalent for external workflow communication

**Severity: Medium**

**Detail**: The TypeScript widget-store uses `DBOS.setEvent()` and `DBOS.getEvent()`
for communication between workflow instances and HTTP handlers:

```ts
// Workflow sets events:
await DBOS.setEvent(PAYMENT_ID_EVENT, DBOS.workflowID);
await DBOS.setEvent(ORDER_ID_EVENT, orderID);

// HTTP handler retrieves events:
const paymentID = await DBOS.getEvent<string | null>(handle.workflowID, PAYMENT_ID_EVENT);
```

The cleat SDK provides `setQueryState()` but no corresponding `getEvent()` or
blocking event delivery for external callers. The HTTP handler must poll or use
a different mechanism to read workflow state.

**Workaround**: Use `setQueryState(key, value)` for one-way state publishing and
have HTTP handlers poll via the cleat API. Or use `createPromise`/`awaitPromise`
for two-way communication.

---

## Issues Status Summary

| # | Description | Status |
|---|-------------|--------|
| 1 | SDK compiles with AS 0.27.32 | FIXED |
| 2 | @durableEntry transform SUSPEND_SENTINEL | PARTIALLY FIXED |
| 3 | try/catch with --runtime stub | STILL BROKEN |
| 4 | JSON parsing | FIXED |
| 5 | Existing AS example compiles | FIXED |
| 6 | Scoped package imports | PARTIALLY FIXED |
| 7 | DurableCallOutcome API match | FIXED |
| 8 | awaitSignals bit 62 suspension check | NEW - UNFIXED |
| 9 | Saga framework function references | NEW - UNFIXED |
| 10 | WASM export class warnings | NEW - LOW |
| 11 | No setEvent/getEvent equivalent | NEW - UNFIXED |
