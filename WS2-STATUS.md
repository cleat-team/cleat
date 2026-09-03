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
> | item | evidence it is still open | **status 2026-09-03** |
> |---|---|---|
> | §1.4 phase F | `engine/store_admin.go` implements force-complete/fail; nothing resolves a *pending* step. | ✅ **DONE** — `engine.ResolveStep` (`engine/admin_intent.go:51`). A defect in it was fixed separately in #602 (§3.89). |
> | §2.35 residual | `event_history` has `error TEXT` and no `error_code` column — `awk '/CREATE TABLE.*event_history/,/^\);/' migrations/postgres/001_schema.sql`. | ✅ **Engine half done** (#572), and **the evidence column is why this row read as open for longer than it was**: the command is still accurate — there is no `error_code` COLUMN — but the class ships in the `payload` JSONB (`engine/store_events.go:170`), so the check confirms a prediction about the mechanism rather than the fact. Plugin half open. |
> | `AdminReReplay` | still in `engine/store_admin_stubs.go`. | ✅ **DONE** — #591 deleted that file; real bodies in `engine/store_admin_rereplay.go`. |
> | `pendingSentinel` | still in `engine/types.go:399`, exported as `PendingSentinel` for `tests/integrity`. | ✅ **DONE** — #589. Nothing ever wrote it, so `tests/integrity` never depended on it. |
>
> **A re-derivation is only as good as what it re-derives against, and this table is the
> example.** Every one of these four checks was run and every one was accurate on the day. Three
> went stale within a month because they were checks against *this stream's own upcoming work* —
> the rows closed because WS-2 closed them. The §2.35 row is the interesting failure: its command
> still passes today and still returns the answer "open", because it looks for a column and the
> fix landed as a JSONB key. **A check that names a mechanism will confirm the bug forever if the
> fix arrives by another mechanism** — the same trap CLAUDE.md records for §1.1's `Files:` bullet
> pointing at `003_procedures.sql` when the fix shipped as `004`. Prefer a check on the OBSERVABLE
> (does a recorded failure replay with its class?) over one on the implementation.
>
> **And there is a new first item that is not on the list below: the durable-record shape for
> §3.35 phase 5.** WS-3 has phases 1–4 of `defer` shipped and is deliberately not starting
> phase 5, because its resumable record is the same shape as §1.4's and building a second
> answer is what the one-stream rule exists to prevent. That makes it WS-2's to answer, it is
> a written design rather than a PR, and it should come **before** §2.35 — both touch the same
> rows, and doing them in the other order means designing the record twice.
>
> **Answered 2026-09-02 — §3.75, #569. Do not take this; read the answer.** It reframes the
> question rather than choosing between the two options: both assumed defers run *after* the
> workflow is terminal, and after phases 2–4 they no longer do, so the paths that matter need
> no new record at all. The execution half is built and measured too (§3.81, WS-3's). **The
> sequencing advice above was followed and turned out not to matter** — §2.35's engine half
> landed without touching the defer record, because the record was never needed.
>
> Of the "Not mine, and blocked on WS-3" pair below, **§3.23 landed** (fixed 2026-08-31). **A
> guest-visible `CallErrorAmbiguous` did not** — `grep -rn CallErrorAmbiguous .` is empty and
> the enum still ends at `CallErrorPermissionDenied`. What did change is that adding a member
> is now a bounded job rather than a hunt through five SDKs: `engine.GuestCallErrorCodes()` is
> the single source and `cleat/callerror_contract_test.go` fails if an SDK's hand-written copy
> drifts from it. Still worth doing, still not what stands between an ambiguous crash and an
> operator being told about it.

> ## Re-derived 2026-09-03 against `develop` at `470bf17`
>
> **The numbered list below is now empty.** Its 2026-09-02 note ends "Nothing in this numbered
> list is open except §2.35's plugin half"; that closed today in #633, so the note is stale in
> exactly the direction CLAUDE.md warns about — a status line that was true when written and
> now reads as open work that is not there.
>
> **What WS-2 landed on 2026-09-03**, each one a durability defect found by looking at the
> carrier rather than at the status marker:
>
> | § | PR | what was actually wrong |
> |---|---|---|
> | 3.96 | #617 | `StreamFinish` was persisted by neither the payload nor a column, so a recorded plugin stream **error** replayed as a **success** whose chunk content was the error text |
> | 3.98 | #632 | the database payload carrier had no completeness check at all, unlike the other two carriers. Four fields dropped — `NewVersion` (a versioned continue-as-new replayed from the database restarts as *different code*), `StateKeys` (`ListState` replays empty **and reports success**), and both parent fields |
> | 2.35 plugin half | #633 | **three of four** streaming plugin failures were misclassified, not the one the section claimed. The same deployment gap was retryable through `PluginCall` and permanently fatal through `PluginCallStreaming` |
> | 3.97 → 3.102 | #635 | nine read paths, four different answers about the same row. On **PostgreSQL, the default dialect**, `EXTRACT(EPOCH FROM created_at)::BIGINT * 1000` truncated `TimestampMs` — the replay virtual clock — so a workflow resuming after a crash saw a `Now()` up to **999 ms earlier than the run that recorded it** |
> | — | #629 | the engine suite outgrew the cluster job's 300s timeout: 241s → 291s over two and a half hours of merges, then 300.034s and killed, with **no named failing test** in the log |
>
> **The through-line worth keeping.** Three of those four were found the same way: by reading a
> comment in `engine/compaction.go` that explains why *compaction* carries a field, and then
> checking whether the *database payload* carried it too. Compaction had a completeness property
> test (`FuzzCompactionEquivalence`) and the payload had none, so every payload defect was
> invisible until someone noticed the behaviour it broke. **When one carrier has a mechanism and
> its sibling does not, the sibling is where the bugs are.**
>
> ### Open, as of 2026-09-03
>
> Three items, and **all three have a decision at the front rather than an implementation** —
> which is why none of them was taken today.
>
> 1. **§3.75 step 2 — the two-phase terminal transition.** The flagship. Step 1 (#623) landed the
>    migration and *no Go code*: `grep -rn "pending_terminal_status|defer_phase_deadline"
>    --include=*.go` returns nothing, and so does `grep -rn '"terminating"'`. §3.81 built the
>    execution mechanism, `WithDeferPhase`, and `grep -rn WithDeferPhase --include=*.go` finds
>    **only its own tests** — no production caller. Step 2 is the whole connection.
>
>    Two constraints measured 2026-09-03 that the design did not anticipate:
>
>    - **It is language-gated.** `deferSegmentLanguages = {"go": true}` (`engine/engine.go:486`)
>      against `WasmtimeLanguages = [go, assemblyscript, java, rust, python]` (`:464`), and
>      `engine/executor.go:213` fails the execution **closed** for anything else. So the defer
>      phase can only run for Go guests, and terminate would mean something different depending
>      on the workflow's language. That is a `tiers.yaml` change and a product call, not a
>      detail — D6 decided terminate is asynchronous, not that it is asynchronous *for Go*.
>    - **The worker needs a second engine.** `e.deferPhase` is read per-execution inside
>      `Execute` but set as an `EngineOption` at construction, and the worker builds one shared
>      engine (`cmd/cleat-worker/setup.go:1705`). A second instance sharing the registered
>      backends is the clean answer; worth deciding before implementing rather than during.
>
>    Suggested split, by transition rather than by layer — splitting by layer means landing a
>    mechanism with no caller, which is what §2.35 refused to do and what §1.4 cost the project
>    350 lines of never-run code: (1) `TerminateWorkflow` on all three dialects, carrying the
>    status, claim, segment, finalize and reaper the others reuse; (2) the parent-close
>    `TERMINATE` arm; (3) `adminForceResolve`.
>
> 2. **§2.35's `ServiceCaller` half.** The plugin half is done. What remains is the seven
>    `ErrorCode` values collapsing into one `Retryable()` bit. It changes retry behaviour for
>    workflows in flight, so it wants a decision on direction first. Related and also undecided:
>    whether the four stream-failure causes should get *truthful* codes (`NotFound`,
>    `PermissionDenied`) — recorded in §2.35, and it has to be answered for **both** call paths
>    at once or it re-opens the asymmetry #633 just closed, pointing the other way.
>
> 3. **`DBEventStream` (`engine/event_stream.go`) — a decision, not a task.** A fourth
>    `EventRecord` reader. It selects no `payload` column, so every payload-carried field comes
>    back empty, and its `WHERE` clause is `workflow_id = $1` with **no `tenant_id` predicate**.
>    `grep -rn NewDBEventStream --include=*.go .` finds only its own tests. The trap: its
>    constructor takes a bare `*sql.DB` with no tenant context, so **the missing predicate cannot
>    be fixed without changing an exported signature**. Fix, delete, or leave — all three are
>    public-API calls. Recorded in §3.102. The cross-tenant `WHERE` is the same class WS-1 closed
>    in §3.86, §3.91, §3.92.
>
> ### One process measurement, because it cost more than any single fix
>
> Two PRs were renumbered **seven times between them** on unchanged code — #617 five times
> (§3.90 → 3.91 → 3.93 → 3.94 → 3.95 → **3.96**) and #635 twice (§3.99 → 3.101 → **3.102**) —
> and `scripts/skip-budget.txt` was resolved additively **ten times on one line in two days**.
> Four of the renumbers arrived through rebases that did **not** conflict: the sections were far
> enough apart that git merged both cleanly, so there was no marker to resolve and
> `scripts/check-section-numbers.sh` was the only thing that noticed. **Run it after every
> rebase, not only after a conflicted one** — a clean rebase is evidence the text survived, not
> the numbering.
>
> One of the five was caused by the PR *fixing* an earlier collision: #628 renumbered a duplicate
> onto 3.95, which is where #617 had just moved to escape #626 taking 3.94. With a single global
> counter and no serialisation, even the repair collides. Both fixes are repo-wide rather than
> any one stream's, and both are recorded in §3.96's note: **"Require branches to be up to date
> before merging"** on `develop`, at the cost of one extra CI cycle per merge, or stop numbering
> plan sections in one global sequence.

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
>
> **2026-09-03: items 2, 5 and 6 are now done as well, and item 3 is half done.** #578 and #602
> (2, phase F and the checksum defect in it), #591 (5, `AdminReReplay`), #589 (6, the sentinel),
> #572 (3's engine half). The list stays because the reasoning under each item is still the
> reasoning — but read it as history, and take the board in `PARALLEL-WORKSTREAMS.md` as the
> live one.
>
> **Later on 2026-09-03: §2.35's plugin half closed too (#633), so this numbered list is now
> entirely history.** An earlier version of this paragraph ended "Nothing in this numbered list
> is open except §2.35's plugin half", which was true for about six hours. The current open
> items are in the `Re-derived 2026-09-03` banner above, and none of them is on this list.

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

**Those two absolutes are stale, and a session that trusts them will misread its own run.**
Measured 2026-09-02 on `develop` at `603fdd9`, `go test ./engine/... -count=1 -p 1` with
PostgreSQL and MySQL set and no MSSQL DSN: **149 s** for the engine package, 2 m 37 s wall.
That is three times the "all three" figure with one dialect fewer. It was taken while another
sandbox was running its own suite (load average 8.6, `221% cpu` for this one), so it is an
upper bound rather than a replacement number — which is the point: **the delta only works as a
same-session control.** Run one arm, run the other, compare the two you just took. Do not
compare today's run against a figure someone wrote down in August, and do not take a timing
number at all while `ps aux | grep engine.test` shows another stream's run.

What survives is the shape of the check, not the constants: fewer dialects is faster, and a
run that finishes suspiciously fast tested less than you think.

**Use `-p 1` when running more than one database-backed package in a single invocation.**
`go test ./engine/ ./cmd/... ./tests/crash/` runs those packages concurrently against one
database while `CleanupPostgresTestData` deletes from eleven tables unqualified, so a different
unrelated test fails every few runs. That is §2.60d, reproduced locally at last — it was the
mechanism diagnosed from code alone for #297's CI failure. CI does not hit it: each of those is
a separate job with its own database. Six clean `develop` runs and one failure on a branch is
not evidence about the branch, it is evidence about the invocation.

Recreating databases: PostgreSQL `DROP DATABASE cleat WITH (FORCE)`; MySQL
`docker exec cleat-ws2-mysql mysql -u root -pcleat -e "DROP DATABASE cleat; CREATE DATABASE cleat;"`.

**Done on 2026-09-02 for #594** (`workflow_defs` keyed by `(tenant_id, name, version)`, with
migrations on all three dialects), following the rule recorded above about recreating after a
schema change lands on `develop`. Both databases came back clean and `develop` at `603fdd9` is
green here on PostgreSQL and MySQL — which also says those migrations apply to a *fresh*
database on both, not only as an ALTER over an existing one. MSSQL skipped for an absent DSN,
the one legitimate reason; this sandbox still cannot run that dialect at all (see below).

Verified the skip was the only one that mattered rather than assuming it:
`go test ./engine/ -run 'TestTerminateWorkflowEnforcesParentClosePolicy|TestDeployRecordsTheDeployingTenant' -v`
shows `PASS/postgres`, `PASS/mysql`, `SKIP/mssql`. A green suite is not evidence that a
dialect ran; a named subtest is.

**Recreating `cleat` is not enough, and the leftovers fail in a way that reads as a code
defect.** `cmd/cleat-worker`'s tenant-isolation tests create *per-tenant* databases named
`cleat_<tenant-uuid-with-underscores>` and drop them in `t.Cleanup` — which does not run when a
test run is killed. MySQL has no `CREATE INDEX IF NOT EXISTS`, so re-applying `001_schema.sql`
to a surviving one always fails:

    tenant_isolation_mysql_test.go:76: apply mysql migrations: migration 001_schema.sql:
    execute: Error 1061 (42000): Duplicate key name 'idx_instances_ready'

Nothing in that says "stale database". Found 2026-09-02 after killing a run mid-flight, and
confirmed environmental by reproducing it on `develop` with the branch stashed — worth doing
before reading a failure like this as yours. List and drop them:

    docker --context colima exec cleat-ws2-mysql mysql -uroot -pcleat -N -e "SHOW DATABASES LIKE 'cleat%'"

That line only lists them; this drops every one and puts `cleat` back:

    for db in $(docker --context colima exec cleat-ws2-mysql mysql -uroot -pcleat -N \
                  -e "SHOW DATABASES LIKE 'cleat%'"); do
      docker --context colima exec cleat-ws2-mysql mysql -uroot -pcleat -e "DROP DATABASE \`$db\`;"
    done
    docker --context colima exec cleat-ws2-mysql mysql -uroot -pcleat -e "CREATE DATABASE cleat;"

`--context colima` rather than a bare `docker exec` for the same reason the line above uses it:
another stream's `colima start` rewrites the global context, and a bare `docker exec` then
resolves the container name in whichever profile it left behind. Both forms were run on
2026-09-02 and both worked, because the default context happened to be `colima` — which is
exactly the condition that makes the bare form look safe.

**The tenant UUIDs are fixed, not random**, so the leftover set is small and recognisable:
`00000000…`, `11111111…`, `22222222…`, `33333333…`. All four were present again on
2026-09-02 after the previous session's killed run — the trap recurs on every interrupted run
rather than having been a one-off.

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
