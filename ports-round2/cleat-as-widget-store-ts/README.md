# Cleat AssemblyScript Widget Store (TypeScript port)

Port of the DBOS TypeScript widget-store application to AssemblyScript via the cleat durable execution SDK.

## Source Project

**TypeScript source**: `/localssd/rcownie/cleat-agent1/ports-round2/dbos-demo-apps/typescript/widget-store/`

The original uses DBOS transaction/workflow primitives:
- `DBOS.registerWorkflow()` for workflow registration
- `DBOS.recv()` for signal-based event waiting
- `DBOS.setEvent()` / `DBOS.getEvent()` for external communication
- `DBOS.startWorkflow()` for child workflow dispatch
- `DBOS.sleep()` for timer-based delays

## Port Architecture

The AS port maps DBOS primitives to cleat equivalents:

| DBOS Primitive | Cleat Equivalent |
|----------------|-------------------|
| `DBOS.registerWorkflow()` | Manual ABI export |
| Transaction (Knex) | `durableCall()` to service |
| `DBOS.recv(topic, timeout)` | `awaitSignals(namesJson, timeoutMs)` |
| `DBOS.setEvent(key, value)` | `setQueryState(key, value)` |
| `DBOS.startWorkflow(func)` | `childWorkflow(name, inputJson)` |
| `DBOS.sleep(duration)` | `durableSleep(durationMs)` |
| try/catch error handling | `.isError` + conditional compensation |

## Files

```
cleat-as-widget-store-ts/
  package.json          -- Dependencies (AS 0.27.32, @cleat/sdk, @cleat/transform)
  asconfig.json         -- AS compiler config (--runtime minimal or --runtime stub)
  ISSUES.md             -- Findings from porting
  assembly/
    index.ts            -- Main workflows: checkout_workflow, dispatch_order, cancel_order
    tsconfig.json       -- AS TypeScript config
```

## Workflows

### checkout_workflow
Main checkout saga with compensation:
1. Reserve inventory via durableCall
2. Create order via durableCall
3. Wait for payment signal (120s timeout)
4. On payment received: mark paid, start child dispatch workflow
5. On payment failure: cancel order, release inventory

### dispatch_order
Child workflow for dispatch simulation:
1. Loop 10 iterations: sleep 1s, update progress via durableCall
2. Mark order as dispatched

### cancel_order
Cancellation-aware variant that checks pollCancellation() before and after each step.

## Building

```bash
# With --runtime stub (proven to work):
npx asc assembly/index.ts --runtime stub --optimize --initialMemory 170 -o dist/workflow.wasm

# With --target release (runtime minimal, no AS235 warnings):
npx asc assembly/index.ts --target release -o dist/workflow.wasm

# With @cleat/transform (for @durableEntry decorator):
npx asc assembly/index.ts --target release --transform ./node_modules/@cleat/transform/index.js
```

## Key Constraints (AS subset)

- No try/catch -- error handling via return value checks
- No JSON.parse/stringify -- use SDK's JsonParser/JsonBuilder or manual field extraction
- No closures -- use named top-level functions
- No `any` type -- explicit typing required
- Manual ABI exports `(argsPtr: usize, argsLen: i32, outPtr: usize, maxOutLen: i32) -> i64`
- JSON construction via string concatenation
- Scoped imports resolved via root-level index.ts or relative paths

## SDK Reference

The cleat AssemblyScript SDK is at `packages/cleat-as/`. Key APIs:

- `HostCalls` -- main class for all durable operations
- `DurableCallOutcome` -- result of durableCall (response, error, isError)
- `AwaitSignalsOutcome` -- result of awaitSignals (signalName, payload, timedOut)
- `DurableResult<T>` -- generic result wrapper (value, error, isError)
- `CancellationStatus` -- cancellation polling result
- `Saga` -- structured compensation framework
- `Memory` / `readString` / `writeString` / `encodeExportResult` -- ABI I/O
- `JsonParser` / `JsonBuilder` / `jsonExtractString` -- JSON utilities
- `SUSPEND_SENTINEL` / `isWorkflowSuspended()` -- suspension detection
