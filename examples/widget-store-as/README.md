# Widget Store (AssemblyScript)

An AssemblyScript port of the DBOS widget-store e-commerce checkout workflow, demonstrating durable orchestration and signal-based payment handling.

## What it shows

- Direct ABI exports for WASM entry points (no `@durableEntry` decorator)
- Multi-step checkout: subtract inventory, create order, await payment signal
- Compensation on failure: undo inventory on order/payment failure
- `HostCalls.awaitSignals` with timeout for payment webhook
- `HostCalls.childWorkflow` for dispatching delivery tracking
- `HostCalls.durableSleep` for delivery progress polling
- `HostCalls.setQueryState` for exposing payment info externally
- Manual JSON parsing (AS 0.27.32 with `--runtime stub` has no `JSON.parse<T>()`)

## Build

```bash
cd examples/widget-store-as
npm run build
```

## Run

```bash
cleat deploy widget-store-as dist/workflow.wasm
cleat run checkoutWorkflow '{"product":"widget","quantity":2}'
```

## Key files

- `assembly/workflows.ts` — workflow entry points (`checkoutWorkflow`, `dispatchOrder`)
- `assembly/cleat-runtime.ts` — local fixed copy of the SDK runtime for AS 0.27.32
- `package.json` — build scripts with `@cleat/transform`
- `ISSUES.md` — documented AssemblyScript SDK compatibility issues
