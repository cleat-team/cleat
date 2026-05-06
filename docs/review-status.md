# Cleat Review — Technical Issues Status

## Round 1: Core Technical Fixes

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

## Round 2: Plugin Gap Fixes

The initial review incorrectly described plugins as "skeletons." A thorough audit found 8 of 11 are genuinely functional. The three with real gaps were fixed:

### Critical — Missing Execution Dispatch

| Plugin | Issue | Status | Resolution |
|--------|-------|--------|------------|
| scheduler | Background worker queried due schedules but only logged "triggering schedule" — never started workflows | **Fixed** | Calls `env.StartWorkflow(ctx, defName, input)` on each due schedule. Logs run ID on success, continues on error |
| jobqueue | Background worker claimed jobs and logged "would dispatch job" — no execution engine | **Fixed** | Added `def_name`/`input` columns to `task_queue`. Background worker dispatches jobs as workflows via `env.StartWorkflow`. Tracks success/failure status |

### Critical — Missing Cleanup

| Plugin | Issue | Status | Resolution |
|--------|-------|--------|------------|
| eventstore | Background cleanup was a no-op — `cleanup()` only logged a message. Table would grow unboundedly | **Fixed** | Implemented retention-based cleanup (configurable `retention_days`, default 30 days). Logs deleted event count |

### Enabler — Plugin Environment API

| Issue | Status | Resolution |
|-------|--------|------------|
| Plugins couldn't start workflows — no programmatic API to call `StartNewRun` | **Fixed** | Added `StartWorkflow func(ctx, defName, input) (runID, error)` to `plugin.Environment`. Wired in worker main.go wrapping `store.StartNewRun` with latest version auto-resolution |

### Important — Security & Correctness

| Plugin | Issue | Status | Resolution |
|--------|-------|--------|------------|
| oauthprovider | No PKCE, no CSRF nonce (state = tenant UUID), sessions stored as plaintext tokens | **Fixed** | Added PKCE (S256), random 32-byte state nonce with 5-min expiry, SHA-256 token hashing at rest. Code verifier stored server-side during flow, cleared after exchange |
| ratelimiter | No rate limit headers on responses, token state lost on restart | **Addressed** | Added `X-RateLimit-Remaining`, `X-RateLimit-Limit`, `X-RateLimit-Reset`, `Retry-After` headers on both 429 and successful responses. Documented that token state is ephemeral (standard practice) |
| blobstore | No tests (largest, most complex plugin at 1,099 lines) | **Fixed** | `plugins/blobstore/blobstore_test.go` — 1,066 lines, 10 tests: CRUD, dedup, TTL, list with prefix, soft-delete, workflow ref GC, auth, not-found. Uses fake SQL driver for hermetic testing. Also fixed routes to support slashed blob keys |

---

## Round 3: Push-to-Signal + Observability

### Webhookingest Push-to-Signal

| Issue | Status | Resolution |
|-------|--------|------------|
| `await_webhook` host function is poll-only — up to retry-interval latency before webhook is processed | **Fixed** | Added `SignalWorkflow func(ctx, workflowID, signalName, payload)` to `plugin.Environment`. Webhook sources can bind to a workflow via `signal_workflow_id`/`signal_name` columns. On ingest, signal is delivered immediately — zero polling latency |

### Observability & Graceful Shutdown

| Issue | Status | Resolution |
|-------|--------|------------|
| No visibility into background worker health — no metrics, no timing, no item counts | **Fixed** | Added structured logging to all 7 background workers with `plugin`, `duration_ms`, and domain-specific count fields (e.g., `deliveries_attempted`/`succeeded`/`failed`, `jobs_claimed`/`dispatched`/`failed`, `deleted_events`) |
| Worker shutdown didn't wait for background goroutines — cleanup could be interrupted | **Fixed** | Added `sync.WaitGroup` coordination with 30-second timeout. SIGTERM cancels context, then waits for all background workers to drain |

---

## Issues Intentionally Not Addressed

| Issue | Rationale |
|-------|-----------|
| Go 1.26+ lock-in | WASM support (`//go:wasmimport` / `//go:wasmexport`) requires Go 1.24+. TinyGo fallback exists but is a language subset |
| SDK panics on nil core primitives | Documented contract: these indicate programmer error. The production WASM adapter always populates all fields |
| JSON as sole serialization format | Design tradeoff. Protobuf/msgpack could be added as optional without breaking the ABI |
| Transformer pipeline complexity | Integration tests now exercise the full pipeline. Differential `durable dev` vs WASM testing is a future enhancement |
| Single PostgreSQL bottleneck | Addressed by sharding (`docs/sharding.md`) and `ShardedStore` |
| Ratelimiter ephemeral token state | Standard practice (API gateways do this). Adding persistence would require a Redis dependency or DB-backed counters |
| `examples/subscription/billing.go` pre-existing build errors | Separate from this review; the broken example predates all changes made here |

---

## Plugin Audit Correction

The initial review described plugins as "skeletons." An audit of all 11 plugins found:

**Useful today (8):** blobstore, notifications, kvstore, slacknotify, auditlog, webhookingest, oauthprovider, ratelimiter
**Needed dispatch wiring (2, now fixed):** scheduler, jobqueue
**Needed cleanup wiring (1, now fixed):** eventstore

All three gaps have been resolved. All 11 plugins are now deployable with real functionality. External dependencies are minimal: only `minio-go` (blobstore S3) beyond `google/uuid`.

---

## Verdict

All 15 actionable issues identified across the review have been resolved in three passes. The project is significantly more production-ready, with:

- A real bug fixed (MinVersion WASM import)
- 4 end-to-end integration tests
- Backward-compatible schema improvement (JSONB payload)
- 3 plugin execution gaps closed (scheduler, jobqueue, eventstore)
- 3 security/correctness issues hardened (oauthprovider PKCE, ratelimiter headers, blobstore tests)
- Push-to-signal for zero-latency webhook delivery
- Observability and graceful shutdown on all background workers

### Total Changes (3 commits)

```
Commit 29e85e3 (Round 1):  320 insertions,  27 deletions —  9 files
Commit 9a9eead (Round 2): 1693 insertions,  50 deletions — 18 files
Commit 63c3868 (Round 3):  280 insertions,  70 deletions — 12 files
─────────────────────────────────────────────────────────────────
Total:                    2293 insertions, 147 deletions — 22 files
```
