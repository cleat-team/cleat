<!-- tla-index: issue=1998 -->
# CleatParentAwait.tla — status

Models parents awaiting children (`AwaitChild`/`AwaitAnyChild`/`AwaitAllChildren`,
`engine/children.go`), the status-to-outcome mapping an await resolves against
(`GetChildResult`/`childOutcomeForSettledStatus`, `engine/store_children.go` and
`engine/status_vocabulary.go`), and the parent-close cascade to every descendant, not
just direct children (`enforceParentClosePolicyAt`/`cascadeIntoClosedChildren`,
`engine/store_lifecycle.go`). Builds on `CleatRunLifecycle.tla`'s (cleat#1997) status
vocabulary and its own, deliberately one-level-only `CascadeTerminate` — that file's
header names this issue as where both the await side-effect and the deeper topology
get picked up.

**Every one of the four bug issues this model exists to check (#1974, #1976, #1978,
#1108) was already CLOSED and its fix already live on the commit this model was read
from (`81fdd8984`, 2026-10-07).** That inverts this directory's usual discovery story:
the model is expected to PASS, not find anything. `CleatParentAwaitPreFix1974.cfg`
(kept outside `specs/`, not run by `make tla` — see its own header for why) is a
deliberate known-positive configuration that flips one CONSTANT
(`PreFix1974Mapping = TRUE`) back to the pre-#1974 `GetChildResult` mapping and
reproduces the violation on demand — see "Known-positive controls" below.

`CleatParentAwait.cfg`: `NumInstances = 4` (fixed: an awaiting/cascading root,
its two direct TERMINATE children — also the await targets — and one grandchild
under the first child), `ClockCeiling = 6`, `AwaitTimeout = 2`,
`PreFix1974Mapping = FALSE`. Measured 2026-10-07: 82,021 distinct states, 521,053
generated, search depth 10, ~13s. `TypeOK`, `Safety` (= `TypeOK` here — see the
`.tla`'s own comment on why `ResumeOutcomeNeverChangesOnceSet` cannot be folded into
it), `ResumeOutcomeNeverChangesOnceSet`, `L1_AwaitEventuallyResumes`,
`L2_AwaitModeResolvesByItsOwnRule`, `L3_CascadeReachesEveryDescendant`, and
`S1_ObservedOutcomeMatchesFinalStatus` all hold at this bound — re-derive with
`make tla`, not by trusting this paragraph.

## Why SettleChild is fair for exactly one instance

The single most consequential modeling decision in this file, found by TLC rather
than decided up front. `SettleChild` (one atomic write standing in for whichever real
action settles a run — see the `.tla`'s own SCOPE DECISIONS) was first given blanket
fairness, `\A i \in Instances : WF_vars(...)`, matching the intuition "every run
eventually settles, full stop". Two measurements rejected that, in opposite
directions:

| fairness given to `SettleChild` | result |
|---|---|
| none at all | `L1_AwaitEventuallyResumes` **falsely violated** — instance 2 simply never settles, forever (34,693 distinct states, counter-example depth 5) |
| every instance, including TERMINATE-policy descendants | `L3_CascadeReachesEveryDescendant` **falsely PASSES** even with `CascadeTerminate` restricted to the root only (the exact pre-#1108 shape) — every descendant eventually settles on its own regardless, so the property never gets a chance to observe the missing recursion |

The fix: `SettleChild` is fair for instance 1 (the root, `ClosePolicy = "NONE"`) only.
Instances 2, 3 and 4 all carry `ClosePolicy = "TERMINATE"`; `SettleChild` stays
*enabled* for them (a child can still finish on its own in any one behaviour TLC
explores) but is not *forced*, the same treatment `CleatRunLifecycle.tla` gives
Crash/Restart. This is not merely a state-space convenience — it is the honest claim
the model makes: for a TERMINATE-policy child, the parent-close cascade can be the
*only* thing standing between an await and waiting forever, and a model that assumes
every child progresses on its own cannot tell whether the cascade does any work at
all. L1 and L3 therefore share one fairness clause rather than being checked in
isolation: instance 1 settles (fair) → `CascadeTerminate(1)` (fair) terminates 2 and
3 → the await (targets `{2,3}`) resolves, and `CascadeTerminate(2)` (fair, now
enabled since 2 is settled) reaches grandchild 4.

## Known-positive controls

Six, covering every gated invariant and property at least once. Each was produced by
a one-line mutation of the clean spec, confirmed to fail for the stated reason, then
restored and the restoration verified byte-identical against the pristine file
(`diff`, not re-reading the mutation's own output) before the next mutation — the
same discipline CLAUDE.md's "Ground rules for changes" asks of every falsification in
this repo.

| mutation | property caught | distinct states | depth |
|---|---|---|---|
| `PreFix1974Mapping = TRUE` (separate `.cfg`, see above) | `L1_AwaitEventuallyResumes` | 75,201 | 7 (stutter) |
| blanket `SettleChild` fairness (see "Why SettleChild is fair" above) | `L1_AwaitEventuallyResumes` | 34,693 | 5 (stutter) |
| `AwaitResolved`'s `"all"` branch replaced with `\E` (behaves like `"any"`) | `L2_AwaitModeResolvesByItsOwnRule` | 697 | 4 |
| `CascadeTerminate(p)` restricted to `p = 1` (pre-#1108, one level only), with per-instance `SettleChild` fairness restored to see the false-pass above | `L3_CascadeReachesEveryDescendant` falsely PASSES | — | — |
| the same restriction, with `SettleChild` fair for the root only (the shipped spec) | `L3_CascadeReachesEveryDescendant` correctly VIOLATED | 36,991 | 5 (stutter) |
| `Repoll` records `ObservedOutcome(status[4])` (an unrelated, never-awaited instance) regardless of which target resolved | `S1_ObservedOutcomeMatchesFinalStatus` | 667 | 4 |
| `StartAwait`'s `~resumed[1]` guard removed (a second await can fire after the first resolves) | `ResumeOutcomeNeverChangesOnceSet` | 54,320 | 7 |

The fourth and fifth rows are the same mutation under two different `Fairness`
clauses, kept as two rows deliberately: the false pass is not a footnote, it is the
reason `SettleChild`'s fairness is scoped the way it is, and a reader re-deriving this
table should hit the false pass first, the same order it was found in.

## #1974 reproduction, attached to the issue

Run by hand against `specs/CleatParentAwait.tla` with the known-positive `.cfg`
described above:

    java -cp tla2tools.jar tlc2.TLC -config CleatParentAwaitPreFix1974.cfg specs/CleatParentAwait.tla

Trace (75,201 distinct states, depth 7): instance 1 starts an `AwaitAllChildren`-style
await (`awaitMode = "all"`) over `{2, 3}`; instance 2 settles `"done"`; instance 3
settles `"terminated"`. Under the pre-#1974 mapping, `ObservedOutcome("terminated")`
is `NoOutcome` — indistinguishable from "still running" — so `AwaitResolved` can never
see all of `{2, 3}` as resolved, and `Repoll` reschedules forever (`Stuttering`,
matching the issue's own words: "The parent re-polls on its await timer for the life
of the deployment"). The full trace is attached as a comment on cleat#1974.

## Adding a fifth bug issue to the acceptance bar

cleat#1998's own acceptance bar names only #1974 and #1978 for "passes with X
modelled" — #1976 and #1108 are modelled too (the parent-wake-on-every-terminal-path
shape is why `SettleChild`'s single atomic write is a faithful stand-in regardless of
*which* real action reached it, and `L3_CascadeReachesEveryDescendant` is #1108's own
property), but neither gets an independent known-positive `.cfg` the way #1974 does:
all three of #1976/#1978/#1108's fixes are structural (a shared status value, a
shared post-settle helper, a recursive cascade call) rather than a single flippable
boolean, and the known-positive table above already exercises the one piece of each
that this model can represent (`L3`'s cascade-restriction row stands in for #1108;
#1978's `error_op = 'parent_close'`/cleared `error_code` are data fields this model
does not carry at all, per the `.tla`'s own SCOPE DECISIONS, so there is nothing
there to falsify — its only observable-to-this-model effect, that the child ends up
`'terminated'` rather than `'failed'`, is simply how `CascadeTerminate` writes today
and always has been in this file).

## Go test mapping (specs/README.md's "Invariant-to-test mapping" policy)

No counterexample was found on any GATED property in this file, so per that policy
there is nothing to promote to a Go regression test from *this* PR. Naming the
existing coverage instead, since every modelled fix already shipped with one. Every
name below was grepped (`grep -n '^func Test' <file>'`), not recalled -- the first
draft of this table named four plausible-looking test functions, none of which
exist; see the paragraph after the table.

| property | nearest Go test |
|---|---|
| L1 (`#1974`'s fix) | `TestAParentIsToldWhenItsChildIsTerminatedOrCancelled`, `TestAwaitAllChildrenReportsTerminatedAndCancelledChildren` (`engine/a_terminated_or_cancelled_child_answers_its_parent_test.go`) |
| L3 (`#1108`'s fix) | `TestTerminateCascadeReachesEveryDescendant` (`engine/terminate_cascade_depth_test.go`) |
| S1 (`#1974`'s fix) | `TestChildOutcomeForSettledStatusNeverReturnsAnEmptyError` (`engine/child_outcome_error_is_never_empty_test.go`); the grandparent-visible case specifically: `TestAGrandparentIsToldWhenAPolicyClosedChildIsTerminated` (`engine/parent_close_terminate_reaches_grandparent_test.go`) |
| the shared post-settle helper (`#1976`) | `TestRecordTerminalFailureWithHistory_NotifiesTerminal`, `TestRecordTerminalFailureWithHistory_DeadLetteredNotifiesDistinctly`, `TestReleaseOrFail_NotifiesTerminal` (`cmd/cleat-worker/terminal_failure_test.go`) |

(Names as of `81fdd8984`; re-grep before citing, per this repo's own rule on numbers
and names that rot. **This table's own first draft is a worked example of why**: it
named `TestGetChildResult_TerminatedAndCancelledChildrenSettle`,
`TestTerminateCascadeReachesGrandchildren`, `TestChildOutcomeForSettledStatus` and
`TestNotifyTerminal_WakesParentAndNotifiesObserversForEveryTerminalStatus` --
plausible-sounding, conventionally-named, and when actually grepped for, absent --
`grep -rn "func Test" <each file>` returned zero matches for all four before this
paragraph was corrected against the real file list.)
