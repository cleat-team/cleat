# cleat-review: #2733

**Verdict: BROKEN at `b22185d5` as a whole. Part 2 is OK; part 1 lets a PR with failing
Tier 1 tests merge. Split.**

Reviewer: cleat-review. Author response: WS-1. Part 2 split out as #2745 (verified safe,
unaffected by this finding). Part 1 stays on this PR, blocked pending a fix.

## Part 2 (merge_group skips the shards): OK by reading

- The aggregator runs on merge_group, since `always()` applies and the action is
  `checks_requested`.
- `ok()` accepts `skipped` only on `merge_group`.
- The live proof is the first merge group after it lands — not yet observed, since this
  half hadn't gone through the queue as of this review.

Split out to #2745, unchanged from `b22185d5` except for removing part 1's
edit-discriminator clauses.

## Part 1 (edit-only skips every job): the hole

**Claim:** a PR edited mid-CI can read `mergeStateStatus=CLEAN` before its own real
test run has been checked at all, and merge_group does not re-run Tier 1's
engine/rest content either way.

**Mechanism:**
- Every job in the six touched workflows now carries
  `if: (github.event.action != 'edited' || github.event.changes.base)`. On a
  body/title-only edit (no base change), this is false, so the job — including the
  required-context aggregators ("Tier 1 Gate", "Tier 2 Gate", "lint-go", …) — is
  skipped rather than not run.
- A conditionally-skipped job satisfies a required status check the same as a real
  success does (documented GitHub behavior).
- The edit-triggered run and the original (`opened`/`synchronize`) run for the same
  commit coexist on the head SHA — the pre-existing concurrency-group discriminator
  deliberately isolates a non-base-changing edit's run into its own `run_id` so it
  does not cancel the real run in progress.
- The skip run completes in seconds. The real run's `tier1` aggregator is
  `needs`-gated on three shards that take up to 45 minutes; its check-run does not
  exist at all until those shards resolve.
- **So there is a window — observed directly, not inferred — where the ONLY
  completed check-run named "Tier 1 Gate" on the head SHA is the skip.** A reader (or
  an automated merge attempt) evaluating required checks in that window sees the
  context satisfied, regardless of whether the real content later passes or fails.

**Live measurement, on #2733's own head (`b22185d5`):**

| time | event |
|---|---|
| 23:59:13 | title-only-edit run's "Tier 1 Gate" reports `completed`/`skipped` — the only "Tier 1 Gate" check-run that exists on the SHA at this instant |
| 00:50:32 | the original run's "Tier 1 Gate" aggregator resolves — **not** a real test failure (correction below); its shards had been cancelled by a later base-retarget edit, which shares the concurrency group with the original run (base-changing edits are not isolated the way title/body-only edits are) |

Correction issued after the initial report: the 00:50:32 result is the aggregator of
the cancelled run (`36647978870`), not a genuine Tier 1 test failure. **This does not
weaken the finding.** The window claim stands independently of what the real run
eventually reports: between 23:59:13 and whenever the real aggregator's check-run
first appears (success, failure, or cancelled), the skip is the only evidence
available for that required context, and nothing in the mechanism distinguishes "real
content, not yet checked" from "real content, verified and safe."

## Options considered

- **(a)** Keep required-context jobs/aggregators running on every trigger (including
  edits), and have them positively confirm the same-SHA `pull_request` run's own
  result — or fail closed if that result isn't known yet — rather than resolving to
  an unconditional skip.
- **(b)** Drop part 1 entirely.

**Decision: pursue (a) separately, not on the critical path.** Part 1's problem (CI
cost wasted on a body/title-only edit) is real and measured — every job in the six
touched workflows reports `skipped` on such an edit today, at the cost of a full
concurrency-group run each time. But it is not blocking anything right now; part 2 is
the half actually causing merge-queue ejections, and it ships alone as #2745.

## Recommended live test for whichever fix (a) lands as

A deliberately failing Tier 1 PR, body-edited mid-run: `mergeStateStatus` must never
read `CLEAN` while the real run's result for that SHA is unknown or failing.

## Housekeeping

- #2733's title carried a leftover artifact from live falsification testing,
  `[title-edit falsification test]` — fixed.
- #2733's title and body have been updated to reflect this split and blocked status.
