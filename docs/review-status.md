# Cleat Review — Technical Issues Status

## Issues Found & Resolved

### P0 — Real Bugs

| Issue | Status | Resolution |
|-------|--------|------------|
| `MinVersion` maps to wrong WASM import (`durable_version` instead of `durable_min_version`) | **Fixed** | `internal/wasm/usage.go:54` — 1-line fix |
| ABI.md documents 15 host functions but code defines 18 | **Fixed** | Added §2.16-2.19 for `durable_call_retry`, `durable_call_heartbeat`, `durable_await_all_children`, `plugin_call`. Updated counts in §2 header, §4.2, and changelog |

### P1 — Integration Tests

| Issue | Status | Resolution |
|-------|--------|------------|
| No end-to-end test exercises real DB + WASM compile + execute + replay | **Fixed** | `internal/host/integration_test.go` — 589 lines, 4 tests: full pipeline, multi-step, signals, replay divergence. Guarded by `DURABLE_TEST_DB` env var |

### P2 — Schema Anti-pattern

| Issue | Status | Resolution |
|-------|--------|------------|
| `event_history` has 21 nullable columns for disjoint event types | **Addressed** | `migrations/007_jsonb_payload.sql` adds JSONB `payload` column. `internal/host/db.go` updated to dual-write (old columns + payload) and read from payload when available with fallback to old columns. Old columns preserved for backward compat |

### P3 — Example & Dev Quality

| Issue | Status | Resolution |
|-------|--------|------------|
| FoodDash hardcodes `DriverName: "Alex"`, `ETAMinutes: 15` | **Fixed** | Extended `findDriverResponse` and dispatch client types. `findDriver` now returns full `driverResult`. 5 files updated |
| `durable dev` child workflows are stubs (`awaitChild` always returns `{"status":"completed"}`) | **Fixed** | Added `ChildWorkflowRunner` interface and `WithChildWorkflowRunner` option to `durable/localdev/localdev.go`. Child workflows execute synchronously when a runner is configured. Falls back to stub when none provided |

---

## Issues Intentionally Not Addressed

These were identified in the review but are architectural decisions or require broader discussion:

| Issue | Rationale |
|-------|-----------|
| Go 1.26+ lock-in | WASM support (`//go:wasmimport` / `//go:wasmexport`) requires Go 1.24+. TinyGo fallback exists but is a language subset. This is a platform decision, not a bug |
| SDK panics on nil core primitives (`DurableSleepMs`, `NowMs`, `Random`) | Documented contract: these indicate programmer error. The production WASM adapter always populates all fields. Worker goroutine recover() catches panics if they occur |
| JSON as sole serialization format | Design choice trading performance for simplicity. Protobuf/msgpack could be added as optional alternatives without breaking the ABI |
| Transformer pipeline complexity (5 stages) | The integration tests now exercise the full pipeline. Differential testing between `durable dev` and WASM path is a future enhancement |
| Single PostgreSQL bottleneck | Addressed separately by sharding (`docs/sharding.md`) and `ShardedStore` implementation. Scaling beyond single-instance is documented |

---

## Verdict

All 6 actionable technical issues from the original review have been resolved in a single pass. The remaining items are architectural tradeoffs or require broader strategic decisions. The project is now in a significantly better state for production readiness.

### Files Changed

```
 ABI.md                                       | 123 ++++++++--
 durable/localdev/localdev.go                 |  42 +++-
 examples/fooddash/clients/dispatch/client.go |   4 +-
 examples/fooddash/order.go                   |  22 ++-
 examples/fooddash/order_test.go              |  12 +-
 examples/fooddash/order_typed.go             |   4 +-
 internal/host/db.go                          | 139 +++++++++++-
 internal/wasm/usage.go                       |   2 +-
 internal/host/integration_test.go            | 589 ++++++++++++++++++ (new)
 migrations/007_jsonb_payload.sql             |   4 + (new)
 10 files changed, 910 insertions(+), 27 deletions(-)
```
