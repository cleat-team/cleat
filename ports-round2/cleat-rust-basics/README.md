# Cleat Rust SDK Basics

Port of the Restate Rust "basics" example to the Cleat durable execution Rust SDK.

## Purpose

This is the first Rust port in the Cleat ecosystem. It validates the entire Rust SDK pipeline including:

- `#[durable_entry]` proc-macro for WASM export generation
- `HostCalls` trait methods for durable operations
- WASM compilation pipeline (`wasm32-wasip1` target)
- JSON-based serialization for inputs/outputs

## Patterns Ported

| Pattern | File | Original | Cleat Entry Points |
|---------|------|----------|-------------------|
| Durable Execution | p0_durable_execution.rs | SubscriptionService.add() | `subscription_add` |
| Building Blocks | p1_building_blocks.rs | MyService.run() | `building_blocks_demo` |
| Virtual Objects | p2_virtual_objects.rs | GreeterObject.greet/ungreet | `greeter_greet`, `greeter_ungreet` |
| Workflows | p3_workflows.rs | SignupWorkflow.run/click | `signup_run`, `signup_click` |

## Building

```bash
# Ensure the wasm32-wasip1 target is installed
rustup target add wasm32-wasip1

# Build for WASM
cargo build --target wasm32-wasip1 --release
```

The output WASM binary is at:
`target/wasm32-wasip1/release/cleat_rust_basics.wasm`

## WASM Exports

All 6 entry points are exported as WASM functions:

| Export | Input | Description |
|--------|-------|-------------|
| `subscription_add` | `SubscriptionRequest` | Create payment + subscriptions (durable execution) |
| `building_blocks_demo` | `EmptyInput` | Reference of all durable building blocks |
| `greeter_greet` | `GreetInput` | Increment greeting count (virtual object) |
| `greeter_ungreet` | `GreetInput` | Decrement greeting count (virtual object) |
| `signup_run` | `User` | User signup with email verification (workflow) |
| `signup_click` | `ClickInput` | Handle email verification click (workflow signal) |

## Testing

```bash
# Run unit tests (host target)
cargo test
```

Unit tests cover serialization round-trips and type compatibility.

## Architecture

The Cleat Rust SDK follows a different architecture from Restate's SDK:

- **Restate**: HTTP server with `#[service]`/`#[object]`/`#[workflow]` trait macros, async handlers with `Context` objects
- **Cleat**: WASM library with `#[durable_entry]` proc-macro on standalone functions, synchronous host call bindings

Each `#[durable_entry]` function:
1. Takes `&HostCalls` as first parameter for durable operations
2. Takes one deserializable input struct
3. Returns `Result<String, String>` (JSON output or error message)
4. Generates a `#[no_mangle]` WASM export with the function name

## Key API Mapping

| Restate | Cleat |
|---------|-------|
| `ctx.run(\|\| ...)` | No direct equivalent (gap) |
| `ctx.rand_uuid()` | `h.uuid(seed)` |
| `ctx.sleep(Duration)` | `h.durable_sleep(ms)` |
| `ctx.get(key)` | Missing: `get_state()` (gap) |
| `ctx.set(key, value)` | Missing: `set_state()` (gap) |
| `ctx.awakeable()` | `h.create_promise()` / `h.await_promise()` |
| `ctx.resolve_awakeable()` | Missing: `resolve_promise()` (gap) |
| `ctx.object_client().call()` | `h.durable_call(service, op, json)` |
| `ctx.object_client().send()` | `h.signal_workflow(target, signal, payload)` |
| `ctx.key()` | `h.get_scope().1` (instance_key) |
| `ctx.promise(name)` | `h.await_signals()` / `h.await_promise()` |
| `ctx.resolve_promise()` | Missing: `resolve_promise()` (gap) |
| `set_query_handler()` | `h.set_query_state(key, value)` |
