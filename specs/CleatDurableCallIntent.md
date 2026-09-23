<!-- tla-index: issue=1999 -->
# CleatDurableCallIntent.tla — status

Models `engine/callintent.go`'s write-ahead call intents for one workflow's one call step:
`historyRow \in {"absent","pending","complete"}` is the only durable, replay-visible state;
a service's own apply count is separate ground truth. Checks S1 (no unwitnessed redispatch),
S2 (a deduping service applies at most once), S3 (a recorded resolution is never silently
overwritten) and L1 (a pending ambiguity eventually reaches a proper resolution), plus
cleat#1984's not-yet-built declared-lookup resolver design and its own stated open question
about whether a "never sent" answer can safely permit a retry under today's `ResolveCall`
interface.

`CleatDurableCallIntent.cfg`: `Dedupe = FALSE`, `NotSentIsSafeRetry = FALSE` (today's actual
service/resolver contract — the harder configuration for S1, and the honest one for L1).
`serviceApplyCount` is self-clamping at `ApplyCeiling == 3` (no `.cfg` `CONSTRAINT` — the
same cleat#2034 lesson `CleatRunLifecycle.tla`'s `ClockCeiling` already paid for: an
unbounded counter here really is unbounded, since `Dispatch` → `Crash` → `RetentionSweep` →
`AdminReReplayGuardAllows` is a real cycle back to `historyRow = "absent"`, and the unclamped
model did not terminate in 120s). Measured 2026-09-23: 62 distinct states, 184 generated,
search depth 16, under 1s. `TypeOK` and `S3_ResolutionIsDurable` hold at this bound —
re-derive with `make tla`, not by trusting this paragraph. `S3` was verified capable of
failing (the mandatory known-positive, CLAUDE.md's own rule): a deliberately reintroduced
`BuggyOverwrite` action (`historyRow`: `"complete"` → `"pending"` with nothing else changed)
was caught at depth 3 on a scratch copy, then discarded — not present in the shipped file.

**`S1_NoUnwitnessedRedispatch` and `L1_PendingEventuallySettles` are defined but
deliberately NOT gated** in `CleatDurableCallIntent.cfg` — unlike `CleatRunLifecycle.tla`'s
L1-L3, this is not an unconverged spec artifact. Both are **expected to fail against the
current code, and were confirmed to fail with concrete traces**, because they are the
mechanized form of a real gap:

  * **S1** fails at depth 6: `Dispatch` (send #1) → `Crash` → `RetentionSweep` (deletes the
    still-pending row: `--retention-days` defaults to 30 and is on by default) →
    `AdminReReplayGuardAllows` (the guard in `engine/admin_ops.go`'s `ReReplay` reads
    `historyRow = "absent"` — indistinguishable from "never attempted" — and permits resume)
    → `Dispatch` again (send #2, with no `[AMBIGUOUS]` report or resolution ever having
    occurred in between). `s1Violated` latches on this second, unwitnessed send.
  * **L1** fails via two independent mechanisms, checked separately: (a) the same
    `RetentionSweep` race — it can fire while `wfStatus = "failed"` and `historyRow =
    "pending"` (reached via `Crash` alone, before any resolver acts), which disables
    `OperatorResolves`'s fairness obligation by making its guard false rather than by ever
    satisfying it; confirmed to still fail under `NotSentIsSafeRetry = TRUE`, showing this
    cause is independent of the second. (b) Under `NotSentIsSafeRetry = FALSE` specifically,
    `ResolverAnswersNotSent` is a real, always-available no-op — it satisfies `WF`'s
    fairness obligation on every step while changing nothing, so `historyRow` can stay
    `"pending"` forever. This directly answers cleat#1984's own open question: **no, a 404
    ("never sent") answer cannot currently settle an ambiguity**, because `ResolveCall`'s
    interface has no way to express "definitely not sent, safe to retry" — only
    resolved+response or resolved=false.

`S2_DedupeAtMostOnce` is structural under `Dedupe = TRUE` by construction of `Dispatch`
(a deduping service's apply count cannot exceed 1, so `S1`'s violation guard — which only
latches on a second *applied* send — can never fire either); verified non-vacuous with a
separate, non-shipped local run (`Dedupe = TRUE`, `NotSentIsSafeRetry = FALSE`: 14 distinct
states, `TypeOK`/`S1`/`S2` all hold) rather than a second `.cfg`, since `make tla`'s spec
discovery pairs one `.cfg` to one `.tla` by exact basename.

**A documentation defect this model exists to settle, found while grounding it in source
rather than in the issue's own description of the gap.** `engine/db.go`'s
`DeleteExpiredEvents` doc comment and `engine/retention_predicates.go`'s shared-predicate
comment both assert the sweep's event-deletion arm "can never match" for a `'failed'`
workflow, because `finalize_workflow_status` "already deleted those rows" and "the worker
takes that path in production (`FinalizeWorkflowSegment`)". That is contradicted by
`cmd/cleat-worker/setup.go`'s own comment on `FinalizeWorkflowSegment`'s one production call
site — `finalStatus` there is computed as `"done"`, conditionally reassigned to `"ready"`,
and **never** `"failed"` — and empirically by `engine/store_admin_rereplay_test.go`'s
`TestAdminReReplay_ResetsAStoppedWorkflowAndKeepsItsHistory`, which fails a claimed workflow
through `store.FailWorkflow` (the real production path to `'failed'`, confirmed to contain no
`event_history` deletion anywhere in its body) and asserts a preserved call event survives.
So the two comments' "can never match" claim is true of a code path `'failed'` workflows do
not take, and false of the one they do: on a default deployment (`--retention-days` is 30,
on by default), `DeleteExpiredEvents`' first arm **does** delete a failed workflow's
`event_history`, including a still-pending call intent, thirty days after failure. This is
the concrete, default-reachable mechanism `RetentionSweep` models above, not a hypothetical
worst case gated behind a non-default flag.

**Not modeled, and worse than what is: `RetryWorkflow`** (`cmd/cleat-worker/app.go`'s
`st.RetryWorkflow` call, `dead_lettered` → `ready`) **has no equivalent guard at all** —
confirmed by reading its one call site and finding no wrapper analogous to `ReReplay`'s
`isPendingIntent()` check anywhere between the HTTP handler and the store method. A missing
guard and a guard defeated by retention are different defects needing different fixes;
tracked separately rather than folded into this model, per CLAUDE.md's "one PR, one thing".
