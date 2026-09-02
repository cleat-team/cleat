# WS-2 status — durability: crash recovery and replay

**As of 2026-08-06, round 2.** Sandbox `/localssd/rcownie/cleat-agent1`. Migrations `020–029`.
PostgreSQL `5433`, MySQL `3307`, SQL Server `1434`.

Read `PARALLEL-WORKSTREAMS.md` first for the ownership map, then this, then WS-2's items in
`IMPROVEMENT-PLAN.md`. This file is the short version: what is done, what is next, and what a
future session should not have to rediscover.

> **Re-derived 2026-09-02 against `develop` at `81d8f8b`. Two corrections, then the current
> list.** The "This round" sections below are still accurate — they describe work that landed
> and the traps it surfaced, and they are the reason to keep this file. Two things are not:
>
> - **The `020–029` block is spent, and the block scheme is retired** (#563). Take the next free
>   number above the dialect's high-water mark — `ls migrations/<dialect>/*.sql | tail -1`. As of
>   2026-09-02 that is postgres `034`, mysql `033`, mssql `037`, and they are not aligned: `033`
>   is a different migration in each dialect.
> - **"Standing constraints" says `CGO_ENABLED=0` "silently runs everything on wazero".** It
>   does not, as of #459 (2026-08-10): `engine/backend_wazero.go` was deleted and there is no
>   backend left at all, so `cleat-worker` exits 1 at startup rather than running unfenced. The
>   conclusion is unchanged — a result obtained that way is not evidence about the engine — but
>   the mechanism in that bullet is wrong and has been for three weeks. Corrected in place below.
>
> **Of the six items in "Open, in the order I would take it", one is done and one moved.**
> §3.24 was fixed 2026-08-31 and is listed first; §2.60d is now carried on WS-1's board, since
> the mechanism lands in `engine/testutil/`. The four that remain are real — each re-checked
> against the tree, not against the plan's status markers:
>
> | item | evidence it is still open |
> |---|---|
> | §1.4 phase F | `engine/store_admin.go` implements force-complete/fail; nothing resolves a *pending* step. |
> | §2.35 residual | `event_history` has `error TEXT` and no `error_code` column — `awk '/CREATE TABLE.*event_history/,/^\);/' migrations/postgres/001_schema.sql`. |
> | `AdminReReplay` | still in `engine/store_admin_stubs.go`. |
> | `pendingSentinel` | still in `engine/types.go:399`, exported as `PendingSentinel` for `tests/integrity`. |
>
> **And there is a new first item that is not on the list below: the durable-record shape for
> §3.35 phase 5.** WS-3 has phases 1–4 of `defer` shipped and is deliberately not starting
> phase 5, because its resumable record is the same shape as §1.4's and building a second
> answer is what the one-stream rule exists to prevent. That makes it WS-2's to answer, it is
> a written design rather than a PR, and it should come **before** §2.35 — both touch the same
> rows, and doing them in the other order means designing the record twice.
>
> Of the "Not mine, and blocked on WS-3" pair below, **§3.23 landed** (fixed 2026-08-31). **A
> guest-visible `CallErrorAmbiguous` did not** — `grep -rn CallErrorAmbiguous .` is empty and
> the enum still ends at `CallErrorPermissionDenied`. What did change is that adding a member
> is now a bounded job rather than a hunt through five SDKs: `engine.GuestCallErrorCodes()` is
> the single source and `cleat/callerror_contract_test.go` fails if an SDK's hand-written copy
> drifts from it. Still worth doing, still not what stands between an ambiguous crash and an
> operator being told about it.

---

## This round

### §3.20 admin force-resolve — done, PR #297

`AdminForceComplete` / `AdminForceFail` were stubs returning `not implemented yet` on all
three dialects while `cmd/cleat-worker/api_admin.go` routed to them behind the `X-Confirm`
guard and §1.7's ownership check. Real bodies now, on all three dialects: a status write
fenced on generation but not `assigned_to`, a generation bump that fences the old owner, an
`admin_action` audit event appended through `appendEventsInTx` so it joins the checksum
chain, and the usual post-commit cleanup — all in one transaction.

Full detail is in `IMPROVEMENT-PLAN.md` §3.20. The three things worth carrying forward:

- **The handlers applied the operation to the wrong store.** Ownership was checked against the
  caller's tenant-scoped store and the operation then ran on `s.store`, the process-wide one.
  Invisible while the methods were stubs, and invisible in tests because `newTestAPIServer`
  serves every tenant from one mock.
- **`eventRecordToPayload` had no `admin_action` arm**, so the audit record would have sat
  outside the checksum entirely. `verifyShadowColumns` looks like it would catch that and does
  not: `populateFromPayload` only overwrites keys the payload *carries*, so an absent key
  inherits the column's value and always compares equal. An empty payload reads as agreement.
- **The audit append can be silently displaced** by a concurrent writer taking the step
  number, because every dialect's append is an upsert that leaves an existing row alone.

**Two falsifications corrected the work rather than confirming it**, which is the argument for
doing them one mechanism at a time:

1. The collision test passed at first for the wrong reason — the confirm lookup was
   tenant-scoped and returned "no rows" for the planted row, never reaching the comparison it
   existed to exercise. The lookup is now by primary key, which is the right scope for "is the
   row at this step mine".
2. The payload arm's justification named `verifyShadowColumns`. Removing the arm left every
   database test green on all three dialects and failed only the unit test.

### §1.4 phase D — done, PR #308

Write-ahead call intent, end to end. Migration `020` on three dialects adds
`event_history.intent_at`; an event is pending iff `intent_at IS NOT NULL AND checksum IS
NULL`; and the ordering is the whole feature:

    commit intent  ->  dispatch  ->  commit outcome

The three defects that made the deleted `flushCallIntent` unwirable are gone by construction —
no sentinel in `error`, no checksum on a pending row, and a chain computed from
`s.lastChecksum` rather than read back from the database. Full detail in `IMPROVEMENT-PLAN.md`
§1.4 phase D.

**The assertion that matters is made from inside the call.**
`TestDurableCall_CommitsIntentBeforeDispatch` reads the history back through the store *while
the call is in flight* and requires the pending row to be visible. An implementation that wrote
the intent afterwards passes every after-the-fact check and fails that one.

**Reachable from the shipped worker**, via `--write-ahead-intent-ops`. Without a flag this
would have been §1.4's own shape: durability code that is tested, believed and unreachable.

Three things worth carrying forward:

- **A design-doc claim was wrong.** §5 said `--no-per-step-flush` "defeats this entirely" and
  required rejecting the combination at startup. True of an implementation routing the intent
  through `flushEvent`; this one writes through the store and never consults the flag. A test
  now asserts they are orthogonal on all three dialects, and the doc is struck through rather
  than quietly ignored.
- **MySQL's `RowsAffected` counts rows *changed*, not matched.** The completion-fence test
  passed on MySQL for that reason alone — re-completing with identical values reports 0 whether
  or not the guard is in the `WHERE` clause, so the assertion would have held against a store
  with no fence. The second completion now carries a *different* outcome. Any three-dialect
  test that asserts on a row count needs to change a value to discriminate on MySQL.
- **The MSSQL filtered index on `checksum IS NULL` is legal** despite `checksum` being
  `NVARCHAR(MAX)`. I expected SQL Server to reject a LOB column in a filtered-index predicate.

### §1.4 T3 — the crash scenario — done, PR #316

The evidence phase D shipped without. A real SIGKILL, same fixture as §2.4, one flag different:

| | Ship | what recovery did |
|---|---|---|
| at-least-once | **2** | repeated a call that may already have happened |
| `--write-ahead-intent-ops payments.Ship` | **1** | did not repeat it |

The pending row is asserted *before* the crash, so the claim is about ordering. Removing the
flag fails the test at that assertion.

**It found §3.22, which was the more important half** — now fixed across #328, #331 and #343.
Three separate defects sat between an ambiguous crash and an operator:

1. `wasm/exports.go` wrapped `encodeJSONString`'s output in quotes, and that helper already
   quotes. The result was `{"error":""…""}` — invalid JSON. **Fixed** (#331), along with an
   adjacent unmarshal-error emission that concatenated `err.Error()` with no escaping at all.
2. `FinalizeWorkflowSegment` replaced any unstorable result with `{}` silently, in a two-line
   conditional with no log statement, in all three stores. **Fixed** (#328) — it still
   replaces, because failing the terminal write would lose a whole workflow over a formatting
   defect, but it now says so with the workflow ID and the discarded value.
3. The wasmtime backend reported a guest that said it had failed as a success. **Fixed**
   (#343) — see the §3.22 section below.

**My write-up of the third went wrong twice, in opposite directions.** First I inferred "a
second `cleat_complete` with status 0 overwrites the first" from two probe lines. Then I
retracted it, because instrumenting `Engine.Replay`'s boundary showed **one** execution
returning `err == nil` with a 233-byte result — which I read as ruling out two completions. It
does not: that is precisely what two completions predict, and the 233 bytes were the payload.
**The retraction was the error, and it is the one that cost more**, because it sent the next
session looking for a second mechanism that was never there. A measurement that cannot
discriminate between two accounts is not evidence against either.

**Three things that cost me runs, for whoever continues:**

- `engine/imports.go`'s `cleat_complete` is the **wazero** host set. A CGO build never reaches
  it — `engine/wasmtime_hostfuncs.go` is the one that runs. Instrumenting the wrong one shows
  nothing at all, which reads like "the guest never completed".
- Probes inside the worker go to its captured log, which the harness prints **only on
  failure**. A passing test shows nothing; add a `t.Logf` of `second.output()` while debugging.
- Two probe lines are not a mechanism. Both of my wrong inferences this session came from
  reading a sequence of observations as causation without instrumenting the boundary that
  would settle it.

### §1.4 phase E — automatic resolution — done, PR #335

`AmbiguityResolver`: when replay finds a pending intent row, ask the service about the key the
original attempt sent. If it answers, write the outcome over the row and carry on — the crash
lost the answer, not the effect.

**The resolution is persisted, and that is the load-bearing part.** Using an answer without
recording it means the next replay asks again, and a service that answers differently makes
the same step resolve two ways. `ResolveCallIntent` is separate from `CompleteCallIntent`
because it runs during replay, where the session holds no checksum chain, so the previous
checksum is read from the row before it inside the same transaction — safe here precisely
because everything before a pending row is persisted by definition.

Four ways of declining, all leaving the ambiguity exactly as it was: no resolver, no record, a
resolver that errored, and a resolution that could not be recorded. The last has its own test
because using it would be a determinism bug.

**Phase F** (admin force-resolve for a pending step) is unblocked and unstarted: §3.20 built
the force-resolve path and `adminForce` in `engine/store_admin.go` is the shape it extends.

`pendingSentinel` is still detected alongside `Pending` because `tests/integrity` exercises it
directly; retiring it is still open.

### §3.22 — a returned error was stored as a success — done, PR #343

Closes §3.22. **Every Go workflow that returned an error was recorded `status='done'`**, error
text in the result column — cancellation and replay divergence as much as ambiguity.

A failing Go guest reports itself twice: `cleatDispatch` calls `cleat_complete(1, err)` and
*then returns the same error as its `[]byte` result*, which generated `main()` re-reports under
status 0 with no branch, having no way to know the dispatch failed. Both reach the host, which
keeps them apart correctly, and `backend_wasmtime.go` preferred the status-0 one. Fixed by the
precedence — which is what the direct-export branch 100 lines below and the wazero backend
already did. **Go on wasmtime, primary language on primary backend, was the only path that
collapsed the distinction.**

Traps were never affected: a trapped guest never reaches `cleat_complete`, so fuel and epoch
exhaustion still hit the resource-limit check. It was specifically the guest that stopped
cleanly and *said* it had failed that was not believed.

**Two of my own claims were wrong and are corrected in the plan.** (a) "This needs an ABI
change" — it does not; the wire format was never involved, both completions already crossed it
with the right status bits. (b) I had retracted the two-completions hypothesis as disproved by
an instrumented boundary showing `err == nil` with a 233-byte result. That observation is
exactly what the hypothesis predicts — the 233 bytes *were* the payload. **The retraction was
the error.** A measurement that cannot discriminate between two accounts is not evidence
against either.

Also corrected: the design doc's "`ErrAmbiguous` already exists as error code 5" is **true**,
for `engine/errors.go`'s host-side `ErrorCode`. I had checked it against `engine/callerrors.go`
— a different enum — and reported the mismatch as the doc being wrong. Three things carry that
name; the plan now lays them out.

---

## What to know about CI and the databases

**1. The engine matrix job had no `-p 1`, and the sibling job's comment already explains why
it needs one.** `./engine/...` is two packages; `engine/testutil`'s own tests call
`CleanupPostgresTestData`, an unqualified `DELETE FROM` eleven tables including
`workflow_defs` and `workflow_instances`, against the one database the job provides. In
parallel they delete each other's fixtures mid-test. Fixed in #297 by adding the flag the
Cluster job has carried all along.

The failures it produces do not look like one cause: a workflow that vanishes between two
statements, and a foreign key to a definition deleted milliseconds after it was deployed.
They read as flakes. **Not reproduced locally** — `go test -race ./engine/...` passes here and
two attempts to force the interleaving also passed. The diagnosis is from the code and the
sibling guard, not a repro. §2.60d is the real fix.

**2. Another stream's migration landed in WS-2's database.** PostgreSQL on 5433 had
`idempotency_keys` with WS-1's `(key_hash, tenant_id)` primary key while this checkout still
had `ON CONFLICT (key_hash)`, failing ~20 engine tests. Confirmed against a clean `develop`
before touching anything, so it was contamination and not a regression. **When a schema change
lands on `develop`, recreate the databases in every sandbox** — otherwise the next session
spends its first hour on someone else's migration.

**3. A merge's own `develop` run can be cancelled by the next merge.** #308's runs on
`develop` show `cancelled`, not `success`, because WS-3 merged forty seconds later and the
branch's concurrency group killed the in-flight jobs. Cancelled is not passed. On a branch
three streams are merging into, "verify `develop` after merging" means verifying the *current
head* — which contains your commit — not your own SHA.

**4. `gh run list --commit <sha>` returned zero runs for a SHA whose runs existed.** After a
force-push it reported nothing for several minutes while `gh run list --branch <branch> --json
headSha,name,status` showed five workflows on that exact SHA. Last round's habit — "verify by
SHA" — is right, but `--commit` is the unreliable way to do it. Use `--branch` and filter on
`headSha` yourself.

**5. `check-test-only-code.sh` cannot see any caller behind `//go:build cgo`.** It forces
`CGO_ENABLED=0` — required by its `GOOS=linux` cross-compile — which hides the whole wasmtime
backend, so a helper called only from there is reported as code that only tests reference.
`contextWithRawMemBuf` was already baselined for exactly this and nothing said so;
`guestErrorText` joined it in #343, and the scan now carries a note naming both. **Before
acting on one of these findings, check whether the callers are cgo-gated** — the guard's own
advice ("wire it in or delete it") would have you delete live code from the primary backend.

**Also, once more: `''` in a Go doc comment is not gofmt-stable under Go 1.26.** The comment
formatter rewrites it into a typographic quote pair. Quoting SQL string literals in comments
needs rephrasing, not escaping.

---

## Open, in the order I would take it

> **Superseded 2026-09-02 — read the banner at the top of this file for the current order.**
> Item 1 is done, item 4 moved to WS-1, and a new first item (the §3.35 phase 5 record shape)
> sits ahead of all of these. The list is kept because items 2, 3, 5 and 6 are still open and
> the reasoning under each of them is still the reasoning.

1. ~~**§3.24 — an ambiguous outcome classifies as `unknown`.**~~ **Done, 2026-08-31.** `engine.ErrAmbiguous` has existed
   since the first commit and `NewAmbiguousError` is called by nothing but its own test, so
   nothing can query for the one failure class that needs a human to go and look at the
   external service. Unblocked by #343 and cheap: `cmd/cleat-worker/setup.go:1703` already does
   `errors.As(err, &ce); errorCode = ce.Code.String()`. What is missing is the engine wrapping
   the ambiguous failure as `*CleatError{Code: ErrAmbiguous}` on the way out. **No ABI change.**
   Same shape as §2.35's residual, so they are worth doing together.
2. **§1.4 phase F** — admin force-resolve for a pending step. §3.20 built the force-resolve
   path; `adminForce` in `engine/store_admin.go` is the shape it extends.
3. **§2.35 residual** — persist `error_code` per event so replay recovers the classification
   instead of re-deriving one bit of it.
4. **§2.60d** — test isolation. **Moved to WS-1's board, 2026-09-02**, because the mechanism
   lands in `engine/testutil/` and the recommendation below is a tenancy design choice. Still
   the reason a green run on a reused database means little, and the reason the engine matrix
   job needed `-p 1`. The recommendation is unchanged and travels with it: a tenant per package
   with tenant-scoped deletes, because it is the only option that makes the suite exercise
   tenant scoping rather than route around it.
5. **`AdminReReplay`** is still a stub and now answers 501 rather than 500. It needs D–F's
   replay semantics, not a fourth `UPDATE`.
6. **Retire `pendingSentinel`** — still detected alongside `Pending` because `tests/integrity`
   exercises it directly.

**Not mine, and blocked on WS-3:**

- **§3.23** — a guest-returned error now reads `wasm trap: host: export "…" failed: <error>`.
  `resolveWasmTrap` prefixes every message reaching it, which was right until #343 gave it a
  second class of error, so this is that change's debt. The fix belongs in
  `engine/executor.go` — WS-3's, *and* touched by their in-flight defers branch.
- **A guest-visible `CallErrorAmbiguous`.** `cleat.CallErrorCode` has no ambiguous member, so a
  workflow author's `switch e.Code` sees `[0]`, Unknown. Value 6 is free and the wire field is
  32 bits, but every SDK carries its own copy of the enum — `python-sdk/cleat_sdk/host_calls.py`
  has a literal `{0..5}` dict. Worth doing; **not** what stands between an ambiguous crash and
  an operator being told about it.

---

## Environment

```bash
export CLEAT_TEST_POSTGRES='postgres://cleat:cleat@localhost:5433/cleat?sslmode=disable'
export CLEAT_TEST_MYSQL='root:cleat@tcp(127.0.0.1:3307)/cleat?tls=false&parseTime=true&multiStatements=true'
export CLEAT_TEST_MSSQL='sqlserver://sa:CleatTest123!@localhost:1434?database=cleat&encrypt=disable'
```

The PostgreSQL container on 5433 is provisioned `cleat:cleat`, **not** `postgres:postgres` —
round 2's brief has the wrong credentials for it. `encrypt=disable` is required for
azure-sql-edge: its self-signed certificate has a negative serial number and current Go
rejects it, surfacing as an opaque TLS handshake failure rather than an auth error.

`go test ./engine/` takes ~21 s with PostgreSQL alone and ~50 s with all three. **Check that
delta** — an unset DSN skips its dialect silently and the suite still prints `ok`.

**Use `-p 1` when running more than one database-backed package in a single invocation.**
`go test ./engine/ ./cmd/... ./tests/crash/` runs those packages concurrently against one
database while `CleanupPostgresTestData` deletes from eleven tables unqualified, so a different
unrelated test fails every few runs. That is §2.60d, reproduced locally at last — it was the
mechanism diagnosed from code alone for #297's CI failure. CI does not hit it: each of those is
a separate job with its own database. Six clean `develop` runs and one failure on a branch is
not evidence about the branch, it is evidence about the invocation.

Recreating databases: PostgreSQL `DROP DATABASE cleat WITH (FORCE)`; MySQL
`docker exec cleat-ws2-mysql mysql -u root -pcleat -e "DROP DATABASE cleat; CREATE DATABASE cleat;"`.
**azure-sql-edge ships no `sqlcmd`**, so SQL Server has to be recreated over a Go connection
or by restarting the container. Do not run `docker compose down -v` — it destroys the user's
database. Remove containers by name.

**And once you recreate it, SQL Server does not come back — this sandbox cannot verify the
MSSQL dialect at all.** Found 2026-09-02, and it was invisible until the database was recreated:
the old one had never been migrated past `001`, so it failed early and looked like ordinary
staleness. On a *fresh* database the migrations get as far as
`011_json_scalar_payloads.sql` and stop:

    migration 011_json_scalar_payloads.sql: execute: mssql: The isjson function requires 1 argument(s).

That migration needs `ISJSON(payload, VALUE)`, the two-argument form introduced in SQL Server
2022. **azure-sql-edge is SQL Server 15.0 and does not have it.** Measured across all three
sandboxes' ports rather than assumed, with `SELECT @@VERSION` and a
`SELECT ISJSON('"x"', VALUE)` probe:

| port | server | two-arg `ISJSON` |
|---|---|---|
| `1433` (WS-1) | SQL Server 2022 (RTM-CU26) 16.0 | supported |
| `1434` (WS-2) | **Azure SQL Edge 15.0** | **not supported** |
| `1435` (WS-3) | SQL Server 2022 (RTM-CU26) 16.0 | supported |

So **WS-2 is the only stream that cannot run the repo's own migrations**, and the fix is to
replace the container with `mcr.microsoft.com/mssql/server:2022-latest`. That needs a Rosetta
profile — `~/.colima/default/colima.yaml` has `rosetta: false`, and `sqlservr` aborts under
QEMU without it. **Do not fix this by restarting the `default` profile**: it hosts every
stream's PostgreSQL and MySQL, so restarting it stops their databases too. Create a
`cleat-ws2` profile the way `PARALLEL-WORKSTREAMS.md` describes, and note that `colima start`
rewrites the global docker context, so do it when no other stream is mid-run.

Until then, say so in the PR rather than letting a two-dialect run read as three. CI covers
MSSQL.

**A related trap the ports table cannot show: a container can be in `docker ps` on a port you
cannot reach.** There are two containers named `cleat-ws1-mssql` — one in the `colima-cleat-ws1`
profile on `1433`, and an azure-sql-edge one in the default `colima` profile publishing `1435`,
which is also WS-3's port. `1435` answers as SQL Server 2022, so it is WS-3's VM that owns the
binding and the default-profile container is shadowed. Probe the port, do not read the table:

    for c in $(docker context ls -q); do docker --context $c ps --format "$c {{.Names}} {{.Ports}}"; done

---

## Standing constraints

- Build and test with **CGO on**. `CGO_ENABLED=0` removes `NewWasmtimeBackend` (`//go:build
  cgo`) and ~~silently runs everything on wazero~~ — **corrected 2026-09-02** — leaves no WASM
  backend at all, so `cleat-worker` exits 1 at startup. `CGO_ENABLED=0 go build ./...` still
  exits 0, so nothing warns you at build time. A result obtained that way is not evidence
  about the engine.
- WS-2 owns `engine/durablecalls.go`, `engine/flush*.go`, `engine/store_events*.go`,
  `engine/idempotency.go`, `engine/callerrors.go`, `engine/lifecycle.go`, `tests/crash/`,
  `migrations/*/02[0-9]_*`. `engine/testutil/` is **WS-1's this round** — ask before adding
  test-schema columns, which phase D will need for `intent_at`.
- Never hand-edit `scripts/skip-baseline.txt` or `scripts/test-only-baseline.txt`; regenerate
  with `--update`. `scripts/skip-budget.txt` *is* hand-maintained: set the number the runner
  reports, not a comfortable ceiling. `test-go/engine` and `cluster` move together.
- Prove every regression test can fail before shipping it — and read *why* it failed, not just
  that it did. Two of nine falsifications this round were passing for the wrong reason.
- Never merge on red. Never re-run a failing job hoping it passes — read the log first.
- Branch prefixes are exactly `feature/`, `bugfix/`, `fix/`, `docs/`, `release/`, `hotfix/`.
  Not `feat/`, not `test/` — `Validate branch name` fails the whole PR, and a PR's head branch
  cannot be renamed, so each mistake costs a close-and-reopen. I paid it twice.
