# Remediation plan — findings of `REVIEW-2026-08-09.md`

**Written:** 2026-08-09 against `develop` @ `ec88d08`.
**Companion to:** `REVIEW-2026-08-09.md` (what is wrong and how it was proven).
This file is only about **what to do, in what order, and what can run at the same time.**

Priority reflects the owner's framing: this is an engineering artifact, not a commercial
product. Gaps that are purely the absence of a product offering are **out of scope** and are
listed in "Explicitly not doing" at the end so they do not silently re-enter the backlog.

House rules that constrain every item below, from `CLAUDE.md`:
- **One PR, one thing.** Every bundled PR here has been split deliberately.
- **Branch prefixes are exactly** `feature/ bugfix/ fix/ docs/ release/ hotfix/`. Not `feat/`,
  not `test/`. A wrong prefix fails `Validate branch name` and a head branch cannot be
  renamed — each mistake costs a close-and-reopen.
- **Prove every regression test can fail, and read *why* it failed.** Remove the fix, watch
  it go red, confirm it went red for the expected reason, put it back.
- **Build and test with CGO on.** `-p 1` for more than one DB-backed package.
- **Migration number ranges are reserved per stream** (`PARALLEL-WORKSTREAMS.md:108`):
  WS-1 `010–019`, WS-2 `020–029`, WS-3 `030–039`. Streams below inherit a range; do not
  renumber another stream's file.
- **Rebase before every push.** Several streams land in `engine/` on the same days.

---

## The one structural fix that pays for the rest

**Stream A — build every test database from `migrations/<dialect>/*.sql`, and delete
`engine/testutil/*_schema.go`.**

Six of the findings share one cause: the tests do not run against the schema that ships. B1
(MySQL saga/child-workflow inserts rejected) is invisible *only* because
`engine/testutil/mysql_schema.go` declares nullable what the migration declares `NOT NULL`.
This is the same fix #333 applied to MSSQL — which immediately turned 26 subtests red, every
one of them a path that had only ever worked by bypassing the fence — never extended to MySQL
or Postgres.

**This is the keystone: it is a prerequisite for proving B1 fixed, and it will find things
nobody has listed here.** Expect it to go red on landing. That is the point, and it is why it
gets its own stream and its own buffer. Do not widen the schema to make tests pass; fix the
code or fix the migration, and say which.

---

## Wave 0 — do this alone, first, and merge within the hour

Serialized because it touches the whole tree and would conflict with everything.

### W0 · `hotfix/rotate-committed-credential`
**Findings:** B2, S9 · **Effort:** ~1 session · **Depends on:** nothing · **Blocks:** nothing

1. **Rotate the `cleat_sk_…` key at `clew-agent.json:3` before anything else.** Treat it as
   compromised — it has been in tracked history since `14dec5e` (2026-06-06).
2. `git rm --cached` and `.gitignore`: `clew-agent.json`, `clew-executor.plugin`, `bin/`,
   `cmd/cleat-worker/cleat-worker`, root `cleat-worker`, `durable-worker`, `durable-bench`,
   `task_state/`, `plans/`, `prompts/`, `projects/`, `session-rcownie-cleat-0*.json`,
   `ast_transform_demo*.go`, and all 445 `node_modules` files.
3. Note in the PR that `.gitignore` **already listed** several of these — the rules were added
   after the paths were tracked and are therefore no-ops. That is the lesson worth recording.

**Deliberately deferred:** history rewrite (`git filter-repo`) to reclaim the 187 MB `.git`.
It invalidates every outstanding branch and clone. Do it as a scheduled, announced operation
once the waves below have landed, not now.

> **Interaction with worktrees:** if any agent worktrees are already checked out when W0
> lands, `examples/*/node_modules` disappearing will look like a broken tree. Land W0 *before*
> creating the worktrees in Wave 1.

---

## Wave 1 — seven parallel streams

All seven branch from `develop` **after W0 lands**. File ownership below is disjoint by
design; the two coupling points are called out explicitly.

```
                          W0 (credential + hygiene)
                                    │
        ┌──────────┬──────────┬─────┴────┬──────────┬──────────┬──────────┐
        │          │          │          │          │          │          │
        A          B          C          D          E          F          G
   test-schema  security   dashboard  fencing    replay      docs       CLI
   from migr.   scoping    auth       tokens     integrity   truth     dialects
        │                                │
        └──► A2 (MySQL NOT NULL fix,     └──► D2 (zombie-writer regression
             provable only after A1)          test, needs D1's fencing token)

   Merge-order coupling only:  A ↔ B  (both add migrations)
                               C ↔ F  (both would touch web/README.md — C owns it)
```

### A · Test-schema truth — `feature/test-db-from-migrations`
**Findings:** B1, and the cause behind six others · **Effort:** 3–5 sessions
**Owns:** `engine/testutil/*_schema.go`, `migration/`, `migrations/mysql/*`
**Blocks:** A2 · **Blocked by:** W0

- **A1.** Replace `engine/testutil`'s hand-written Go schemas with a bootstrap that runs the
  real `migration.Runner` over `migrations/<dialect>/`. Delete the Go copies. Land this
  **alone**, with the resulting failures triaged in the PR body — do not fix them in the same
  PR, per one-PR-one-thing.
- **A2.** Fix B1 itself. Two candidate fixes; pick one and justify it:
  (a) relax `service`/`operation`/`request` to nullable in a new MySQL migration, matching
  Postgres and MSSQL; or (b) stop `nullStr()` collapsing `""` to `NULL` for these three
  columns. (a) is the smaller change and restores dialect parity, which is the actual
  invariant. Regression test must go red on a database built from the shipped migration.
- **A3** *(same stream, after A2)*: add a CI guard that fails if `engine/testutil` ever
  reintroduces a `CREATE TABLE`. This is the mechanism, not the sweep.

> **This stream is the long pole.** Start it first and give it the most capable agent.

### B · Tenant-scoping and RLS — `fix/version-handler-tenant-scoping`
**Findings:** B3, S1 · **Effort:** 2–3 sessions
**Owns:** `engine/version_handler.go`, `cmd/cleat-worker/main.go` (registration site only),
`migrations/{postgres,mssql}/03x_*` · **Blocked by:** W0

- **B1s.** Route `RegisterVersionHandler` through the same `storeFor`/`tenantFor` path every
  other handler in `server.go` uses. Add the multi-tenant test that
  `engine/version_handler_test.go` lacks — assert a second tenant gets 403/404 on
  `purge`, not silence.
- **B2s.** Add RLS policies for the six unprotected `tenant_id` tables (`concurrency_keys`,
  `idempotency_keys`, `workflow_update_requests`, `kv_store`, `feature_flags`,
  `tenant_api_keys`) on Postgres, plus `dbo.workflow_promises` on MSSQL. Use migration range
  `030–039`.
- **B3s.** Add `tenant_id` to the three `idempotency_keys` UPDATEs at
  `engine/store_lifecycle.go:361,418,526`.

> **Verify at the right layer.** `CLAUDE.md`'s warning applies directly: the Postgres test
> role is `SUPERUSER BYPASSRLS`, so a cross-tenant assertion can pass against a wide-open
> policy because the store's own SQL carries `tenant_id = $N`. Break the Go filter
> specifically and confirm the policy alone still holds.

**Coupling with A:** B2s adds migrations; A1 changes how migrations reach test databases.
Whichever lands second rebases. Low risk — different files — but sequence the *merges*.

### C · Dashboard authentication — `fix/dashboard-sends-auth-header`
**Findings:** B5 · **Effort:** 1–2 sessions
**Owns:** `web/`, `charts/cleat/templates/`, `web/README.md` · **Blocked by:** W0

- Add `Authorization: Bearer` to `fetchJSON` in `web/src/lib/api.ts` (one wrapper — every
  call site inherits it), sourced from a real, documented mechanism.
- Fix `web/README.md:121`, which documents an `AUTH_TOKEN` variable that does not exist.
- Fix the Helm preStop drain hook, which 401s and is swallowed by `|| true`, so graceful
  drain silently never fires.
- Decide and document the `webhookingest` / `oauthprovider` exemption (S6) — those two
  register externally-reachable routes behind `auth.Middleware`, which exempts only
  `/healthz` and `/metrics`. Either add a public-path mechanism or state that those plugins
  require a separate listener. **Do not** resolve it by recommending `--require-auth=false`.

**Fully independent** — no Go files. Ideal for a parallel agent with no rebase pressure.

### D · Write-path fencing — `fix/fence-per-step-event-writes`
**Findings:** B4 · **Effort:** 3–4 sessions
**Owns:** `engine/flush.go`, `engine/adaptive_flush.go`, `engine/store_intent.go`
**Blocked by:** W0 · **Related:** S7 (this code has 0% coverage today)

- **D1.** Carry `generation` (or `assigned_to`) into the per-step flush and write-ahead-intent
  paths and gate the writes on it, mirroring `store_lifecycle.go`'s terminal writes.
- **D2.** Regression test: a reaped worker attempting a late flush must be rejected, not merged
  via `ON CONFLICT … DO UPDATE`. **Do not build this on a sleep.** `PARALLEL-WORKSTREAMS.md`
  records a 2 ms sleep in the zombie-writer scenario that survived four CI runs and lost the
  fifth — drive the generation bump explicitly instead of racing it.
- **D3.** Decide whether the generation-checked `Heartbeat` (`engine/db.go:236`), currently
  dead in production, should be wired or deleted. `BatchHeartbeat` deliberately skips the
  check; if that stays, `Heartbeat` is a trap for the next reader.
- **D4.** Write the missing `adaptive_flush_test.go` (S7) — this stream is already in the file.

### E · Replay integrity — `fix/replay-preserves-non-retryable`
**Findings:** S4 · **Effort:** 2 sessions
**Owns:** `engine/compaction.go`, `engine/heartbeats.go` · **Blocked by:** W0

Elevated above its apparent severity: these are holes in the **flagship guarantee**, not
ordinary bugs. Engine-introduced non-determinism is the one defect class this architecture
exists to make impossible.

- **E1.** Add `ErrNonRetryable` to `CompactedEvent` and preserve it through
  `extractCompactionState` / `buildFullHistoryFromCompaction`.
- **E2.** Make `replayCallWithHeartbeat` (`engine/heartbeats.go:205`) honour the persisted
  class, and delete the stale comment claiming it "was never persisted" — line 124 makes that
  false.
- **E3.** The existing replay fixture at `heartbeats_test.go:169` leaves `ErrNonRetryable`
  unset, which is why it cannot catch this. Fix the fixture, then confirm it goes red.
- **E4.** Extend the compaction fuzz test to cover the field, so the next added field is
  caught by construction rather than by review.

### F · Documentation truth pass — `docs/reconcile-claims-with-tiers-yaml`
**Findings:** B7, and the whole claim-vs-reality table · **Effort:** 2–3 sessions
**Owns:** `README.md`, `ARCHITECTURE.md`, `docs/`, `LANGUAGE_SUPPORT.md`, `ABI.md`,
`DX_COMPARISON.md` · **Blocked by:** W0

Split into three PRs, because they have different risk:

- **F1.** *Things that are wrong and dangerous.* `docs/explanation/security-model.md`
  describing RLS and resource limits as unimplemented; `README.md:63` on RLS;
  `README.md:68` crediting wazero as the runtime; `web/README.md`'s fictional `AUTH_TOKEN`
  (coordinate with stream C, which owns that file — let C take it).
- **F2.** *Things that are wrong and merely embarrassing.* The `internal/` paths in
  `ARCHITECTURE.md` and `docs/reference/database-backends.md` (including a documented test
  command that cannot run); the Postgres 14/16 and SQL Server 2017/2022 splits; the stale
  SDK line counts (~3–4× off across five sites); `ABI.md`'s 52-vs-58/59 host-function count
  and its seven undocumented functions; `LANGUAGE_SUPPORT.md` describing three shipped SDKs
  as future work.
- **F3.** *The onboarding path.* Fix `cleat dev start` and `cleat version` — either add the
  subcommands or fix the docs; adding `version` is trivial and worth doing. Fix the
  `place_order.wasm` / `cancel_order.wasm` mismatch and the `POST /api/workflows` route in the
  README quick start. **Add the missing migration step** — no onboarding doc mentions applying
  migrations at all, so a new reader hits `relation "workflow_defs" does not exist`.
- Also correct `tiers.yaml`'s own TLA+ claim: line 53 of `specs/CleatClaim.tla` is `\* ===…`,
  a **line comment**, not a module terminator. The narrower true claim (no `.cfg`, TLC never
  runs) stands.

**Zero code conflict with any other stream** — but F2 is the one place a doc must be re-read
after A/B/D/E land, since those change what is true.

### G · Dialect claim vs shipped CLI — `fix/cli-dialect-claim`
**Findings:** B6 · **Effort:** 1 session (narrow) or 4–6 (build)
**Owns:** `cmd/cleat/` · **Blocked by:** W0

`cmd/cleat/` has zero `mysql`/`mssql` references and ten hardcoded `sql.Open("postgres", …)`
sites, while the README claims three backends.

**Recommendation: narrow the claim, do not build the feature.** Make `cleat deploy/versions/
rollback` fail with a clear "the CLI supports PostgreSQL only; see cmd/deploy-workflow" rather
than a driver error, and say so in `README.md` and `tiers.yaml`. The engine genuinely supports
three dialects; only the CLI does not, and absent an adoption funnel the honest one-session
fix beats the six-session one. Revisit if MySQL/MSSQL deployment ever becomes a real workflow.

---

## Wave 2 — after Wave 1 lands

Sequenced because each depends on a Wave-1 outcome or on a quieter tree.

| Stream | Branch | Findings | Depends on | Notes |
|---|---|---|---|---|
| **H** | `feature/completed-workflow-retention` | S2 | A (schema truth) | Retention for completed instances + indexes for the `ILIKE` list path. Needs migrations; A must have settled how test DBs are built. |
| **I** | `fix/drop-tenant-deletes-tenant-data` | S3 | B (RLS) | `admin.drop_tenant` must delete the ten data tables, gain a Go caller, and get an MSSQL counterpart. Interacts with B's new policies. |
| **J** | `fix/mssql-request-column-width` | S5 | A | `NVARCHAR(255)` → `MAX`, plus the three missing MSSQL indexes (S10). Provable only once tests use the shipped schema. |
| **K** | `fix/wazero-fallback-is-explicit` | S10 | — | Make the wasmtime→wazero fallback fail loudly on anything other than "CGO absent", and correct the `engine/executor.go:129` comment and `--wasm-instance-timeout` help text, which both overclaim. |
| **L** | `fix/remove-unwired-query-handler` | positioning gap 3 | — | `RegisterQueryHandler` appends a name to a slice and nothing calls back. Wire it or delete it — follow the `isReplaying()` precedent (#448), which handled this exact shape correctly by deletion. |
| **M** | `fix/scheduledbackup-claims` | S8 | — | Remove the S3 and restore claims from the doc comments, or implement them. Convert `Config.DSN` to `plugin.Secret` and stop passing it as an `exec` argument. |
| **N** | `fix/observability-gaps` | S10 | — | Correct the two wrong metric names in the Grafana dashboards; add a step-retry counter and store-pool `.Stats()` instrumentation. |
| **O** | `fix/dead-code-and-guards` | S7, S11 | D | Delete `engine/batch_flush.go` and `plugin/audit.go` (zero callers anywhere); fix `wasmrw.Error`, which ignores its argument and returns a constant `1`; extend the dead-code guard to catch called-from-nowhere, which no current guard covers. |

**Deliberately unscheduled:** the errcheck/gosec backlogs (890/678). `CLAUDE.md` is right that
a backlog of 200 similar findings is usually one missing abstraction. Do not sweep these; pick
the subset in the paths Wave 1 touches and let the rest wait for a mechanism.

---

## Parallelism: how to actually run this

**Worktrees.** One per stream, all branched from `develop` after W0:

```sh
git worktree add ../cleat-wt-A feature/test-db-from-migrations
git worktree add ../cleat-wt-B fix/version-handler-tenant-scoping
# ...one per stream
```

**Databases are the real contention, not files.** `engine/testutil`'s
`CleanupPostgresTestData` is an unqualified `DELETE FROM` across eleven tables. Two agents
running DB-backed packages against one database will delete each other's fixtures mid-test,
and the failures will look like unrelated flakes. Two options:

- **Per-stream databases** (preferred): each worktree gets its own Postgres/MySQL/MSSQL
  containers on distinct ports, following the `PARALLEL-WORKSTREAMS.md` pattern.
- **Or** serialize DB-backed test runs: only one agent runs `go test ./engine/...` at a time.
  Streams C, F and G need no database at all and can always run concurrently.

**Which streams are safe to run fully concurrently:**

| | A | B | C | D | E | F | G |
|---|---|---|---|---|---|---|---|
| **A** | — | merge-order | ✅ | ✅ | ✅ | re-read after | ✅ |
| **B** | | — | ✅ | ✅ | ✅ | re-read after | ✅ |
| **C** | | | — | ✅ | ✅ | owns `web/README` | ✅ |
| **D** | | | | — | ✅ | ✅ | ✅ |
| **E** | | | | | — | ✅ | ✅ |
| **F** | | | | | | — | ✅ |

Only two real couplings: **A ↔ B** (both add migrations — sequence the merges, not the work)
and **C ↔ F** (both would touch `web/README.md` — give it to C).

**Suggested agent allocation**, three concurrent agents rather than seven, because review
capacity is the bottleneck and `develop` moving under seven branches costs more than it saves:

- **Agent 1 (strongest):** A → then H, J. The keystone and its dependents.
- **Agent 2:** D → E → then O. All durability-semantics work, one mental model.
- **Agent 3:** B → C → then I. Security and the auth surface.
- **Opportunistic:** F and G need no database and little review; slot them between merges.

---

## Definition of done

A stream is done when, and only when:

1. The regression test was **proven to fail** with the fix removed, **and the failure reason
   was read** and matches the diagnosis. `CLAUDE.md`: twice this has caught a test passing
   for the wrong reason.
2. The layer under test is the layer holding the test up — verified by breaking that specific
   layer, not an adjacent one.
3. No timing-dependent assertion was introduced. If an assertion depends on wall-clock time,
   the timing was removed rather than widened.
4. Every number written into a doc carries a date and the command that re-derives it.
5. `tiers.yaml` was updated if the change moves anything across a tier boundary, and the prose
   describing the fix was updated — not just a status marker. A ✅ over a stale body is worse
   than no marker.

---

## ~~Open follow-up~~ — wazero removal, part 2 — ❌ **DECIDED AGAINST 2026-09-01. Do not start this.**

> Everything below describes work that will not happen; it is kept as the record of why.
> See `IMPROVEMENT-PLAN.md` §3.56 for the decision and §3.30 for the question it closes.
>
> Two things changed after this was written. #503 made `Engine.resolveBackend` fail closed on a
> module whose declared language matches no backend — the guest-controlled path that made the
> wazero fallback a safety problem — so the safety case for removal is gone. And all three
> released binaries build CGO-free today: removing wazero forces `cleat` onto wasmtime and
> therefore onto CGO, ending pure-Go cross-compilation for the CLI, for no correctness gain. It
> would also break exported API (`engine.Runtime`, `engine.NewRuntime`,
> `wasmtest.WasmTestEnv.Runtime()`).
>
> wazero stays, scoped to CLI and dev tooling. The price is
> `engine/hostabi_runtime_parity_test.go`, which compares the two host-ABI implementations and
> found a real defect on its first run (§3.55). **The stash referenced below is stale** and is
> reference material, not something to apply: its WIP message records "wazero imports 4 left",
> while the tree today has **31 files importing wazero, 20 of them non-test** (measured
> 2026-09-01 — `grep -rl 'tetratelabs/wazero' --include='*.go' . | grep -v node_modules | wc -l`,
> and again with `grep -vc _test.go`).

**Part 1 is on `develop`** (`engine/backend_wazero.go` deleted, `--allow-wazero-fallback`
removed, worker-level fallback gone; build + vet + all three dialects + every `cmd` package
green). **Part 2 is deliberately NOT merged** and is stashed in the `cleat-wt-W` worktree as
`stash@{0}` — "W commit2 WIP round2".

Part 2 is the wide mechanical collapse and it got roughly 85% of the way:

| | at stash time | re-derive |
|---|---|---|
| files with `api.Module` | 21 → **3** | `grep -rl 'api\.Module' --include='*.go' engine/ wasm/ \| wc -l` |
| files importing wazero | 8 → **4** | `grep -rl 'tetratelabs/wazero' --include='*.go' . \| grep -v node_modules \| wc -l` |
| `//go:build cgo` files | **38** remaining | `grep -rln 'go:build cgo\|go:build !cgo' --include='*.go' . \| grep -v node_modules \| wc -l` |
| `go.mod` requires wazero | **yes** | `grep -c wazero go.mod` |

**Why it was parked rather than finished.** `go build ./...` passed but `go vet ./...` did
not: deleting the wazero-specific test helpers removed `newTestHostFuncHarness`, `wasmI32`,
`wasmI64`, `minimalMemoryWasm` and `engine.Runtime`/`engine.NewRuntime`, which are still
referenced by `benchmarks/wasm_bench_test.go`, `tests/integrity/ambiguity_detection_test.go`,
`tests/cross-language/cross_language_test.go`, `engine/json_hostfuncs_test.go` and
`engine/lifecycle_test.go`.

Each of those needs a **judgment call, not a mechanical edit**: was the test exercising
wazero specifically (delete it), or exercising engine behaviour through a wazero harness
(port it to wasmtime)? Getting that wrong silently deletes coverage, which is the exact
failure class this whole review exists to catch. Finishing it under a session limit was the
wrong trade.

**Do it as its own stream**, with the full three-dialect sweep plus the `cleat/` module as
the gate, and treat the per-test delete-or-port decision as the deliverable rather than the
line count.

---

## Explicitly not doing

Recorded so they do not silently re-enter the backlog. These follow from the owner's framing:
this is an engineering artifact, not a commercial product.

- **No managed cloud, no community, no ecosystem, no case studies.** Not gaps.
- **Not closing the effort-to-first-success gap against `temporal server start-dev`.** The one
  exception is `cleat dev start` failing outright (F3) — that is a broken command, not a
  competitive disadvantage.
- **Not building MySQL multi-tenancy.** The 6.1× scan cost was measured and the decision
  (D1, 2026-08-06) is sound. It stays a documented boundary.
- **Not building indexed search attributes.** Revisit only if list latency actually bites.
- **Not sweeping the errcheck/gosec backlogs.** Wait for the mechanism.
- **Not rewriting git history yet** — scheduled, announced, after Wave 1.

## One thing worth doing that is not a defect

**Measure the throughput ceiling once, properly, and write it down with the hardware and the
command.** `benchmarks/comparative/results/` is entirely placeholder tokens, and the only
figures anywhere are design-doc estimates (~312–1,562 claims/sec, claim path ~1/8 of the
INSERT ceiling). Nobody — including the author — currently knows where this falls over. Given
how much of this repo's discipline is built on "no number without a re-deriving command", the
central performance number being an estimate is the most conspicuous gap in that discipline.
