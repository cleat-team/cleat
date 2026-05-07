# Issues Found in the Cleat Rust SDK

This document catalogs gaps, bugs, and missing features discovered during the port of the Restate Rust basics example. Issues are ordered by severity.

---

## Critical Gaps

### C1: Missing K/V State Operations (`get_state` / `set_state` / `delete_state`)

**Impact**: Virtual Objects pattern cannot be fully implemented. The Greeter example relies on `ctx.get("count")` and `ctx.set("count", count)` for persistent state, which have no equivalent in HostCalls.

**Evidence**: 
- `HostCalls` exposes 25 import methods (host_calls.rs) but none for persistent K/V state
- Only `set_query_state()` exists, which writes to *running workflow query state*, not persistent Virtual Object storage
- The Go SDK has `GetState`/`SetState`/`DeleteState`; the Rust SDK does not

**Suggested fix**: Add to `host_calls.rs`:
```rust
pub fn get_state(&self, key: &str) -> (String, Option<String>);
pub fn set_state(&self, key: &str, value: &str) -> Option<String>;
pub fn delete_state(&self, key: &str) -> Option<String>;
```

These would map to new WASM imports (`durable_get_state`, `durable_set_state`, `durable_delete_state`) matching the Go SDK's ABI.

---

### C2: Missing `resolve_promise`

**Impact**: The workflow email-verification pattern cannot use Restate's promise-based approach. In Restate, `ctx.resolve_promise("email-link", secret)` resolves a promise from a separate handler. In Cleat, `create_promise`/`await_promise` exist but there is no `resolve_promise`.

**Evidence**:
- `durable_create_promise` (ABI 2.20) and `durable_await_promise` (ABI 2.21) are implemented
- No `durable_resolve_promise` or similar exists in the imports
- The closest alternatives are `reply_to_signal` (requires correlation token) and `signal_workflow` (fire-and-forget)

**Workaround in this port**: Use `await_signals` in the workflow and `signal_workflow` from the click handler. This works but is semantically different from the Restate pattern.

**Suggested fix**: Add `resolve_promise` to HostCalls mapping to a new WASM import `durable_resolve_promise(promise_id, payload)`.

---

### C3: Missing `ctx.run()` / Idempotent Action Wrapping

**Impact**: Restate's `ctx.run(|| async_fn())` wraps arbitrary side effects, persists their results, and skips re-execution on replay. Cleat has no equivalent for wrapping non-deterministic or non-durable code.

**Evidence**:
- `durable_call` is already durable (host-managed), but there's no way to wrap inline synchronous code (e.g., generating a local random value, calling an external HTTP API not routed through the host)
- In Restate, `ctx.run()` is the primary tool for idempotency alongside `ctx.rand_uuid()`
- The Restate examples use `ctx.run()` extensively (subscription creation, payment creation, user entry creation, email sending)

**Suggested fix**: Add a `durable_run` or similar mechanism that persists the result of a closure and returns it on replay. This is architecturally complex for WASM since closures can't be replayed from bytecode, but it could be modeled as a host call that takes a serialized operation descriptor.

---

### C4: No HTTP Server / Service Registration

**Impact**: Unlike Restate's SDK which includes `HttpServer` and `Endpoint::builder()` for local development and testing, the Cleat Rust SDK is purely WASM-based with no local development server. Developers must have a Cleat host runtime to test.

**Evidence**:
- The SDK has no `HttpServer`, `Endpoint`, or service registration utilities
- No mechanism to run entry points outside of a WASM host
- No mock or test harness for HostCalls

**Suggested fix**: Provide a mock `HostCalls` implementation for unit testing, and document how to run the WASM module with a local Cleat host.

---

## Moderate Issues

### M1: No Async Support

**Impact**: All `#[durable_entry]` functions are synchronous. The HostCalls methods are also synchronous (they panic with `SuspendSentinel` for suspension). This means all entry functions must be synchronous, which differs from Restate's async handler model.

**Evidence**:
- `#[durable_entry]` generates a `fn` (not `async fn`) export
- HostCalls methods are synchronous and use `std::panic::panic_any` for suspension
- The `catch_unwind` wrapper in the proc-macro handles suspend

**Note**: This is by design (WASM has no built-in async), but it's a significant ergonomic difference that affects porting.

---

### M2: JSON as Core Serialization Format

**Impact**: All HostCalls methods operate on raw JSON strings. There is no generic type parameter (like `durable_call<T: Serialize, R: Deserialize>`) to handle serialization/deserialization automatically.

**Evidence**:
- `durable_call(service, op, request_json: &str) -> (response_json: String, Option<String>)`
- Inputs and outputs are always JSON strings
- Each entry function must manually parse/serialize JSON (the proc-macro does auto-deserialize the input argument)

**Suggested fix**: Add typed wrapper methods to HostCalls:
```rust
pub fn durable_call_typed<T: Serialize, R: Deserialize>(&self, service: &str, op: &str, req: &T) -> Result<R, String>
```

---

### M3: `set_query_state` is the Only State API

**Impact**: `set_query_state` is documented as setting observable state on a *running workflow*, not as a persistent K/V store. Using it for Virtual Object state (as this port does as a workaround) is semantically incorrect.

**Evidence**:
- Comment in the ported code: "[set_query_state] is NOT persisted across invocations -- it's query state on the running workflow, not Virtual Object state"

---

### M4: No `send_after` / Delayed Message Scheduling

**Impact**: Restate provides `send_after(Duration)` for scheduling delayed message delivery. Cleat has no equivalent. The `durable_sleep` exists but there is no way to schedule a message to be delivered to another service after a delay.

**Evidence**:
- The Restate building blocks example uses `ctx.object_client().cancel().send_after(Duration::from_secs(5))`
- Cleat has `signal_workflow` (immediate) but no delayed variant

---

## Minor Issues

### m1: `durable_log` Does Not Accept Format Arguments

The Rust SDK's `durable_log` takes only `&str`, requiring callers to use `format!()` or `to_string()` explicitly. A `durable_log!` macro would be more ergonomic.

### m2: No Documentation / Doc Examples on HostCalls Methods

Several HostCalls methods lack doc comments explaining their behavior, return values, and error conditions. The existing comments reference Go implementations but are not complete Rust documentation.

### m3: No WIT or Interface File

There is no WIT (WebAssembly Interface Types) file describing the host imports. This makes tooling integration harder.

### m4: `stdlib` Test File in Target Directory

The target directory contains a `.d` dependency file but no separate test WASM. Unit tests only run on the host target, which means they cannot test the WASM ABI directly.

---

## WASM Compilation Assessment

| Check | Result |
|-------|--------|
| Host target compilation (`cargo check`) | PASS |
| WASM target compilation (`wasm32-wasip1`) | PASS |
| `#[durable_entry]` macro | PASS - generates correct WASM exports |
| HostCall import linking | PASS - all 13 used functions link to `env` module |
| Export function names | PASS - all 6 names present in WASM binary |
| Binary size | 141 KB (release, stripped) |
| Compilation warnings | None (after fix) |

## Summary of API Completeness

Comparing the Cleat Rust SDK HostCalls against the Go SDK and Restate Rust SDK:

| API Area | Restate Rust SDK | Cleat Rust SDK (Go parity) | Cleat Rust SDK (current) |
|----------|-----------------|---------------------------|--------------------------|
| Durable call | `ctx.object_client().call()` | `DurableCall()` | `durable_call()` |
| Durable sleep | `ctx.sleep()` | `DurableSleep()` | `durable_sleep()` |
| UUID | `ctx.rand_uuid()` | `UUID()` | `uuid()` |
| Random | N/A (use UUID) | `Random()` | `random()` |
| State get | `ctx.get()` | `GetState()` | **MISSING** |
| State set | `ctx.set()` | `SetState()` | **MISSING** |
| State delete | N/A | `DeleteState()` | **MISSING** |
| Promise create | N/A | `CreatePromise()` | `create_promise()` |
| Promise await | `ctx.promise()` | `AwaitPromise()` | `await_promise()` |
| Promise resolve | `ctx.resolve_promise()` | N/A | **MISSING** |
| Awakeable | `ctx.awakeable()` | N/A | N/A |
| Signal workflow | N/A | `SignalWorkflow()` | `signal_workflow()` |
| Await signals | N/A | `AwaitSignals()` | `await_signals()` |
| Child workflow | `ctx.one_way_call()` | `ChildWorkflow()` | `child_workflow()` |
| Await child | N/A | `AwaitChild()` | `await_child()` |
| Send delayed | `ctx.send_after()` | N/A | **MISSING** |
| Set scope | built-in | `SetScope()` | `set_scope()` |
| Get scope | `ctx.key()` | `GetScope()` | `get_scope()` |
| Query state | `set_query_handler()` | `SetQueryState()` | `set_query_state()` |
| Cancellation | auto-handled | `PollCancellation()` | `poll_cancellation()` |
| Continue-as-new | N/A | `ContinueAsNew()` | `continue_as_new()` |
| Run (idempotent) | `ctx.run()` | N/A | **MISSING** |
| Log | N/A | `DurableLog()` | `durable_log()` |
| Plugins | N/A | `PluginCall()` | `plugin_call()` |
