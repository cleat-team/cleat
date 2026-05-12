# AssemblyScript Workflow

Demonstrates a durable order processing workflow written in AssemblyScript and compiled to WASM with the `@cleatEntry` decorator.

## What it shows

- `@cleatEntry` decorator for marking WASM-exported workflow entry points
- Saga-like compensation: reserve inventory, charge payment, create shipment, notify
- Manual compensation on failure (release inventory on payment failure, refund + release on shipping failure)
- Manual JSON parsing without `JSON.parse<T>()` (AS 0.27.32 with `--runtime stub`)
- `HostCalls.cleatCall` for durable service invocations
- `HostCalls.pollCancellation` for cancellation-aware workflows

## Build

```bash
cd examples/as-workflow
npm run build
```

## Run

```bash
cleat deploy as-workflow dist/workflow.stamped.wasm
cleat run PlaceOrder '{"userID":"usr_001","items":[{"sku":"SKU-1","quantity":2}]}'
```

## Key files

- `assembly/index.ts` — workflow entry points (`PlaceOrder`, `CancelOrder`)
- `asconfig.json` — AssemblyScript compiler configuration
- `package.json` — build scripts with `@cleat/transform` and metadata injection
