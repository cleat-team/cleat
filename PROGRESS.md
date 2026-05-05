# Durable Execution Project — Progress

## Status: Transformer and SDK complete; host runtime pending

## What's built

### Transformer pipeline (Phases 1-8)
- Package loading, type resolution, entry point detection
- Call graph construction with durable leaf identification
- Transitive closure computation and construct validation (E001-E007, W001)
- HostCalls threading verification (E010) including global var h pattern
- Auto-threading transform (context object → param injection)
- WASM import/export/adapter code generation
- Build directory assembly and WASM compilation
- CLI: `durable build` and `durable vet`

### SDK runtime (durable package)
- `HostCalls` interface with 23 methods
- `hostCallsImpl` concrete implementation with WASM adapter hooks
- `Saga` structured compensation (nil-safe)
- `PollUntil` generic sleep-based polling
- `Selector` multi-future wait (signals, timer, child workflow)
- `CallError` structured error types with retryable/non-retryable classification
- `RetryPolicy` with exponential backoff and non-retryable error filtering
- `DurableCallTyped` eliminating manual JSON marshaling
- `LogKV` structured key-value logging
- `SignalResult` structured AwaitSignals return

### Testing framework (durabletest)
- Mock `HostCalls` with stub registration (string, nil, func matchers)
- Simulated clock with AdvanceTime/SetTime
- Signal delivery (immediate and scheduled)
- Call history recording with AssertCalled/AssertNotCalled
- Thread-safe for concurrent test execution

### Code generator (durable-gen)
- Parses spec directories with `Client` interface definitions
- Generates typed client wrappers using `DurableCallTyped`
- Eliminates magic strings from service/operation calls

### Examples
- `testdata/basic/` — workflow with explicit h parameter threading
- `testdata/autothread/` — workflow using global h pattern (auto-threaded)
- `testdata/errors/` — deliberate violations for validation testing
- `examples/fooddash/` — full food delivery app with Saga, signals, typed clients
- `examples/fooddash/clients/` — generated typed clients (dispatch, menu, orders, payments, restaurant)
- `examples/fooddash/spec/` — service spec interfaces for code generation

### Demo (wasm-demo/)
- Host runtime with checkpoint/replay, crash simulation, resume
- Worker with DB failover handling
- Versioned WASM loading
- Cluster design walkthrough (observability, resilience, performance, versioning)

## What's missing (remaining Phases 9-13)
- Phase 9 (partial): E008-E011, W002 validation rules
- Phase 10: tinygo compilation target
- Phase 11: `durable deploy` + database schema
- Phase 12: WASM conformance tests
- Phase 13: Defer execution engine
- Production host runtime (wazero integration with PostgreSQL)

## Key design decisions
[See durable_context.md for architectural rationale]
