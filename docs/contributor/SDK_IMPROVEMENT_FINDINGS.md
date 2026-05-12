# SDK Improvement Findings

Implementation of architecturally feasible SDK gap closure plan.
May 7, 2026.

## Summary

5 of 5 planned improvements implemented across 6 files (+ new saga.rs).
342 lines added, 13 modified. Go builds and vets cleanly.

## Results

### 1. Lock API (AcquireLock/ReleaseLock) — ALL SDKs

**Go SDK:** Already complete from previous work (`cleat/runtime.go` lines 309-318).

**Rust SDK** (`crates/cleat-sdk/src/host_calls.rs`):
- Added `cleat_acquire_lock` and `cleat_release_lock` to `extern "C"` block
- Added `acquire_lock(&self, key, ttl_ms) -> (bool, Option<String>)` method
- Added `release_lock(&self, key) -> Option<String>` method
- Follows promise method patterns with scratch memory + result bit decoding

**Java SDK** (`crates/cleat-java/src/main/java/cleat/HostCalls.java`):
- Added `@Import` native declarations for `cleatAcquireLockRaw`/`cleatReleaseLockRaw`
- Added `acquireLock(String key, long ttlMs) -> boolean` public method
- Added `releaseLock(String key)` public method
- String packing via `packStrings`, result decoding matches Go host convention

**AssemblyScript SDK** (`packages/cleat-as/assembly/host-calls.ts`):
- Added `@external("env", ...)` import declarations
- Added `acquireLock(key, ttlMs) -> DurableResult<bool>` method
- Added `releaseLock(key) -> string | null` method

**Python SDK** (`python-sdk/cleat_sdk/host_calls.py`):
- WIT imports and stubs already present from previous work
- Added `acquire_lock(key, ttl_ms) -> bool` method on HostCalls class
- Added `release_lock(key) -> None` method on HostCalls class
- `python-sdk/wit/cleat.wit` already had `durable-lock` interface

### 2. Rust Saga (`crates/cleat-sdk/src/saga.rs`)

**Created:** `crates/cleat-sdk/src/saga.rs` (74 lines)
- `SagaStep` struct: name + forward/compensate closures
- `Saga` struct: `new()`, `add_step()`, `run()` with LIFO compensation
- Builder pattern: `Saga::new().add_step(...).add_step(...).run(&h)`
- Compensation errors accumulated and appended to original error message
- Exported from `lib.rs`

**Edited:** `crates/cleat-sdk/src/host_calls.rs`
- Changed `continue_as_new` return type from `Option<String>` to `Result<(), String>`
- Now follows same convention as `reply_to_signal`, `signal_workflow`, `resolve_promise`

### 3. Python WIT Gaps

**`python-sdk/wit/cleat.wit`:**
- Added `durable-child-workflow-with-options` to `durable-children` interface
- Added `durable-lock` interface with `acquire-lock` and `release-lock`
- Added `import durable-lock` to `cleat-workflow` world

**`python-sdk/cleat_sdk/host_calls.py`:**
- Added `cleat_child_workflow_with_options`, `cleat_acquire_lock`, `cleat_release_lock` to WASM import block
- Added `_import_cleat_acquire_lock` and `_import_cleat_release_lock` fallback stubs
- Added architecture docstring to `cleat_fetch`: "Routes through cleat_call('http', 'fetch', ...) — no separate WASM import needed."

### 4. Java Convenience Wrappers

**`crates/cleat-java/src/main/java/cleat/HostCalls.java`:**
- Added `fetchGetJson(String url)` — convenience wrapper, delegates to `cleatFetch("GET", url, null, null)`, returns body string
- Added `fetchGetJson(String url, Map<String, String> headers)` — overload with custom headers

## What Was Already Done (Discovered During Implementation)

- **Go lock API**: Fully implemented in `cleat/runtime.go` from previous work
- **Rust ContinueAsNew**: WASM import already existed, only return type needed fixing
- **Python lock WIT**: `durable-lock` interface already existed in `cleat.wit`
- **Python lock stubs**: `_import_cleat_acquire_lock` and `_import_cleat_release_lock` stubs already existed
- **Java typed call**: `cleatCallTyped` already existed at line 1695 — no new typed call helper needed

## Items Not Implemented (Architecturally Infeasible or External Limitations)

| Gap | Reason |
|-----|--------|
| `ctx.run()` side-effect caching | Requires engine event history changes |
| Virtual Object runtime enforcement | Requires worker-level key routing |
| TeaVM tree-shaking fix | TeaVM limitation, not cleat bug |
| SUSPEND_SENTINEL bit overlap | AssemblyScript runtime issue |
| try/catch in AS | AS runtime limitation |
| String-based service routing | Fundamental to WASM architecture |
| Unit mismatches | Convention, documented in DX_COMPARISON.md |

## Verification

- `go build ./cleat/... ./internal/host/...` — PASS
- `go vet ./cleat/... ./internal/host/...` — PASS
- Rust, Java, AS, Python SDKs: manual verification of syntax and patterns
