# Rust Workflow

Demonstrates a durable order processing workflow written in Rust using the `cleat-sdk` crate and `#[cleat_entry]` proc-macro.

## What it shows

- `#[cleat_entry]` proc-macro for marking WASM-exported workflow entry points
- Saga-like compensation: reserve inventory, charge payment, create shipment, notify
- `HostCalls.cleat_call` for durable service invocations
- `HostCalls.poll_cancellation` for cancellation-aware workflows
- `HostCalls.cleat_log` for structured audit logging
- Serde for typed JSON serialization/deserialization
- WASM target `wasm32-wasip1` with `cdylib` crate type

## Build

```bash
cd examples/rust-workflow
cargo build --target wasm32-wasip1 --release
```

## Run

```bash
cleat deploy rust-workflow target/wasm32-wasip1/release/rust_workflow.wasm
cleat run place_order '{"user_id":"usr_001","cart":[{"sku":"SKU-1","quantity":2}]}'
```

## Key files

- `src/lib.rs` — workflow entry points (`place_order`, `cancel_order`) and unit tests
- `Cargo.toml` — crate configuration with `cleat-sdk` and `cleat-macro` dependencies
