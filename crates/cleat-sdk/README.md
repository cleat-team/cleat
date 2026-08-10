# `cleat-sdk` -- Rust SDK for cleat durable workflows

Rust SDK providing `HostCalls` WASM host bindings, the `#[cleat_entry]`
proc-macro, a mock test harness, and typed plugin wrappers for writing durable
workflows that compile to WebAssembly.

## Installation

Add to your `Cargo.toml`:

```toml
[dependencies]
cleat-sdk = { path = "path/to/cleat/crates/cleat-sdk" }
cleat-macro = { path = "path/to/cleat/crates/cleat-macro" }
serde = { version = "1", features = ["derive"] }
serde_json = "1"
```

## Quick Start

```rust
use cleat_sdk::HostCalls;
use cleat_macro::cleat_entry;
use serde::{Deserialize, Serialize};

#[derive(Deserialize)]
struct GreetInput { name: String }

#[cleat_entry]
fn greet_workflow(h: &HostCalls, input: GreetInput) -> Result<String, String> {
    h.cleat_log(&format!("Hello workflow started for {}", input.name));
    let (resp, err) = h.cleat_call("greeter", "Greet",
        &serde_json::json!({"name": input.name}).to_string());
    resp.ok_or_else(|| err.unwrap_or_else(|| "unknown error".into()))
}
```

## The `#[cleat_entry]` Macro

Transforms a function into a `#[no_mangle]` WASM export with the ABI signature
`(args_ptr, args_len, out_ptr, max_out_len) -> i64`.

**Requirements:**
- First parameter must be `&HostCalls` (injected by the macro).
- Exactly one additional parameter, deserialized from the workflow input JSON.
- Return type must be `Result<T, E>` where `T: Serialize` and `E: Display`.
- Function must **not** be `async` (WASM does not support futures).

**Generated wrapper behavior:**
- Wraps the body in `std::panic::catch_unwind` to intercept
  `SuspendSentinel` panics and propagate suspension back to the host.
- Normalizes output JSON through the host's `encoding/json` for cross-language
  deterministic serialization.
- On panic with `SuspendSentinel`, returns the sentinel value to the host
  engine (workflow suspension). All other panics are re-dispatched.

**Compile-time validation errors:**

| Condition | Error message |
|-----------|---------------|
| `async fn` | `#[cleat_entry] does not support async functions` |
| Non-`Result` return | `function must return Result<T, E>` |
| Missing `&HostCalls` | `first parameter must be &HostCalls` |
| More than 1 extra param | `functions must have exactly one input parameter` |
| Destructuring pattern | `destructuring patterns ... are not supported` |

## HostCalls API (Key Functions)

The `HostCalls` struct wraps all WASM imports from the `"env"` module.

### Durable Execution

| Method | Description |
|--------|-------------|
| `call` / `cleat_call` | Recorded API call, returns `(String, Option<String>)` |
| `cleat_call_typed` | Typed variant: `(service, op, &T) -> Result<R, String>` |
| `cleat_call_with_retry` | Server-side retry with `RetryPolicy` |
| `cleat_call_heartbeat` | Long-running call with progress heartbeats |
| `cleat_sleep` / `cleat_sleep_ms` | Suspend for a duration (survives restarts) |
| `cleat_log` | Emit a log message (recorded in event history) |
| `cleat_fetch` | Durable HTTP fetch, returns `Result<FetchResult, String>` |
| `cleat_send` | Fire-and-forget (no response) |
| `schedule_invoke` | Delayed one-shot invocation |
| `side_effect` | Record non-deterministic computation result |

### Signals & Events

| Method | Description |
|--------|-------------|
| `await_signals` / `await_signals_ms` | Wait for external signals, returns `(name, payload, timed_out, error)` |
| `poll_signal` | Non-blocking signal check |
| `poll_cancellation` | Check for cancellation request |
| `signal_workflow` | Fire-and-forget signal to another workflow |
| `send_signal_and_wait` | Signal with response (request-response) |
| `reply_to_signal` | Respond to a signal from within a handler |

### Child Workflows

| Method | Description |
|--------|-------------|
| `child_workflow` | Start child, returns `(run_id, error)` |
| `child_workflow_with_options` | Start with `ChildWorkflowOptions` (version, priority, policy) |
| `child_workflow_in_schema` | Start child in a different schema |
| `child_workflow_typed` | Typed child start via serde |
| `await_child` / `await_child_typed` | Await single child completion |
| `await_all_children` | Await multiple children concurrently |
| `await_any_child` | Await first completing child |
| `poll_child` | Non-blocking child check |

### State, Promises, Handlers, Locks

| Method | Description |
|--------|-------------|
| `set_state` / `get_state` / `delete_state` | Typed state operations |
| `incr_state` / `has_state` / `list_state` | Numeric state, existence, prefix listing |
| `set_query_state` | Set externally-queryable state |
| `create_promise` / `await_promise` | Durable promise creation and awaiting |
| `resolve_promise` / `reject_promise` | Promise resolution |
| `register_update_handler` | Register bi-directional update handler |
| `acquire_lock` / `release_lock` | Concurrency lock management |
| `continue_as_new` / `continue_as_new_versioned` | Workflow history compaction |
| `run_detached` | Execute detached from cancellation |
| `cleat_defer` | Register cleanup action on exit |

### Virtual Objects (Entity Workflows)

| Method | Description |
|--------|-------------|
| `set_scope` | Scope state to entity instance, returns previous scope |
| `get_scope` | Get current `(object_type, instance_key)` scope |
| `clear_scope` | Clear scope, returns previous |
| `uuid` | Deterministic UUID from seed |

### Plugin Calls and JSON Helpers

| Method | Description |
|--------|-------------|
| `plugin_call` / `plugin_call_typed` | Call host plugin function |
| `plugin_call_streaming` | Streaming plugin call |
| `json_parse` / `json_stringify` | Validate/canonicalize JSON via host (WASM only) |

### RetryPolicy

```rust
use cleat_sdk::RetryPolicy;

let policy = RetryPolicy {
    max_attempts: 3,
    initial_interval_ms: 1000,       // 1 second
    backoff_multiplier: 2.0,         // exponential backoff
    maximum_interval_ms: 30_000,     // cap at 30 seconds
    non_retryable_errors: vec!["INVALID_INPUT".into()],
};
```

## WASM Compilation

Compile using the `wasm32-wasip1` target:

```bash
rustup target add wasm32-wasip1
cargo build --target wasm32-wasip1 --release
```

The output `.wasm` file is at `target/wasm32-wasip1/release/your_crate.wasm`.

The `#![no_std]` attribute is not required -- the `wasm32-wasip1` target
provides std support. The SDK's WASM imports (`extern "C"` from the `"env"`
module) are resolved at runtime by the cleat worker.

## Testing with MockHostCalls

The SDK includes a WASM-free test harness for unit testing with `cargo test`:

```rust
use cleat_sdk::test::{CleatTest, MockHostCalls};

#[test]
fn test_payment_workflow() {
    let mut env = CleatTest::new();

    // Stub external service
    env.register_call_stub("payment", "charge", r#"{"id":"ch_123"}"#);

    // Run the workflow with mock HostCalls
    let result = env.run_workflow(
        |host: &mut MockHostCalls, input: &str| -> String {
            let (resp, err) = host.cleat_call("payment", "charge", input);
            resp
        },
        r#"{"amount":5000}"#,
    );

    assert_eq!(result, r#"{"id":"ch_123"}"#);
    assert!(env.assert_called("payment", "charge"));
}
```

Key test harness methods:

| Method | Description |
|--------|-------------|
| `register_call_stub(service, op, response)` | Stub a call response |
| `register_call_error(service, op, error)` | Stub a call error |
| `deliver_signal(name, payload)` | Inject a signal |
| `advance_time(ms)` | Advance simulated clock |
| `set_retry_simulation(n)` | Simulate `n` transient failures |
| `run_workflow(fn, input)` | Execute workflow closure |
| `assert_called(service, op)` | Verify call was made |
| `assert_not_called(service, op)` | Verify call was not made |
| `assert_state(key, value)` | Verify workflow state |

The `#[cleat_test]` attribute (from `cleat_macro`) wraps tests in
`catch_unwind` so `SuspendSentinel` panics are safely intercepted:

```rust
use cleat_macro::cleat_test;

#[cleat_test]
fn test_workflow_with_suspend() {
    let mut mock = MockHostCalls::new();
    // Test code that may encounter SuspendSentinel
}
```

## Typed Plugins

The `Plugins` struct provides typed wrappers for built-in host plugins:

```rust
use cleat_sdk::{HostCalls, plugins::Plugins};

fn workflow_with_plugins(h: &HostCalls) -> Result<String, String> {
    let p = Plugins::new(h);

    // Slack
    let (msg, err) = p.send_message("cfg-1", "Hello!", "#general", None);

    // LLM
    let (chat, err) = p.chat("gpt-4", &messages, &tools);

    // Blobstore, PagerDuty, Kafka, Feature Flags, Webhooks
    let (blob, err) = p.blobstore_put("k", "data", "text/plain", None, "1h");
    let (incident, err) = p.trigger_incident("cfg-1", "Error", "critical", "app", None);

    Ok("done".into())
}
```

## Known Limitations

| Feature | Status | Workaround |
|---------|--------|------------|
| `async fn` | Not supported | Use synchronous code with host calls |
| WASM target | `wasm32-wasip1` required | `wasm32-unknown-unknown` lacks std |
| Threading | Single-threaded | WASM constraint; not needed for workflows |
| `std::time::Instant` | Non-deterministic on replay | Use `h.now()` instead |
| `std::time::SystemTime` | Non-deterministic on replay | Use `h.now()` instead |
| File I/O | Unavailable in WASM | Use `h.cleat_fetch()` or plugin calls |
| Networking | Unavailable in WASM | Use `h.cleat_call()` / `h.cleat_fetch()` |
| `json_parse` / `json_stringify` | Host JSON normalization only available on WASM target | Native stubs return input unchanged |
