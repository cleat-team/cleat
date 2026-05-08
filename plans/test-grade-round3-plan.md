# Round 3 Test Quality & Coverage — Skeptical Review & Plan

Date: 2026-05-07 | Overall coverage: **31.1%** | Grade after Round 2: **B+**

---

## Skeptical Review

### What Round 2 actually delivered (honest assessment)

Round 2 added 134 tests and eliminated "zero-test packages." But a closer look reveals most of the new coverage is **shallow**:

| What we added | Realism | Depth |
|---------------|---------|-------|
| ai/llm tests (19) | Serialization-only — test JSON marshal/unmarshal against a `mockCallRecorder`. The `Chat()` convenience function is 0%. No HTTP, no real LLM call path. | Surface |
| ai/pgvector tests (16) | Same — JSON field mapping tests. No SQL generation verification. | Surface |
| CLI tests (37) | Flag parsing and help text. `cmd/cleat` at 0.8% real coverage. `cmd/cleat-worker` at 4.6%. These prove the binary compiles, nothing more. | Smoke |
| oauthprovider tests (10) | Good — fake SQL driver, token lifecycle, middleware verification. But only tests the HTTP layer; the actual OAuth2 provider integration (Google/GitHub) is untested. | Medium |
| jobqueue tests (15) | Good — fake SQL driver, enqueue/claim/complete, poll loop. But the `background.go` `Run()` function isn't actually exercised concurrently. | Medium |
| auditlog/datadogexport/slacknotify/webhookingest (37) | Good — follow the blobstore pattern. HTTP handler tests with fake stores. | Medium |

**Verdict**: Round 2 was a quantity play. We closed the "zero tests" gap but the *quality* of most new tests is shallow validation of serialization/plumbing, not deep behavioral verification.

### Where the real risk lives

1. **`internal/host` at 7.3%** — The WASM execution engine. This is the most critical code in the project. The existing `integration_test.go` tests are good but require PostgreSQL + TinyGo and likely aren't run regularly. There are no unit-level tests for the execution loop, replay detection, or divergence handling.

2. **`cleattest` at 47.9%** — The test harness has ~30 functions at 0%: child workflows, promises, updates/queries, signals, locks, side effects, plugin calls, continue-as-new. You can't build reliable workflow tests if the test harness itself is half-untested.

3. **`cleat/embedded` at 44.8%** — The embedded runner has acquireLock, releaseLock, awaitCondition, sideEffect, random all at 0%. These are core workflow primitives.

4. **`plugins/blobstore` at 31.9%** — The S3 backend, background cleanup, and host functions (blobPut/blobGet) are entirely untested. The existing tests only cover the HTTP routes against a fake in-memory store.

5. **`plugins/ratelimiter` at 38.0%** — The actual rate limit enforcement logic, burst handling, and per-tenant isolation are untested. The existing 12 tests only scratch the surface.

6. **`plugins/kafkaconnect` at 24.2%** — Produce/consume host functions have near-zero coverage.

7. **`cmd/cleat-worker` at 13.4%** — The production worker daemon (1887 lines) — dispatch loop, heartbeat, reaper, compaction, API server — all essentially untested.

8. **`cmd/cleat` at 6.7%** — The main build/deploy CLI (834 lines) — build pipeline, deploy logic, version management — all untested.

### What's missing entirely

- **Fuzz tests**: Zero. The WASM parser, JSON deserialization, cron expression parser, and filter expression parser are all fuzz-worthy boundaries.
- **Concurrent tests with `-race`**: Most behavioral tests are single-goroutine. The jobqueue poll loop test starts a goroutine but doesn't stress-test concurrent claim patterns.
- **Error injection**: No tests inject faults (network errors, partial writes, context cancellation) into the execution path.
- **Performance regression tests**: No benchmark comparisons or latency assertions.
- **Python/Rust/AssemblyScript/Java SDK tests**: These run only in-language, not as part of `go test`. Their results aren't aggregated into the coverage report.

---

## Round 3 Plan

### Priority 1 — Host engine test hardening (3 days)

The execution engine at `internal/host` is the single biggest risk.

**Step 1a: Unit tests for event replay and divergence detection**
- Test `ReplayEvents()` with tampered history → divergence detected
- Test `ReplayEvents()` with missing events → error
- Test `ReplayEvents()` with extra events → error
- Test replay with different WASM binary → divergence detected
- Test replay with version migration path (v1 events → v2 WASM)

**Step 1b: Unit tests for execution loop**
- Test `ExecuteWorkflow()` happy path (start → step → complete)
- Test cancellation during execution
- Test timeout during await
- Test error from WASM call → retry
- Test signal delivery mid-execution

**Step 1c: Make integration tests runnable without TinyGo**
- Pre-build the test WASM binary and check it in
- Or add a `-short` compatible path using a pre-compiled WASM blob
- This way the integration tests actually run in CI regularly

### Priority 2 — Test harness completion (2 days)

`cleattest` is the foundation for all future workflow tests. Close the gaps:

**Step 2a: Child workflow support**
- Test `RegisterChildWorkflow` + `OnChildWorkflow` + `childWorkflowImpl`
- Test `awaitChildImpl` + `awaitAllChildrenImpl`
- Test typed child workflow variants

**Step 2b: Promise and update/query support**
- Test `createPromiseImpl` + `ResolvePromise` + `RejectPromise` + `awaitPromiseImpl`
- Test `registerUpdateHandlerImpl` + `HandleUpdate`
- Test `registerQueryHandlerImpl` + `HandleQuery`

**Step 2c: Signal and lock support**
- Test `sendSignalAndWaitImpl` + `signalWorkflowImpl` + `replyToSignalImpl`
- Test `acquireLockImpl` + `releaseLockImpl` + `awaitConditionImpl`

**Step 2d: Other primitives**
- Test `continueAsNewImpl`, `sideEffectImpl`, `runDetachedImpl`
- Test `pluginCallImpl`, `durableLogImpl`, `pollCancellationImpl`

### Priority 3 — Embedded runner primitives (1 day)

Close the 0% gaps in `cleat/embedded`:
- Test `acquireLock`/`releaseLock` — lock contention and release
- Test `awaitCondition` — condition variable semantics
- Test `sideEffect` — deterministic replay of side effects
- Test `random` — seeded random for deterministic replay

### Priority 4 — Blobstore backend and host functions (1.5 days)

The HTTP routes are tested but the actual storage backends are not:

**Step 4a: Memory backend tests**
- Test `Put`/`Get`/`Delete` on `newMemoryBackend`
- Test TTL expiry
- Test concurrent access

**Step 4b: Host function tests**
- Test `blobPut`/`blobGet` host functions end-to-end
- Test large blob handling
- Test error cases (missing key, permission denied)

**Step 4c: Background cleanup**
- Test `cleanupExpired` marks and removes expired blobs
- Test that `Run()` starts and stops cleanly

### Priority 5 — Rate limiter enforcement (1 day)

The existing 12 tests are configuration CRUD. Missing:
- Test actual rate limiting: N requests within window → N+1 rejected
- Test burst handling
- Test per-tenant isolation (tenant A exhausted doesn't block tenant B)
- Test sliding window accuracy
- Test middleware integration end-to-end

### Priority 6 — Fuzz testing (1.5 days)

Add fuzz tests at key boundaries:
- `FuzzFilterExpressionParser` — the event filter expression parser in `plugins/eventtriggers/filter.go`
- `FuzzCronExpressionParser` — cron parsing in `plugins/scheduler`
- `FuzzWASMBinaryParser` — WASM module parsing in `internal/wasm`
- `FuzzJSONDeserialization` — the AI client request/response JSON paths
- `FuzzWorkflowInput` — workflow input JSON parsing

### Priority 7 — Worker daemon smoke tests without DB (1 day)

The `cmd/cleat-worker` package needs tests for its internal logic:
- Test `dispatchLoop` selection logic (multiple queues, priority)
- Test `heartbeatLoop` expiry detection
- Test `reaperLoop` timeout detection
- Test `compactionLoop` threshold logic
- These can be unit-tested by mocking the database layer

### Priority 8 — Coverage threshold raising (0.5 days)

After steps 1-7, raise the CI coverage thresholds:
- `cleat/`: 10% → 20%
- `plugins/`: 15% → 25%
- `internal/`: 50% → 55%
- `cmd/`: 0% → 5%

---

## Summary

| Metric | After Round 2 | Target After Round 3 |
|--------|---------------|---------------------|
| Overall grade | B+ | A- |
| Total tests | ~1,364 | ~1,550 |
| Overall coverage | 31.1% | ~40% |
| `internal/host` coverage | 7.3% | 20%+ |
| `cleattest` coverage | 47.9% | 80%+ |
| `cleat/embedded` coverage | 44.8% | 70%+ |
| Zero-coverage primitives (cleattest) | ~30 functions | 0 |
| Fuzz tests | 0 | 5+ |
| Packages below 30% | 11 | 3 |
| `-race` test coverage | None | All behavioral tests |
| CI runs integration tests | Rarely (needs TinyGo) | Always (pre-built WASM) |

### Estimated effort: 11.5 days

| Step | Days | Impact |
|------|------|--------|
| 1: Host engine hardening | 3 | Critical risk reduction |
| 2: Test harness completion | 2 | Foundation for all future tests |
| 3: Embedded runner primitives | 1 | Core primitive coverage |
| 4: Blobstore backend | 1.5 | Storage integrity |
| 5: Rate limiter enforcement | 1 | Security/DoS protection |
| 6: Fuzz testing | 1.5 | Boundary hardening |
| 7: Worker daemon smoke | 1 | Production path coverage |
| 8: Coverage thresholds | 0.5 | Ratchet up |
