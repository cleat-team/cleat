# Updated Test Coverage Analysis — 2026-05-07

## What We Fixed (Round 1 — Complete)

| Category | Before | After |
|----------|--------|-------|
| AssemblyScript tests | Broken (`asp: not found`) | 16 tests passing |
| Fake "fault" tests | 3 files claiming to test faults (didn't) | Deleted, replaced with 5 real FaultInjector tests |
| Redundant tests | capabilities_enforcement_test.go duplicated | Deleted |
| Tautological assertions | 2 tests in runner_test.go verified nothing | Rewrote with real behavioral checks |
| Rust compensation test | `t.Log` instead of `t.Error` (always passed) | Fixed |
| Test schemas | 5 copies of CREATE TABLE, 200+ lines duplicated | Consolidated in testutil/schema.go |
| Auth tests | Zero | 38 tests (middleware + tenant store + fake driver) |
| AI agent tests | Zero | 11 tests (happy path, tools, errors, limits, context) |
| Untested packages | 13 packages with no test files | 8 test files added (virtualobject, version, plugingen×4, cleat-gen-plugin, localdev) |
| Rust proc-macro | Zero | 8 integration tests |
| Concurrency tests | Near zero | 4 tests passing under `-race` |
| CI test skipping | Critical tests silently skipped | PostgreSQL + TinyGo + Rust WASM target in CI |
| Coverage tracking | Zero | Makefile targets + CI job + threshold checks |
| Plugin behavioral tests | 3 plugins had only Info/Init/RegisterRoutes | kvstore +12, eventstore +8, notifications +11 (plus bug fix in kvstore/routes.go) |
| Python SDK tests | 40% `assert callable()`, heavily mocked | 123 tests, 47 behavioral via CleatTestHarness |
| Web dashboard | Zero | 79 tests (vitest: 43 cost, 26 API, 10 component) |

**Total: ~300 new tests across 26 files. 3 files deleted. 24 files modified.**

---

## Remaining Gaps (Round 2)

### Tier 1 — Fix pre-existing test failures (HIGH PRIORITY)

5 packages have ALL tests failing. These were broken before our changes — they're tightly coupled to `testdata/` fixture files that have drifted from what the tests expect.

| Package | Failing Tests | Root Cause |
|----------|---------------|------------|
| `internal/analyzer` | 8/8 | Tests load `testdata/basic` and `testdata/errors` packages — fixtures changed |
| `internal/callgraph` | 11/11 | Same — depends on testdata |
| `internal/closure` | 19/19 | Same — depends on testdata |
| `internal/transform` | 10/10 | Same — depends on testdata/autothread |
| `internal/wasm` | 13/13 | Same — depends on testdata |

**Fix:** Either update testdata fixtures to match test expectations, or rewrite tests to be less coupled to specific fixture contents. These are the build/lint pipeline's core analysis packages — having them all red means a regression could land unnoticed.

### Tier 2 — Plugins with critically weak tests (MEDIUM PRIORITY)

These plugins have only Info/Init/RegisterRoutes tests with minimal line counts:

| Plugin | Tests | Lines | What's Missing |
|--------|-------|-------|----------------|
| `jobqueue` | 3 | 48 | Job enqueue, dequeue, scheduling, retry, background processing — zero coverage |
| `oauthprovider` | 3 | 63 | Token issuance, validation, refresh, revocation — zero coverage |
| `datadogexport` | 5 | 89 | Metric export formatting, API calls — zero coverage |
| `slacknotify` | 5 | 89 | Message delivery, templating, error handling — zero coverage |
| `webhookingest` | 5 | 90 | Webhook signature validation, payload parsing, retry — zero coverage |
| `scheduler` | 11 | 134 | Cron parsing, schedule execution, pause/resume — zero coverage |
| `auditlog` | 7 | 128 | Log entry creation, querying, filtering — zero coverage |
| `ratelimiter` | 12 | 228 | Rate limit enforcement, burst handling, per-tenant limits — zero coverage |

These plugins handle critical infrastructure (auth tokens, job processing, rate limiting) with effectively no behavioral coverage.

### Tier 3 — Packages still with zero tests (MEDIUM PRIORITY)

| Package | Concern |
|---------|---------|
| `cleat/ai/llm` | LLM client utilities — integration glue between agent and providers |
| `cleat/ai/pgvector` | pgvector database operations — SQL generation, embedding storage |
| `internal/telemetry` | OpenTelemetry tracing setup — observability |
| `cmd/cleat` | Main CLI — build commands, dev mode, init, plugin management |
| `cmd/cleatctl` | Control plane CLI — deploy, version management |
| `cmd/cleat-bench` | Benchmark CLI |
| `cmd/cleat-worker` | Worker binary |

### Tier 4 — Coverage enforcement (LOWER PRIORITY)

Coverage thresholds are set in the Makefile but are non-blocking (exit 0). After fixing the pre-existing failures:
- Gate PRs on coverage regression (compare against main branch baseline)
- Increase thresholds: 80% for `cleat/`, 70% for `internal/host/`, 60% for `plugins/`
- Add coverage badge to README

### Tier 5 — Integration / E2E tests (STRETCH)

We have unit tests and DB-level tests, but no end-to-end workflow execution tests that:
- Start a real worker process
- Execute a multi-step workflow end-to-end
- Test worker crash recovery (kill -9 a worker mid-execution, verify workflow resumes on another worker)
- Test version migration (run workflow on v1, upgrade to v2 mid-execution)

---

## Next Steps (Recommended Order)

1. **Fix pre-existing test failures** (Tier 1) — 61 failing tests across 5 packages. These are the static analysis pipeline. Fixing them should be the immediate next step since they're already red in CI.

2. **Add behavioral tests for auth-adjacent plugins** (Tier 2, top 3) — oauthprovider, ratelimiter, jobqueue. These are security/infrastructure-critical plugins with near-zero coverage.

3. **Add baseline tests for remaining zero-test packages** (Tier 3) — at minimum ai/llm and cleatctl since those are user-facing.

4. **Enable coverage gating** (Tier 4) — once the failures are fixed and coverage is at reasonable levels, make thresholds blocking.
