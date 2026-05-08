# Updated Test Coverage & Quality Assessment — May 2026

## Grade: B (was C+)

### Why B, not A

The core runtime, host engine, and now auth/AI tests are strong. We fixed the broken/misleading tests and added ~300 meaningful tests. But 7 packages still have zero tests, 5 plugins have single-digit test counts, and there are no end-to-end integration tests that run a real worker process end-to-end.

---

## Current State

### Test Counts by Language

| Language | Tests | Framework | Status |
|----------|-------|-----------|--------|
| Go | ~790 | `go test` | All passing |
| Python | ~300 | pytest | All passing |
| TypeScript (web) | 79 | vitest | All passing |
| Rust SDK | ~30 | `#[test]` | All passing |
| Rust macro | 8 | `#[test]` | All passing |
| AssemblyScript | 16 | as-pect | All passing |
| Java | ~10 | JUnit 5 | All passing |
| **Total** | **~1,230** | | **Zero failures** |

### Go Test Strength by Area

| Area | Tests | Assessment |
|------|-------|-----------|
| Core runtime (cleat/) | 145 | Strong — retry, saga, PollUntil, typed calls, signal results |
| Test harness (cleattest/) | 41 | Strong — mock HostCalls, signal delivery, time advancement |
| Embedded runner | 13 | Good — was tautological, now has behavioral checks |
| Virtual objects | 7 | Adequate — registration, lookup, duplicate/empty-name edge cases |
| Version | 3 constant tests | OK — package is mostly constants |
| Local dev | 7 | Adequate — runner creation, concurrency keys, basic operations |
| **AI / LLM** | | |
| AI agent | 11 | Good — happy path, tools, max steps, errors, context, config |
| LLM utilities | **0** | GAP — no tests |
| pgvector | **0** | GAP — no tests |
| **Internal engine** | | |
| Host engine | 36 | Strong — WASM execution, replay, divergence, fault injection |
| Analyzer | 11 | Good — type checking, name extraction, node search |
| Callgraph | 12 | Good — edge building, leaf detection, called-by consistency |
| Closure | 19 | Good — durable closure computation, error detection |
| Transform | 10 | Good — autothread transform, entry point detection |
| WASM codegen | 30 | Good — bit-packing, import/export generation, Go name conversion |
| **Internal infrastructure** | | |
| Plugin system | 62 | Strong — manifest validation, capabilities, index, lifecycle |
| Plugingen | 31 | Good — Go/Python/Rust/TypeScript code generation from IR |
| Auth | 38 | Strong — middleware (21), tenant store (17), fake driver |
| Telemetry | **0** | GAP — no tests |
| **Plugins** | | |
| kvstore | 17 | Strong — PUT/GET/DELETE, versioning, If-Match, validation |
| eventstore | 13 | Good — append/read, SSE delivery, duplicate handling |
| notifications | 19 | Strong — webhook CRUD, delivery, HMAC, background processing |
| blobstore | 11 | Strong — full lifecycle, TTL, dedup, soft-delete |
| dag | 26 | Strong — topological sort, diamond DAG, cycle detection |
| llm + providers | 56 + 32 | Strong — multi-provider chat, embed, list_models, error handling |
| pgvector | 28 | Good — search, upsert, delete, filtering |
| featureflags | 28 | Good — flag evaluation, context matching |
| eventtriggers | 14 | Adequate — event filtering, publishing |
| scheduledbackup | 16 | Adequate — cron commands, background processing |
| scheduler | 11 | Adequate — cron parsing, schedule management |
| ratelimiter | 12 | Adequate — rate limiting, middleware |
| kafkaconnect | 11 | Adequate — produce/consume host functions |
| pagerdutyalert | 10 | Adequate — incident trigger/resolve routes |
| auditlog | 7 | Minimal — Info/Init/RegisterRoutes only |
| datadogexport | 5 | Minimal — Info/Init/RegisterRoutes only |
| slacknotify | 5 | Minimal — Info/Init/RegisterRoutes only |
| webhookingest | 5 | Minimal — Info/Init/RegisterRoutes only |
| jobqueue | **3** | Critical gap — 3 tests in 48 lines, zero business logic |
| oauthprovider | **3** | Critical gap — 3 tests in 63 lines, zero token lifecycle tests |
| **CLI tools** | | |
| cleat-gen | 11 | Good — exprToString, parseSpecDir, generateCode |
| cleat-gen-plugin | 4 | Adequate — flag registration tests |
| cleat (main CLI) | **0** | GAP — no tests |
| cleatctl | **0** | GAP — no tests |
| cleat-worker | **0** | GAP — no tests |
| cleat-bench | **0** | GAP — no tests |

---

## Remaining Gaps by Priority

### Priority 1 — Security / Infrastructure Plugins (HIGH)

These plugins handle critical infrastructure with near-zero coverage:

| Plugin | Current | Gap | Why Critical |
|--------|---------|-----|--------------|
| `oauthprovider` | 3 tests | Token issuance, validation, refresh, revocation | Authentication security |
| `jobqueue` | 3 tests | Enqueue, dequeue, scheduling, retry, background worker | Job processing backbone |
| `ratelimiter` | 12 tests (basic) | Actual rate limit enforcement, burst handling, per-tenant isolation | DoS protection |

### Priority 2 — CLI Tools (MEDIUM)

| Package | Why Important |
|---------|--------------|
| `cmd/cleat` | Main CLI: build, dev, init, plugin commands — user-facing |
| `cmd/cleatctl` | Control plane: deploy, version management — operator-facing |
| `cmd/cleat-worker` | Worker binary — runs workflows in production |

### Priority 3 — AI Utilities (MEDIUM)

| Package | Why Important |
|---------|--------------|
| `cleat/ai/llm` | LLM client utilities used by the agent |
| `cleat/ai/pgvector` | Vector database operations |

### Priority 4 — Remaining Plugins with Minimal Tests (LOW)

| Plugin | Tests |
|--------|-------|
| `auditlog` | 7 |
| `datadogexport` | 5 |
| `slacknotify` | 5 |
| `webhookingest` | 5 |

### Priority 5 — E2E / Integration Tests (STRETCH)

No tests exist that:
- Start a real worker process
- Execute a multi-step workflow end-to-end
- Test worker crash recovery (kill -9 worker mid-execution, verify resume)
- Test version migration (v1 → v2 mid-execution)
- Test the full build → deploy → execute → query lifecycle

---

## Proposed Next Steps (Round 2)

### Step 1: Fix oauthprovider tests (1 day)
- Add fake SQL driver for token storage
- Test: create client credentials → exchange for token → validate token
- Test: refresh token flow
- Test: revoke token → subsequent validation fails
- Test: expired token validation

### Step 2: Fix jobqueue tests (1 day)
- Add in-memory job store
- Test: enqueue job → claim job → complete job
- Test: job retry on failure with backoff
- Test: scheduled job fires at correct time
- Test: max retries exceeded → dead letter queue

### Step 3: Add CLI smoke tests (1 day)
- `cmd/cleat`: test `--help`, `build --help`, `dev --help` — verify commands parse
- `cmd/cleatctl`: test `deploy --help`, `versions --help`
- These can be simple flag-parsing tests (like cleat-gen-plugin)

### Step 4: Add ai/llm and ai/pgvector baseline tests (1 day)
- ai/llm: test client creation, provider routing, model selection
- ai/pgvector: test SQL generation for vector operations

### Step 5: Enable coverage gating (0.5 days)
- Set realistic thresholds based on current coverage after steps 1-4
- Make the CI coverage check exit non-zero below threshold

### Step 6: Remaining plugin behavioral tests (3 days)
- auditlog, datadogexport, slacknotify, webhookingest
- Follow the blobstore/kvstore pattern (in-memory store + fake SQL driver)

### Step 7: E2E integration test (3 days, stretch)
- Docker Compose with PostgreSQL + worker
- Build a test workflow WASM, deploy it, execute it, verify result
- Test worker crash recovery

---

## Summary

| Metric | Before (Audit) | After (Round 1) | Target (After Round 2) |
|--------|---------------|-----------------|------------------------|
| Overall grade | C+ | B | B+ |
| Total tests | ~930 | ~1,230 | ~1,400 |
| Failing tests | 61 | 0 | 0 |
| Broken test infra | AS, web | None | None |
| Fake/misleading tests | 3 files | 0 | 0 |
| Zero-test packages | 13 | 7 | 0 |
| Plugins with <6 tests | 8 | 5 | 0 |
| CI silent skipping | Yes | No | No |
| Coverage tracking | None | Exists (non-blocking) | Blocking |
| E2E tests | 0 | 0 | 1+ |
