# Three-stream coordination

**This is the only coordination file.** It replaces the 2026-08-06 "one at a time" version, whose
title had stopped describing the project (its own table measured all three streams writing in
September). Rewritten 2026-09-04.

Everything here is operative. Nothing here is history — git has that.

---

## The diagnosis, measured

The streams do not collide on code. They collide on shared bookkeeping.

Commits touching each file, 2026-09-01 → 09-04, `git log --since=... --name-only --format="" origin/develop | sort | uniq -c | sort -rn`:

| file | commits |
|---|---|
| `IMPROVEMENT-PLAN.md` | **138** |
| `scripts/skip-budget.txt` | 37 |
| `tiers.yaml` | 18 |
| `scripts/skip-baseline.txt` | 18 |
| — | |
| `engine/engine.go` (busiest **source** file) | 16 |

**211 commits into four shared bookkeeping files, against 16 into the busiest source file.**
Coordination cost here is not a function of how much code a stream writes. It is a function of how
much *shared mutable state* it writes.

Two more numbers that follow from it:

- `scripts/skip-budget.txt` holds one line of **120,780 characters** — a shared counter whose
  comment field accumulated every conflict's archaeology, because the resolution rule was
  "additive". Three of those conflicts had *both sides reading the same number*.
- 27% of commits in that window are `docs:`, and the week produced at least four stale-marker
  incidents (§3.36 read as open work for four days; §3.83's heading two days; `tiers.yaml`'s
  admin-dashboard entry a month; `WS3-STATUS.md` five days without a touch while WS-3 merged daily;
  both retired 2026-09-04).

Throughput is not the problem: 192 of 200 PRs opened since 09-01 merged, 4 closed unmerged. The
process works. It is expensive.

---

## Rules

Each rule exists because something measured above went wrong. They are short on purpose.

**R1 — Never edit a shared counter.** If two streams must both change a number, the number is
wrong. Derive it. `skip-budget.txt`'s total becomes a sum over per-test declarations that live
next to the tests; nobody edits a total, so there is nothing to conflict on.

**R2 — Section numbers are per-stream blocks.** WS-1 `3.200–299`, WS-2 `3.300–399`, WS-3
`3.400–499`. Sequential integers across three streams made collisions that
`check-section-numbers.sh` structurally cannot see, because they exist only across open PRs.
Disjoint blocks make them impossible instead of detected.

**Do not pick a number by hand: `scripts/next-section-number.sh` prints yours.** It reads
`origin/develop`, not your branch, because the gap between those two is the original defect.

This rule first said `3.1xx`/`3.2xx`/`3.3xx`, and that was unimplementable on the day it was
written — 3.100 through 3.114 already existed, allocated by all three streams before any block
scheme (3.100 #634, 3.107 #647, 3.113 #684, 3.114 #687). Handing `3.1xx` to WS-1 would have
retroactively assigned three streams' work to one. The blocks start above the high-water mark
instead; everything at or below 3.114 is grandfathered. See `scripts/section-blocks.sh`.

**R3 — A closed item leaves the plan, by being ARCHIVED, not deleted.** Its *lesson* graduates to
the code it describes (a comment beside the guard) or to `CLAUDE.md`. The section itself moves to a
companion file that keeps its anchor resolvable. `IMPROVEMENT-PLAN.md` is 15,885 lines
(`wc -l IMPROVEMENT-PLAN.md`, 2026-09-04 17:20Z — it moves hourly) and both stale-marker incidents
this week were findability failures caused by size. Target: the plan holds open items.

**This rule said "deleted" until 2026-09-04, and following it literally would have broken about two
thousand cross-references.** Measured before A3 started, which is the only reason it was caught:

| | count | re-derive |
|---|---|---|
| sections in the plan | 153 | `grep -cE '^### [0-9]+\.[0-9]+ ' IMPROVEMENT-PLAN.md` |
| …carrying a closed marker | **143** | same, piped to `grep -cE '✅\|FIXED\|DONE\|fixed in'` |
| `§N.M` refs inside the plan | 800 | `grep -oE '§[0-9]+\.[0-9]+' IMPROVEMENT-PLAN.md \| wc -l` |
| `§N.M` refs in other markdown | 1083 | same over `--include='*.md'`, excluding the plan |
| `§N.M` refs in code | 191 | same over `*.go *.sh *.yml *.rs *.py *.java *.ts` |
| distinct sections cited **from code** | 56 | those refs, `sort -u` |
| …that are closed | **49** | `comm -12` against the closed list |

So "delete the closed sections" means deleting 93% of the file and breaking 49 live code comments,
each of which cites the plan as the authoritative description of what a test package is for.
Archiving costs nothing extra and keeps every one of them resolvable.

(That markdown row read 1080 on the first pass and 1083 on the second. Three of the references it
counts are in the paragraph below, added between the two runs. R3a is not a style rule.)

**A `§N.M` does not always name a `###` heading.** The Phase 2 table carries rows `2.1`–`2.9` in the
same numeric shape, and three of the sections cited from code — `§2.4`, `§2.5`, `§2.7` — resolve to
*rows*, not headings. Checking only headings reports them as dangling; that check was written and it
did, and the finding was wrong. Anything that migrates or validates references must know both
namespaces. The two join deliberately at `2.8`, where row 2.8 says "see below" and `### 2.8 results`
is the write-up.

**R3a — Every number in this file carries the command that re-derives it**, because they all move.
The plan's line count changed between drafting this file and verifying it, which is the whole
argument for the rule rather than a footnote to it.

**R4 — An open item names the command that closes it.** If you cannot write the command, it is a
note, not an item. This is `tiers.yaml`'s own rule applied to `tiers.yaml`'s own prose: its
`open_items` lists are the part of that file CI does not check, and all three dashboard entries
were false for a month.

**R5 — Status is `gh pr list`.** No per-stream status files. `WS2-STATUS.md`, `WS3-STATUS.md`,
`PARALLEL-WORKSTREAMS.md` and this file were 1,803 lines between them with one five days stale.
Retire them into this one. **Done 2026-09-04 (A4)**: all three are deleted and their durable
reference content is in "Sandboxes, databases, and shared files" at the end of this file. What was
retired is the *status* half — boards, per-stream item lists, merged-PR counts — because that is
the half that goes stale between the writing and the reading.

**R6 — Never hold two open PRs that touch the same declaration file.** Land the first, rebase the
second. WS-1's own #680 and #682 collided on the three SDK parity lists because both were open at
once; the merge order decided which one had to be resolved by hand.

**R6a — A DERIVED file is a shared declaration file too, and it is the more dangerous kind,
because its collision is SILENT.** Added 2026-09-17 after WS-3 obeyed R6 on the hand-edited file
they had been warned about and then held two PRs both regenerating `scripts/skip-baseline.txt`.

    two appends to a shared file        -> CONFLICT, loud, someone resolves it
    two regenerations of a DERIVED file -> CLEAN MERGE, and the older scanner wins

It did not conflict. It **reverted**: the older branch's baseline had been generated from a tree
whose `check-skips.sh` could not yet see methods, so merging restored the exact misattribution the
other PR existed to remove, and dropped a third PR's entries. `git merge-tree` reported clean,
because textually it is. A derived file has no conflict surface, so every instinct built around
*git will stop me* is inverted — and the hunk-move that resolves two guards inserting at one
`ci.yml` anchor does not help, because the collision is the whole file's content rather than a
region of it.

**And it had already happened on `develop`, unnoticed**: the baseline carried
`engine mssqlRowDisappearanceReporter 2`, which the current scanner produces **zero** times, and
the guard exited 0 — because it fails only on skips *above* the baseline. A grant covering
something that is not there is invisible to a ceiling-only check. (cleat#1746.)

**The rule, and the second clause is the one that is easy to miss:**

> **Rebase first, regenerate second, assert the diff.** Never conflict-resolve a derived file —
> regenerate it. *And regenerate it after a CLEAN merge too, if the other side changed the
> generator.*

**The trigger is not "the other side touched the derived file" — it is "the other side touched the
GENERATOR"**, a change nowhere near the file you are about to regenerate. #1742 changed
`check-skips.sh`; #1744 regenerated `skip-baseline.txt`. Nothing in #1744's diff pointed at #1742
and no conflict marker ever appeared, so the habit has to be keyed on the **rebase**, not on
inspecting your own diff for overlap.

**The obvious structural fix does NOT work here, and the reason is the sharper half of this rule.**
`scripts/skip-ledger.d/` removed this class for the budget ledger (cleat#1333, #1395) by giving
each declaration its own file, and reaching for the same shape here is the first thing anyone
tries — WS-3 filed exactly that remedy in cleat#1746 and then withdrew it on measurement.

    hand-authored declarations  ->  one file per entry WORKS.  Two streams
                                    writing different files have nothing to
                                    collide over.
    GENERATED wholesale         ->  one file per entry buys NOTHING. A
                                    regeneration from a stale base rewrites all
                                    N files with the older generator's output,
                                    and they merge exactly as cleanly as one
                                    file did.

So the distinction that matters is **hand-authored versus generated**, not one-file versus many —
and it is the same distinction underneath this whole rule. R1 ("never edit a shared counter,
derive it") applies to the first kind. For the second kind there is no layout that helps: the
remedy is a **guard that fails on a stale entry**, which is what cleat#1751 adds for
`skip-baseline.txt`.

**Both halves survive, and not redundantly.** A guard makes staleness fail for the one file it
covers, so there the discipline is enforced rather than remembered. This rule covers every *other*
derived file in the tree, none of which has a guard — and the next derived file to acquire this
hazard will not have one on the day it acquires it. (WS-3's framing, after correcting their own
filed remedy.)

**R7 — Freeze the surface while converging.** No new SDK capability, no new host call, no new
dialect until the existing matrix is guarded. Every addition multiplies 5 languages × 3 dialects,
and §3.111 measured what that costs: seven remaining calls ≈ 40 guest-side edits.

**R8 — A cross-stream claim carries the command that produced it.** This already works and is the
one thing not to change: WS-1 and WS-2 each caught real errors in the other's work this week, every
time by re-deriving rather than accepting. Keep the evidence attached.

**R9 — A stream waiting on CI is available for the next item; a stream whose PR needs it is not.**
"Has an open PR" was the availability test, and it conflates two states that look identical from
outside: *writing* a change, and *waiting* on a queue that will take most of an hour.

Measured 2026-09-16, every PR merged to `develop` that day:

| | value | note |
|---|---|---|
| merged that day | 27 | 3 are dependabot, **excluded** |
| session-authored | **24** | the population below |
| median open→merged | **42 min** | q1 38, q3 48 |
| range | 34–383 min | |
| total PR-holding | **1460 min** | over 24 hours, in one day |

```
gh pr list --repo cleat-team/cleat --state merged --search "merged:2026-09-16" \
  --limit 100 --json number,createdAt,mergedAt,headRefName \
  | jq -r '.[] | select(.headRefName|startswith("dependabot")|not)
           | [(.mergedAt|fromdate) - (.createdAt|fromdate)] | .[]/60'
```

The dependabot exclusion is not tidying: those three were opened days earlier and their median
open→merged is **10,012 minutes**, which swamps the statistic the rule is about. A first pass
quoted "8 PRs, median 48" from a hand-picked subset, and the command above returns 27 — R3a caught
it in the file that states R3a.

**The streams already work around it, which is the actual finding.** On the same day WS-3 held
`#1682` and `#1683` open together for **36 minutes**, and WS-2 held `#1680` and `#1689` for
**37 minutes** (`createdAt`/`mergedAt` intersected per pair). So the rule was stricter than the
practice it described, and its only effect was to stop *directed* assignment while the streams
self-served. A test nobody follows is not a safeguard.

**The candidacy test.** A stream is available if it is idle and every open PR of its own is
*waiting*: in the merge queue **with an entry state of `QUEUED`, `AWAITING_CHECKS` or
`MERGEABLE`**, or with all required checks running or green, **nothing red, no conflict, and no
cancelled twin**. Red, `CONFLICTING`, draft and twinned all disqualify — those need their author.

**Out of the queue is not waiting, and the PR page will not say so** (added 2026-09-23). A PR
removed from the queue for failed checks has no entry to read, and it keeps reading
`mergeStateStatus: CLEAN` with its own checks green: the failure was on the queue's *batch*
commit, a different SHA. That day there were 8 removals that were not merges — 3 `failed_checks`
(#2016, #2037, #2057) and 5 `manual` — and #2057's (15:12:25Z; a plugins-job failure on batch
`a543752ead`, cleat#2063) was found by the coordinator's 15-minute tick, not by its author's
watcher, which polled `mergeStateStatus`. So the last queue event matters too: a removal whose
reason is not `merged`, with nothing pushed since, disqualifies. `scripts/pr-watch.py` applies the
whole candidacy test, this clause included, and reads a dropped PR's failing jobs from its batch:

```
scripts/pr-watch.py 2057 2061          # WAITING / MERGED / NEEDS-AUTHOR, one line per PR
scripts/pr-watch.py --watch 120 2061   # poll; returns the moment one needs its author
```

Known-positive: #2057's state between 15:12:25Z and its re-enqueue at 15:19:10Z, rebuilt from its
real timeline and batch, reports `DROPPED … batch a543752ead failed: Test Go (plugins) on 1.26
(run 35878274795, job 107239811455). The PR page still reads CLEAN`. Negative controls: the three
PRs open at 15:4xZ (two running checks, one `AWAITING_CHECKS`) read `WAITING`; #2054 and #2056
read `MERGED`.

**The queue entry has its own state, and it is not the PR's.** `MergeQueueEntryState` is
`QUEUED | AWAITING_CHECKS | MERGEABLE | UNMERGEABLE | LOCKED`, and an entry can read `UNMERGEABLE`
while the PR's own `mergeStateStatus` still reads `CLEAN` — WS-2 hit this on 2026-09-16 with two of
their own PRs both appending to `IMPROVEMENT-PLAN.md`: the first to merge made the second
unmergeable *in the queue*, and nothing at PR level said so. Reading only `mergeStateStatus` counts
that stream as waiting when it is blocked:

```
gh api graphql -f query='{repository(owner:"cleat-team",name:"cleat"){
  mergeQueue(branch:"develop"){entries(first:20){nodes{state pullRequest{number}}}}}}'
```

This is R6 arriving through a different door — two PRs touching one shared file — and it is why R6
survives R9 rather than being relaxed by it.

**Name the healthy states, not the unhealthy ones.** This clause first said "not `UNMERGEABLE`",
which is the state WS-2 happened to trip over. `LOCKED` exists too and neither of us has met it;
the enum's own descriptions are circular (*"LOCKED: The entry is currently locked"*), so its
semantics cannot be settled from the schema. A blocklist would have the same hole next time in a
different costume, and a new enum value would silently read as healthy. The allowlist fails closed
instead: an unrecognised state means the stream is not offered work, which costs one missed
assignment rather than one wrongly-directed stream.

**Recovering a wedged entry, since the rule now tells streams to look for one.** Measured by WS-2,
not re-run here: `gh pr merge --disable-auto` does **not** dequeue an already-queued PR — it
answers *"already queued to merge"*. The call that works is

```
gh api graphql -f query='mutation($id:ID!){dequeuePullRequest(input:{id:$id}){clientMutationId}}' -F id=<PR_node_id>
```

and `id` is the **PullRequest** node id (`PR_…`), not the entry id (`MQE_…`) — the first error
rejects `pullRequestId` and then demands `id`, which reads as though it wants the entry.

The twin clause is not a detail. A PR can report every required context green and still be
unmergeable, because branch protection is satisfied by neither member of a duplicated run set, and
clearing it needs a **new SHA** rather than a re-run (cleat#1688, three instances in one day):

```
gh api --paginate "repos/cleat-team/cleat/commits/<FULL-40-char-sha>/check-runs?per_page=100" \
  --jq '.check_runs[].conclusion' | sort | uniq -c
```

A non-zero `cancelled` count means the PR needs its author. Without this clause R9 hands work to
precisely the streams least able to take it.

**This clause prescribed `gh run list --commit <head-sha> … | uniq -d` until 2026-09-16, and that
form answers "no twin" three ways** (cleat#1703). An **abbreviated** SHA returns zero runs and no
error, because `head_sha=` is an exact string match rather than a prefix resolve — and the sibling
`…/commits/<sha>/check-runs` endpoint *does* resolve a prefix, so nothing teaches you the
difference. A `?per_page=100` conclusion count silently truncated **121** check runs to 100, which
cost 17 of the cancelled ones on the very SHA the rule cites as its example. And `uniq -d` over
names over-reports: `cla-assistant.yml` subscribes to `pull_request_target: closed`, so a second
`CLA Assistant` run starts 2–3 seconds after `merged_at` — measured on four PRs merged that day,
both members `success` every time. That last one lands at exactly the sample a watcher polling to
`MERGED` takes last, which is the worst possible moment for a false alarm.

Known-positive `431737a0` reports **55 cancelled**; negative control
`cab6353741afd57203d338f06b79b24334baee34`, which merged, reports **0**.

**Cap: two held PRs plus one new item** (raised from one held PR on 2026-09-23, owner-approved).
R6 still governs, and more of it now: no two of the three — both held PRs and the new item — may
touch the same declaration file, derived files included (R6a). If one of them would, the new item
is not eligible regardless of R9. A held PR that turns NEEDS-AUTHOR comes before the new item.

Why two. Measured 2026-09-23 over the 31 non-dependabot PRs merged that day: median open→merged
**65 min** (q1 46, q3 95), of which about **42 min** is CI run twice — once on the PR, once on
the queue's batch (merge_group `CI/CD Pipeline` and `Tier 1 Gate` medians **21 min** each). With
a cap of one, a stream holding two waiting PRs is not offered work however long they wait: WS-3
held #2054 and #2057 together from 14:00:12Z to 15:12:25Z (**72 min**) and sat idle for that span.
This is the same finding R9 started from — the streams already held pairs — one level up.

```
gh pr list --repo cleat-team/cleat --state merged --limit 100 \
  --search "merged:>=2026-09-23T00:00:00Z -author:app/dependabot" --json createdAt,mergedAt \
  --jq 'map(((.mergedAt|fromdateiso8601)-(.createdAt|fromdateiso8601))/60|floor)|sort'
```

The cost is more rebases: with two PRs in the queue at once, the first to merge can leave the
second `UNMERGEABLE` over a file R6 did not catch. The candidacy test catches that one, and that is
why a held PR that needs its author comes first.

**It is an offer, and a decline is final.** Both refusals on the day it was written were correct
and were respected: WS-1 declined `#1410` while mid-task, and declined `#1688` on the grounds that
"parking that to start something else is the pattern we've both agreed is worth avoiding" — then
measured the one open question on it anyway and posted the result. Context-switch cost is real and
the holder is the one who can price it.

**R10 — A red develop is stop-the-line: revert first, investigate second** (owner-approved
2026-09-23 with #2080). From #2080 on, the merge queue no longer re-runs Tier 1 Gate's test
shards on the batch: they report "already checked on the pull request". A semantic conflict
between two PRs that were each green on their own is therefore caught *after* the merge, by
develop's own `push` run or the 4-hourly `engine-race.yml` run (#2082), not before it. That trade
was taken on evidence: in the 57 queue batches measured over 09-21 to 09-23, those jobs failed 0
times, and every failure they caught was on the PR (#2080 has the table). It is acceptable only if
a red develop is fixed at once:

- Whoever sees develop go red says so to the coordinator. The author of the most recent merge,
  or the coordinator if that author is busy, **reverts that merge first** (`gh pr revert`, a
  normal PR through the queue) and investigates afterwards, on the reverted branch.
- No stream enqueues anything else until develop is green again, because everything queued
  behind a red develop is tested against a broken base.
- A red `engine-race.yml` run opens a tracking issue on its own. A real `WARNING: DATA RACE` is
  treated the same way as a red develop.

---

## Verification protocol

Five failures on 2026-09-04 alone, all of the same kind: a check that ran clean and answered a
different question than the one asked.

1. **Assert the mutation applied before running the falsification.** A `perl` substitution that
   silently matched nothing produced a "test held" reading against an unmutated tree.
2. **A falsification that fires two assertions proves neither.** Break the narrowest thing that
   isolates the one you mean to test.
3. **Wait for every check to settle before diagnosing a failure.** Reading the first red while
   others are pending sent an hour into an invisible failure while a self-explaining one sat queued.
4. **A number carries the command, and the command must answer the question.**
   `git log --date=iso` prints local time with an offset; pasting it into a UTC comparison moved a
   window four hours and inflated a finding from 5-of-24 to 11-of-36.
5. **Check the shell, not just the tool.** `cmd | tail && echo ok` reports the tail's exit status;
   a grep for `allowlist` misses `tenantPredicateAllowlist`; zsh eats `$var[...]` as a subscript.

---

## The next 24 hours

Written 2026-09-05. **The previous round is complete** — A1–A3, B1–B3, C1–C3 all closed, fifteen
PRs merged. A2 was the last row and shipped as `scripts/convergence.py`.

This round has one subject, and it is chosen from a measurement rather than from a hunch. The
measurement is §3.210; the short version is two tables.

**The host is exercised. Every one of it.**

    go tool cover -func on ./engine/ with all three dialects:
      56 host handlers reachable from engine/imports.go
      0 with zero coverage        lowest: ListCrons 25.0%, DeleteCron 44.8%
      engine package: 86.7% of statements

**The guest bindings are not.** Host calls a test actually builds *and runs*, per SDK:

| SDK | executed | surface | compiled |
|---|---|---|---|
| go | 11 | 37 | 37 ✅ |
| python | 10 | 73 | 73 (on #741) |
| java | 8 | 68 | 68 ✅ |
| rust | 7 | 61 | 61 ✅ |
| assemblyscript | 7 | 66 | 66 ✅ |

**The gap between those last two columns is this round's subject**: roughly fifty calls per
language that now compile and have never been run against a host in that language.

### Why this and not something else

Every binding-layer defect found on 2026-09-04 and 09-05 was a **guest** defect against a host
that already worked:

| | |
|---|---|
| §3.204 | Go could not compile locks, promises or side effects at all |
| §3.200 | the Go guest decoded the host's error length and discarded the message |
| §3.201 | the Python SDK discarded the host's answer on 13 calls |
| §3.202 | a stop read as a timeout on Python `await_signals` |
| §3.303 | 16 of 17 plugin calls failing in every language, all five tests green |
| #455 | a Java workflow's result was JSON inside a string |

**Six defects, six guests, zero hosts.** That is not a coincidence to note in passing — it says
where the remaining risk is, and the coverage numbers above say the same thing from the other
direction. Compile coverage, which this week took from ~11% to 100% on three SDKs, cannot catch
any of the six: every one of them compiles.

### The shape of the work, and the trap in it

**Do not write 250 tests.** Fifty calls times five languages is a sweep, and CLAUDE.md's rule
applies — a backlog of similar findings is usually one missing abstraction. The abstraction here
already exists in one place: `tests/plugin-harness/wasm_plugin_test.go` runs one fixture per
language, collects a result per call, and compares against a table of expected outcomes. §3.303
extended it to require a *reason* per failure rather than a present key. **That pattern generalises
from 17 plugin calls to N host calls; nothing else in the tree does.**

`tests/cross-language/` is not it — 594 lines, Go and Rust only, one hand-written test per case.

**Two constraints on the Python fixture, both from #742, both design inputs to A1 rather than
things B2 discovers the hard way:**

*A Python component cannot be pinned.* componentize-py's output is not reproducible — five
consecutive builds of unchanged source gave five distinct digests with sizes moving in both
directions (20482296 / 20443810 / 20421353 / 20448164 / 20398088). So "commit the fixture and diff
it" is not available for Python, and neither is #726's remedy of moving the artifact out of the
build's reach and refreshing it. The harness must build Python fixtures into a temp directory and
compare *outcomes*, never bytes.

*And it must pass an absolute `--output`.* `python-sdk/scripts/build_wasm.py` resolves a relative
one against the **entry file's directory** — not the CWD and not `-o` — which is how
`TestPluginCalls_Wasm_Python` came to rewrite two tracked 19 MB fixtures on every run. B2 would have
met this as an unexplained dirty tree with a 20 MB binary diff.

**Python fixtures are verified in the container, not on the host.** `componentize-py` dies on
Darwin with `EXC_GUARD / GUARD_TYPE_MACH_PORT`; `scripts/docker/python-toolchain.Dockerfile` exists
for exactly this and has since 2026-08-06. **Check the mount before trusting a result** — a runtime
that cannot bind-mount this path produces an *empty* directory without saying so, and the failure
reads as a broken checkout:

    docker run --rm -v "$PWD":/src cleat-py-toolchain test -f /src/go.mod
    docker run --rm -v "$PWD":/src -w /src -e CGO_ENABLED=1 \
      cleat-py-toolchain go test ./engine/ -run TestPythonAllHostCallsWorkflowCompiles -count=1

This prescribed `--context desktop-linux` until 2026-09-16, which was correct on a Mac that also
ran colima and **cannot work on this machine**: Docker Desktop is not running, so the flag fails
with `failed to connect to the docker API`, while a plain `docker run` mounts the tree (measured,
`go.mod` visible). The check is the instruction; a context name is a property of one machine at one
time, which is the same reason the correction above says to probe the port rather than read the
table. cleat#1694, and cleat#1667 for the platform change.

Environmental is not the same as unavoidable — that distinction cost this stream a day, and cost
WS-2 one too, because I fed them my wrong diagnosis as agreement.

**Wave 1 is the 24 result-carrying calls, not all 50.** Those are the adapters that decode
something the guest must interpret — an out buffer, a packed length, a host message — and every
one of the six defects above lived in one:

    AwaitAllChildren AwaitAnyChild AwaitChild AwaitPromise ChildWorkflow
    ChildWorkflowWithOptions CreatePromise DurableAwaitSignals DurableCall
    DurableCallWithHeartbeat DurableCallWithRetry DurableDefer DurableDeferFunc
    ListCrons PluginCall PluginCallStreaming PollCancellation PollChild
    PollSignal RunID ScheduleCron SideEffect WorkflowID

    AcquireLock

The **13** scalar-only calls — `Now`, `Random`, `DurableSleep` and the rest — carry nothing the
host computed and are wave 2.

**The split rule is "does the guest have to decode something the host computed",** and it was
"does the error path read a buffer" until WS-3's C1 found the case that separates them.
`AcquireLock` reads no buffer on its error path, so the old rule put it in wave 2 — but its
success path decodes `acquired := (result>>8)&0x1`, a bit the host computed. A guest returning a
constant `true` would compile, pass every compile-coverage check, and be silently wrong about
holding a lock. That is the §3.200 defect class, so it is wave 1.
Re-derive both lists against `wasm.AdapterFieldNames()` rather than by grepping the file, because
the struct literal's shape defeats the obvious regex:

    24 + 13 = 37, and every wave-1 name resolves — checked 2026-09-05 by diffing
    the list above against AdapterFieldNames(), which returns 37.

This said 15 until 2026-09-05, from before #735 removed `AcquireLockMs`. A wave-2 list is exactly
where a removed adapter goes unnoticed: nothing reads it until the wave starts.

### The dependency, stated rather than wished away

**WS-2 and WS-3 cannot start their main task until WS-1's harness exists.** A plan that pretends
otherwise produces three streams writing three harnesses. So each stream has an independent first
task, and the harness lands before the second.

### WS-1 — the harness, and Go as its reference implementation

| | task | done when |
|---|---|---|
| A1 | a table-driven host-call execution harness: one fixture per language, one row per call, expected-outcome table | the 24 wave-1 calls run for **Go**, each with a recorded expected outcome, and a call whose outcome changes fails |
| A2 | executed-coverage measurement becomes a guard | `sdk-host-call-coverage.py` grows an `--executed` mode over the fixtures tests actually run, with its own ratchet |
| A3 | falsify A1 | revert §3.200's fix; the harness must redden **on the Go row of a specific call**, not on a whole-fixture failure |

A3 is the acceptance test for the harness design, not a formality. §3.200 was a guest discarding
the host's message on `plugin_call`; if the harness cannot localise that to one call in one
language, it will not localise the next one either.

### WS-2 — Python and Java through the harness

| | task | done when |
|---|---|---|
| B1 | *(independent, start now)* the expected-outcome table for the 24 calls: what each returns with no backend configured | a reason per call, written down with **why**, in the §3.303 style |
| B2 | Python and Java fixtures through WS-1's harness | both languages run the 24, and their outcomes match Go's table or differ with a recorded reason |
| B3 | falsify B2 | revert §3.201; the Python row must redden on the calls that discarded their result, and Java's must not |

B1 is the part that cannot be rushed and does not need the harness. §3.303's lesson is that the
table is the test: "a key is present" passed over 16 failures, and "a reason that matches, with a
why" did not.

**B2's Python half runs in the toolchain container** and its fixture cannot be byte-pinned — see
the two #742 constraints above before starting it, not after.

### WS-3 — Rust and AssemblyScript, and the CI shape

| | task | done when |
|---|---|---|
| C1 | *(independent, start now)* decide where the harness runs and what it costs | a written answer for whether wave 1 fits the existing tier-2 jobs or needs its own, with measured job times |
| C2 | Rust and AssemblyScript fixtures through WS-1's harness | both run the 24 with recorded outcomes |
| C3 | tier and required-context wiring for whatever C1 concludes | `tiers.yaml` and `check-required-contexts.py` agree with reality, as §3.402 established |

C1 first because it may change A1. If wave 1 cannot run in CI at a tolerable cost, the harness
needs to be sampleable by call or by language, and that is a design input rather than a
retrofit — the scale suite's `assertAllSampled` (§3.401) is the cautionary case for sampling
added late.

**C1 is answered, 2026-09-05.** Wave 1 fits the existing `Cross-Language WASM E2E` job. No new
job and no sampling — **provided the harness builds each guest once and invokes it N times.**
The cost driver is fixture *shape*, not call count:

| shape | added | E2E total |
|---|---|---|
| one fixture per call (23/lang) | ~460s | 825s — 2.3× the job, within 25s of the critical path |
| one module per language, invoked N times | ~145s | ~510s, 340s below the critical path, wave 2 fits on top |

Measured job times on run 33973787289 (sha `fa6dd10a`, all green): Tier 1 Gate 848s is the
critical path, E2E 364s. **A1 is built this way and the Go reference measures 3.6s for 24
invocations plus the build.**

The number that decides it is one nobody would have guessed: **a Python invocation costs ~0.93s
and does not amortise.** Five consecutive executions of a prebuilt 19.87 MB component in one
process: 995 / 937 / 934 / 891 / 935 ms, flat. Building the component costs 1.8s *once*; every
invocation after that costs the same as the first, with no cache. So Python is ~50% of the added
cost and is the term that breaks first if anything ever multiplies invocations — a per-dialect
matrix, a retry loop, wave 1 and wave 2 under one sampling scheme.

Shape C is not new capability: `asworkflow` already exports 8 entry points from one module,
`rustworkflow` carries ~8 `#[cleat_entry]`, and the Java tree generates a `CleatEntryIndex`.
Python differs in mechanism only — `componentize-py` takes a single `--entry` and the component
always exports `run` as its sole entry point — so Python dispatches on input inside one entry
rather than exporting 24 symbols. The Go reference does the same, so the shape is uniform.

**Two costs stated as soft, because they are.** Java's build/execute split is not measured (CI
totals only), so the ~1s/invocation Java figure is the weakest number in the costing — it does
not change the verdict, since Java could be 3× that and shape C still fits, but do not quote it
as measured. And the Python CI split (5.9 build / 3.0 exec) is the Mac ratio applied to a
measured CI total: a model, and the only modelled number in it.

**Unrelated to the harness and worth more than anything it saves:** `Install Python WASM
toolchain` is 137s of E2E's 364s — 38% of the job, pure install, cacheable.

### What would make this round a failure

Not "wave 1 is incomplete" — that is a schedule outcome. It fails if **the harness lands and finds
nothing**, because six defects in two days says the next one is there, and a harness that
reports green over it is §3.303 again one level up. Every stream's falsification is a real reverted
defect for exactly that reason.

---

## The convergence metric

Findings recorded per day, from **both** places the project records them: new `IMPROVEMENT-PLAN.md`
sections, counted by **first appearance** of each section number anywhere in the tree, and new
GitHub issues, bucketed by local day. **Regenerate with `scripts/convergence.py --markdown`** — do
not retype it, and do not count `+###` diff lines (see below).

| | 09-01 | 09-02 | 09-03 | 09-04 | 09-05 | 09-06 | 09-07 | 09-08 | 09-09 | 09-10 | 09-11 | 09-12 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| sections | 27 | 16 | 23 | 17 | 32 | 17 | 19 | 3 | 4 | 14 | 9 | 9 |
| issues | 0 | 0 | 0 | 0 | 7 | 12 | 22 | 37 | 33 | 37 | 54 | 27 |
| **total** | 27 | 16 | 23 | 17 | 39 | 29 | 41 | 40 | 37 | 51 | 63 | 36 |

**The section row is the metric as it was published until 2026-09-12, and on 09-08 it read 3.**
Three, against a documented baseline of "roughly twenty a day" — which is the shape this section
tells a reader to interpret as the work finishing. It was not. **The tracker went into use on
2026-09-06** (228 of the repo's 229 issues were filed on or after that date; the 229th is #71, from
June), findings moved there, and the metric was left counting one of the two places they land. The
combined rate roughly **doubled** over the same window that the published one halved.

Note the direction, because it is the reason nobody caught it for a week: a scan that cannot see
where the answer moved reports the *flattering* number, and a flattering number does not get
re-derived. Same shape as the `pub fn` surface scan in CLAUDE.md, which reported 100% coverage
against a real 88.7%.

**Summing the two rows is not double counting**, and that was checked rather than assumed — of 229
issues, **18** are cited anywhere in either plan file, and a citation is weaker than a section:

    gh issue list --state all --limit 1000 --json number --jq '.[].number' | sort -n > /tmp/i.txt
    grep -ohE '#[0-9]{3,4}' IMPROVEMENT-PLAN.md IMPROVEMENT-PLAN-CLOSED.md \
      | tr -d '#' | sort -un > /tmp/c.txt
    comm -12 /tmp/i.txt /tmp/c.txt | wc -l

**What the total row does not establish**: a `§` section and an issue are not demonstrably the same
*size* of finding. The claim the table supports is the negative one — the finding rate did not fall
— and not a precise rate. If the two units turn out to differ systematically, the fix is to weight
them, not to go back to counting one.

**Bucket the issue side by LOCAL day**, or the two rows are in different calendars and disagree by
the offset at every midnight. Issues #1404 and #1410 carry `2026-09-13T02:24Z` and `03:46Z` and
were filed at 22:24 and 23:46 local on the 12th; a UTC bucket opens a day that has not started yet
and files two findings into it. Across this window the naive read moves 09-05 from 7 to 0 and
09-12 from 27 to 45 — larger than several of the day-to-day differences anyone would read a trend
off. `scripts/convergence.py --self-test` pins this against a fixed `-04:00`, not the machine's
zone, because on a UTC runner "is it converted?" is satisfied by doing nothing.

**When `gh` cannot be asked, the issue and total columns read `UNMEASURED`, never 0.** An offline
or unauthenticated run would otherwise reprint exactly the plan-only figure this change retires,
and it would look like a measurement.

**Partial days undercount, and by a lot.** 09-04 read **11** when it was measured at 21:30 local
and closed at **17**. The last row of this table is always partial, so a low final figure is not
the metric bending — it is the day not being over. That reading was published as evidence of a
possible dip; it was not.

Re-derive by walking every commit that touched either plan file, oldest first, and recording the
first day each `### N.M` is present in the tree — **not** by counting `+###` lines, which counts a
section again every time its heading is rewritten, and a heading is rewritten precisely when a
status marker is corrected. On 2026-09-03 that difference is 48 versus 27.

**It has still not bent.** The total row is noise around forty a day and trending up, not down.
This paragraph has now been wrong twice in the same direction, and the second time is the
instructive one: it once read "27 → 16 → 23 → 11" and called 09-04 the first plausible dip, off a
partial day; it then stood correct for a week while the number underneath it quietly stopped
measuring the thing. **A partial reading and a partial denominator produce the same sentence.**

The conclusion is unchanged: **the project is still finding work faster than a converging project
would.** The open-item count says the opposite and is the less honest of the two, because closing
fast and finding fast look identical in it. Do not quote a figure for it here — this file has
carried "1 🔴 heading out of 168" while the tree held 7 of 271. Run the unmarked/open scan in
CLAUDE.md's *Project state* section instead.

Read it once a day. The first day **the total** falls while the fix rate holds is the first
evidence that the work is finishing rather than continuing. The section row alone can no longer
answer that question.

---

## Sandboxes, databases, and shared files

Absorbed from `PARALLEL-WORKSTREAMS.md`, `WS2-STATUS.md` and `WS3-STATUS.md` when those three
were retired on 2026-09-04 (A4, R5). Both other streams agreed to it unreserved. Only content a
stream re-verified that day came across; the status halves went to `gh pr list`, which cannot go
stale, and two bullets were **dropped rather than ported** because their owner retracted them —
see "What did not come across" at the end.

**CLAUDE.md points here for the DSNs.** They are written down precisely so that nobody rebuilds
one from memory: the section around that pointer records a run where doing so produced a tidy
876 → 581 → 4 skip progression that was 1,086 connection failures wearing the right costume.

### Two rules for the shared Docker host (owner-approved 2026-09-23)

Every stream's containers share one Docker daemon, and one CLI setting, on this machine. Two
incidents on 2026-09-23 each cost more than one stream an afternoon's evidence:

**1. Never change the machine-wide docker context. Name the target instead.** `colima start` (and
`docker context use`) rewrites the *current context* for every session on the machine, not just
the one that ran it. When WS-1 started a colima VM for #982, WS-2's running SQL Server container
"vanished" from `docker ps`: it was still up, on `default`. WS-2 "recreated" it, and the copy
landed *inside* WS-1's VM, publishing the same host port. That invalidated both streams' runs from
the switch onward. So for any non-default daemon, pass the target on each command, and restore
the context if something changed it:

```
docker --context colima-<profile> ps          # or DOCKER_CONTEXT=colima-<profile> in your own shell
docker context show                           # must read `default` when you finish
```

**2. Cap every SQL Server container's memory; suspect memory before code.** SQL Server on Linux
sizes its buffer pool to 80% of the memory it can see, and inside Docker that is the whole VM,
not the container. With five SQL Server containers in one 11.7 GiB Docker VM, each planned on
about 9 GiB, and they starved one another: Errors 802 and 17300, refused logins, one container
killed (exit 137). The Mac itself was fine (`memory_pressure` read 61% free; `vm_stat`'s low "free"
is normal on macOS and is not the signal). So:

```
docker run … -e MSSQL_MEMORY_LIMIT_MB=2048 … mcr.microsoft.com/mssql/server@<digest>
```

On any odd SQL Server behaviour (a hang, a block, a "row not there"), check the host first,
before a query, lock or driver theory:

```
docker stats --no-stream
docker logs <container> 2>&1 | grep -E 'Error: (802|17300)|insufficient system memory'
```

Stop, rather than remove, the containers you are not using; `docker start` brings them back.

---

## ⚠️ CORRECTION 2026-09-16: everything below about *this machine* describes one that is gone

The reasoning about which stream can run what, and why, was measured on a **colima** host with
three VMs. There is no colima here now. Measured today, from the session sandbox at
`/Users/Shared/localssd/rcownie/cleat-agent1`:

    docker context ls                 # default, desktop-linux, orbstack (orbstack current)
    docker info --format '{{.Name}} {{.ServerVersion}} {{.MemTotal}}'
    # -> orbstack 29.4.0 12600156160

**One VM, 11.73 GiB, running OrbStack.** Not five contexts, not `colima`, `colima-cleat-ws1` or
`colima-cleat-ws3` — those three do not exist, so every `docker --context colima-…` command below
fails rather than answering.

**"WS-2 cannot have a local SQL Server 2022 on this machine, and that is a capacity fact" is
retracted.** It is the sentence most likely to stop someone trying, and it is false here. This
session ran one for its whole length:

    SELECT @@SERVERNAME, SERVERPROPERTY('ProductVersion'), SERVERPROPERTY('Edition')
    -- f57f8ff4b368 | 16.0.4275.2 | Developer Edition (64-bit)

That is SQL Server 2022, not Azure SQL Edge 15.0, and it applied every migration in
`migrations/mssql/` including `011_json_scalar_payloads.sql` — the two-argument `ISJSON` that Edge
cannot run. The image is amd64 and the host is arm64, so `docker run` warns about the platform and
the server runs under emulation: **slow, not impossible.** Budget for it — a full `./engine/` sweep
with an MSSQL DSN set took over 30 minutes and hit a timeout once.

**The capacity fact that replaces it is contention, not architecture.** 24 containers were running
on that single 11.73 GiB VM, about a dozen of them SQL Server. Under that load a server reports

    Msg 802 ... There is insufficient memory available in the buffer pool
    Error: 701, Severity: 17 ... insufficient system memory in resource pool 'default'

and the failures look like test failures: this session lost one full-suite run to a 30-minute
timeout and had another report a red on `TestAClaimDefersARunWhoseConcurrencyKeyIsHeld/mssql` that
was the container, not the code. `docker stats --no-stream` before a three-dialect run, and
`docker restart` on your own container, are cheaper than diagnosing either. That is the same
reading CORRECTION 1 below asks for — treat a startup or buffer-pool error as a memory measurement
— on a machine where the pressure comes from neighbours rather than from a VM's own `--memory`.

**Eight of the nine ports in the DSN table below are dead, and the ninth is not what the table
says.** Measured two ways that can disagree, and they agree:

    for p in 1433 1434 1435 5432 5433 5434 3306 3307 3308; do nc -z 127.0.0.1 $p && echo "$p up"; done
    docker ps --format '{{.Ports}}'

Only 1435 answers, and it is **this session's own container** — not WS-3's `cleat-ws3-mssql` as the
table records. That is the worse of the two failures to inherit: a dead port refuses and you go
looking, whereas a live port belonging to someone else connects, accepts your DSN, and answers
about the wrong database. `SELECT @@SERVERNAME` is what distinguishes them, which is what the
existing advice at the end of the SQL Server section already says and is the reason to keep saying
it.

The ports actually published are a different set entirely
(1466, 1477, 1482, 1487, 1499, 1572, 3309, 3365, …), one cluster per live session, which is what
the tables below cannot express: containers are now **per session**, created and destroyed with the
work, not three fixed servers owned by three fixed streams.

**So the rule at the end of the SQL Server section — "probe the port; do not read the table" — is
the only part of this that still holds, and it is now the whole instruction.**

**One trap in following it.** The obvious probe is broken in this shell:

    (echo >/dev/tcp/127.0.0.1/$p) 2>/dev/null && echo BUSY || echo free   # WRONG here

`/dev/tcp` is a bash feature and the tool shell is **zsh**, where the redirect fails for every port
— so it answers *"free"* for all of them, including one you are connected to. It fails in the
direction that looks like success. Use `nc -z`, and check it against a port you know is busy before
believing a "free".

**What this correction does NOT claim.** I measured the runtime, the memory, the ports and my own
server. I did **not** verify which sandbox each stream uses now, what DSNs the other sessions hold,
or when the host changed — so the tables below are left in place rather than rewritten from one
session's view. Read them as history, and probe.

**The second independent measurement arrived on 2026-09-17, which is what the paragraph above was
waiting for.** WS-2, from their own sandbox: OrbStack 29.4.0, and SQL Server 2022 16.0.4275.2 on
**1434** — the port this file's table calls Azure SQL Edge 15.0 — applying all 54 `migrations/mssql/`
files. WS-3 independently reported `docker ps -a` showing every container on the host as
`Exited (255)` after the reboot, so the containers live at any moment are ones a session rebuilt,
not survivors; ports are per session and there is no fixed assignment to recover.

Two operational notes from those runs, recorded because each cost someone time:

- **`sqlcmd -I`** (QUOTED_IDENTIFIER ON) is required, or migration `001` fails on `CREATE INDEX`
  with `Msg 1934`. Measured by WS-2 on 2026-09-17, and hit independently by WS-3 the same night on
  a different migration.

  **A probe that bypasses the real runner needs a control that uses it.** (WS-3's framing, kept
  because it is the durable half.) That failure looked exactly like a migration defect, on the
  dialect where one was most plausible. What distinguished them was that the Go test, going
  through `migration.Runner`, had already applied the same file successfully. **The flag is the
  fix; the control is what tells you the flag is the fix rather than the migration being broken.**
  Generalise past `sqlcmd`: any probe that reaches the database by a path production does not use
  — a bare client, a hand-run script, `docker exec` — can fail for reasons that belong to the path
  and present as defects in the subject.

  **And the honest account of what the note above was worth, which is weaker than it looks.** Two
  sessions hitting this independently is not evidence that writing it down helped: WS-3 had not
  read it and would not have, having gone to this section for DSNs hours earlier with no reason to
  return. The value was not that two people knew — it was that the note existed to be found by
  whoever looks next, and WS-3 was not that person. That is an argument for writing things down,
  not for coordination, and it is the weaker and truer claim. Recorded at WS-3's insistence, over
  the flattering version.
- **`ssh-add -l` can come back empty after a host restart** and `gh` keeps working throughout on its
  own token, so only git-over-ssh is broken and nothing says so until a push fails. It is **per
  session** — one session had three identities loaded while two had none. Check with
  `ssh -o BatchMode=yes -T git@github.com`; repair with `ssh-add --apple-load-keychain`. Note that
  `ssh-add <key>` returns 0 having added nothing when the passphrase prompt is swallowed, so the
  exit status cannot tell you.

---

### Which sandbox is which stream

| | sandbox | docker context |
|---|---|---|
| **WS-1** | `/localssd/rcownie/cleat` | `colima` (default) |
| **WS-2** | `/localssd/rcownie/cleat-agent1` | `colima` (default) |
| **WS-3** | `/localssd/rcownie/cleat-agent2` | `colima-cleat-ws3` |

The same tree is reachable as `/localssd/…` and as `/Users/Shared/localssd/…`, so identify a
checkout by its git *common* directory rather than `$PWD` — which is what
`scripts/section-blocks.sh` does, and it is also how the 14 `cleat-wt-*` worktrees resolve back to
the stream that owns them.

### DSNs

Each row was connected to on 2026-09-04 by the stream that owns it.

| | PostgreSQL | MySQL | SQL Server |
|---|---|---|---|
| **WS-1** | `postgres:postgres@localhost:5432` | `root:cleat@tcp(127.0.0.1:3306)` | `1433` — works since 2026-09-04 |
| **WS-2** | `cleat:cleat@localhost:5433` | `root:cleat@tcp(127.0.0.1:3307)` | `1434` — needs `encrypt=disable`; migrations fail |
| **WS-3** | `postgres:postgres@localhost:5434` | `root:cleat@tcp(127.0.0.1:3308)` | `1435` — the only working one |

    CLEAT_TEST_POSTGRES='postgres://postgres:postgres@localhost:5434/cleat?sslmode=disable'
    CLEAT_TEST_MYSQL='root:cleat@tcp(127.0.0.1:3308)/cleat?tls=false&parseTime=true&multiStatements=true'
    CLEAT_TEST_MSSQL='sqlserver://sa:CleatTest123!@127.0.0.1:1435?database=cleat'

**Credentials are port-specific, so a probe that varies only the port answers the wrong
question.** Measured across the full matrix on 2026-09-04:

| | `postgres:postgres` | `cleat:cleat` |
|---|---|---|
| 5432 | ✅ | ❌ `28P01` |
| 5433 | ❌ `28P01` | ✅ |
| 5434 | ✅ | ✅ |

MySQL has the same shape: `root:cleat` works on 3306, `cleat:cleat` is refused with `1045`. Both
streams that reported on this got a detail wrong, in opposite directions — one called 5433
"set-but-broken" after probing it with 5432's credentials, the other reported `postgres:postgres`
as failing on 5434, where it works. Re-derive with a credential × port matrix, never a port sweep:

    for p in 5432 5433 5434; do for c in postgres:postgres cleat:cleat; do
      psql "postgres://$c@localhost:$p/cleat?sslmode=disable" -c 'SELECT 1' >/dev/null 2>&1 \
        && echo "$p $c OK" || echo "$p $c FAIL"
    done; done

**And on 5434 the pair that also works is the wrong one to use.** That container was built with
`POSTGRES_USER=cleat`, and `cleat` is also a *schema* name here, so `search_path="$user",public`
resolves to a per-user schema and the test tables end up split across two of them. A `postgres`
superuser role was created for exactly this reason, which is why the row above says
`postgres:postgres` and an older draft said otherwise. **Connect as `cleat` and you see half a
schema** — not an error, half a schema, which is the failure mode this whole section is about.
That is also the hazard behind `engine/flush_rls_test.go`'s connection-pinning comment: the
symptom is `relation "<table>" does not exist` from a test whose neighbours pass.

### SQL Server: ~~two of the three streams can run it locally~~ — HISTORY, see the ⚠️ CORRECTION

Every part of this has cost a session at least once. Rewritten 2026-09-04 after 1433 was revived;
the three corrections are marked, because each of them was believed for weeks.

**The heading itself is one of the stale claims.** It asserts a capacity conclusion, so it is read
even by someone who reads nothing under it. All three streams can run SQL Server 2022 locally on
the current host; the tables below describe `colima` VMs and ports that no longer exist. Probe, do
not read.

| port | server | version | state |
|---|---|---|---|
| 1433 | `colima-cleat-ws1/cleat-ws1-mssql` | SQL Server 2022 16.0.4265.3 | **works** (since 2026-09-04) |
| 1434 | `colima/cleat-ws2-mssql` | Azure SQL Edge 15.0.2000.1574 | connects, but see below — migrations fail |
| 1435 | `colima-cleat-ws3/cleat-ws3-mssql` | SQL Server 2022 16.0.4265.3 | works |

**CORRECTION 1: 1433 was not broken, it was starved.** For four weeks it reported `Up`, accepted a
connection, dropped it (`EOF` under all four encryption settings), and logged
`Error: 17300 … Failed to start system task` on repeat. That is SQL Server failing to start under
memory pressure, not a corrupt container. The `cleat-ws1` colima profile had **4 GiB**; `cleat-ws3`,
whose identical image worked, had **6 GiB**. Raising it fixed the server outright:

    colima stop cleat-ws1 && colima start cleat-ws1 --memory 6 --cpu 2
    docker --context colima-cleat-ws1 start cleat-ws1-mssql

**So treat 17300 as a memory reading, not a diagnosis.** 4 GiB is below the floor for this image
and 6 GiB is above it; the exact floor was not measured. Nothing in the log says "out of memory",
which is why this read as corruption for a month.

**CORRECTION 2: Rosetta is on `cleat-ws1` AND `cleat-ws3`, not only ws3.** This file said only ws3
had it. The image is amd64-only, so both profiles need it and both have it; `default` does not,
which is the real reason WS-2's server is Azure SQL Edge rather than SQL Server. Re-derive:

    grep -E '^(rosetta|memory|arch|vmType):' ~/.colima/{default,cleat-ws1,cleat-ws3}/colima.yaml

**CORRECTION 3: the 1435 collision is resolved.** Two containers did publish host port 1435 — the
**`colima`** context's `cleat-ws1-mssql` (Azure SQL Edge) and `colima-cleat-ws3`'s
`cleat-ws3-mssql`. This file blamed the `default` context, which holds **no containers at all**,
so anyone following it looked in the wrong VM. The Edge duplicate was removed 2026-09-04
(`docker --context colima rm -f cleat-ws1-mssql`); it could not run the migrations anyway, so
nothing was lost.

**Keep the hazard in mind even though this instance is gone.** `engine/testutil`'s
`CleanupMSSQLTestData` (`engine/testutil/mssql_schema.go:118`) is an unqualified `DELETE FROM`
across **15 tables**. Two containers racing for one host port means bind order decides which server
a suite wipes, and `-p 1` cannot help — it serialises packages inside one `go test`, not streams
across VMs.

**1434 needs `encrypt=disable`, and this is new.** The default DSN shape fails on
`x509: negative serial number` — Azure SQL Edge's self-signed certificate. Measured across four
settings: only `encrypt=disable` connects; `TrustServerCertificate=true` does **not** help, because
the certificate fails to parse before trust is considered.

**1434 still cannot run this repo's migrations.** `migrations/mssql/011_json_scalar_payloads.sql`
uses the two-argument `ISJSON`, introduced in SQL Server 2022; Edge is 15.0, so all 540 failures
come from that one file. CI covers MSSQL, so this bounds local work only — but a "passes on three
dialects" claim made from that sandbox is not one.

**~~WS-2 cannot have a local SQL Server 2022 on this machine, and that is a capacity fact.~~
RETRACTED — see the ⚠️ CORRECTION above.** The host had 24 GiB and three VMs committing 20
(default 8, cleat-ws1 6, cleat-ws3 6), so a fourth Rosetta VM did not fit and shrinking `default`
was the only route. **None of those VMs exists now**: the runtime is OrbStack, one VM, and two
sessions have since run SQL Server 2022 locally.

The retraction is repeated here rather than only 170 lines above because **this is where the claim
is read**. A reader who wants to run SQL Server opens `### SQL Server: …` and meets this paragraph
with the correction off-screen and under a different heading — which is how it was met on
2026-09-17, by a session that had read the correction. A retraction that lives only at the top of a
document is a retraction the copy does not carry.

**`colima start` rewrites the global docker context.** Set it back with `docker context use colima`
or every other stream's bare `docker` silently retargets.

**So probe the port; do not read the table.** `docker ps` answers a different question:

    docker context ls                       # there are five; a bare `docker ps` sees one
    docker --context colima-cleat-ws3 ps -a
    # then ask the server which server it is:
    SELECT @@SERVERNAME, SERVERPROPERTY('ProductVersion')

`@@SERVERNAME` is the container ID, so it distinguishes two instances that share a name and a port.
That is what settled the 1435 question: `1a4890c33e6e`, WS-3's container.

### PostgreSQL: unqualified names resolve differently per database

`admin.tenant_api_keys` is the only cleanup table outside the default schema, and referring to it
unqualified was a live defect in two places until 2026-09-04 (#720, #721). An unqualified name
resolves through `search_path` (`"$user", public`), so on the 5433 sandbox — whose role is named
`cleat` — it resolved somewhere different from 5432 and 5434.

Two things that cost time here and are worth stating so nobody re-derives them:

- **The `cleat` schema is legitimate and exists on all three.** `assert_tenant_set()` lives in it
  and eleven RLS policies depend on it. It is not a stray, and `DROP SCHEMA cleat` is not a
  cleanup step — PostgreSQL refuses it, which is the only reason a session that tried did not
  break RLS on 5433.
- **A stray `tenant_api_keys` beside it *was* a defect**, manufactured by an unqualified
  `CREATE TABLE` in `cmd/cleat-worker/auth_test.go`. Fixed in #721; the copies were dropped by
  hand the same day. If one reappears, that test is the first place to look.

Baseline after the 2026-09-04 cleanup, true of all three: one `tenant_api_keys` (in `admin`), one
tenant (`default`), one `tenant_` schema. Re-derive:

    select table_schema||'.'||table_name from information_schema.tables
     where table_name='tenant_api_keys';

### Shared files, and the protocol for each

**A checkout is a shared file too, and so is a WORKTREE inside it.** Added 2026-09-17, after
three sessions spent an hour failing to attribute three PRs and one of us rewrote a branch
another session was holding.

The commits behind `#1729`, `#1732` and `#1733` were created in checkouts two streams each
believed were theirs, by a session that was neither of them. The evidence is a reflog, and it is
worth stating because it is stronger than the caveat it replaces:

    # in WS-2's OWN checkout, worktrees/cleat-wt-proto/HEAD
    20:44:38  commit: fix: a member the scan did not parse was skipped and passed
    21:55:28  rebase (finish) onto 4fc62f8a          <- the only entries WS-2 made

So `git worktree list` showing you a worktree is **not** evidence you own it. WS-2 read "in my
checkout" as "mine", rebased and force-pushed — `--force-with-lease` protected the remote ref and
protected nothing about the other session's working tree.

**THE TRAILER SAYS WHO WORKED; THE REFLOG SAYS WHERE. They answer different questions and
neither substitutes for the other.** The failure mode to design against is not a session stamping
the wrong stream on purpose — it is a session stamping the **right** one for a commit produced in
another stream's working tree. That is exactly tonight's case: whichever session made `54b94a19`
would have marked it with its own stream, correctly, while the tree belonged to someone else. The
trailer would have been true and still would not have answered the question anyone was asking.
Only the reflog is unforgeable by typing, and only the trailer names a person. (WS-1's framing.)

**What follows for attribution.** Two signals exist and both resolve to the *working tree*, not the
session: the reflog says which checkout created an object, and the checkout has no single occupant.
The `Claude-Stream:` trailer — `WS-1`, `WS-2`, `WS-3`, `coordinator`, adopted 2026-09-17 — is the
only stream-scoped signal, and it replaced a session id for one reason: **a stream survives a host
restart and a session id does not.** Every `session_…` mapping recorded before 2026-09-16 now names
a session that no longer exists. A stream name is also a value each session can assert from its own
evidence rather than resolve from an id it cannot read.

The older `Claude-Session:` trailer was **absent** for at least two streams — one omits it deliberately because `CLAUDE_CODE_SESSION_ID` is a UUID, the
form that appears in 0 of 741 trailers, and emitting a well-formed marker that resolves to nothing
is worse than emitting none. **So `UNKNOWN` is the correct answer for an unmarked commit, and
looking for a fourth signal is how you get a confident wrong one.** Session ids also rotate on a
host restart, so an id-to-stream table is only as good as its date — every mapping in this repo's
notes predates 2026-09-16 and several are now stale in both directions.

**The cheap check before touching a worktree you did not create is its own HEAD reflog**, not the
branch's: `git reflog worktrees/<name>/HEAD`. Prefer that targeted form because it is unambiguous
and does not depend on the reader knowing what `--all` sweeps.

**It is NOT true that `git reflog --all` misses other worktrees' HEADs**, and this file said so
until it was measured. On **git 2.50.1 (Apple Git-155)** `--all` walks `worktrees/<name>/HEAD`
along with the branch refs — a commit made in a worktree appears twice, once via its branch and
once via that HEAD:

    $ git reflog --all --format='%gD' | sed 's/@.*//' | sort -u
    HEAD
    refs/heads/main
    refs/heads/probebranch
    worktrees/wt/HEAD

So a negative from `--all` **is** informative here, and the retracted clause was the dangerous
half: a reader who believed it would discard a true negative and re-derive by hand. Measured
independently by two sessions on this host, each with a known-positive on the grep so it was not a
search that could only say no. **Pin the version rather than the fact** — per-worktree coverage in
`--all` is exactly the sort of thing that changed at some release, and neither measurement tested
an older git. Re-derive elsewhere: `git init`, commit, `git worktree add`, commit inside it,
`git reflog --all`.

| file | protocol |
|---|---|
| `IMPROVEMENT-PLAN.md` | Edit only your own `§` sections. **Do not pick a number — run `scripts/next-section-number.sh`.** Blocks are per stream (WS-1 `3.200–299`, WS-2 `3.300–399`, WS-3 `3.400–499`; `scripts/section-blocks.sh`), and `.githooks/pre-commit` refuses a commit that adds one outside yours. The script reads `origin/develop`, so it cannot see a number already claimed by an *open* PR — open two at once and it hands out the same number twice. Take the second by hand and say so in the PR. Closed sections are archived by `scripts/archive-closed-sections.py`, never deleted. |
| `scripts/skip-ledger.tsv` | **Add a line; never edit a number.** A job's budget is the sum of its lines, so two streams adding skips do not contend for one total. Attribute a new skip by test name, never by delta. `test-go/engine` and `cluster` move together — the cluster job also runs `./engine/...`. |
| `scripts/skip-baseline.txt` | Never hand-edit. Regenerate with `scripts/check-skips.sh --update` **after** rebasing. A count going down is the point; a count going up needs a sentence. |
| `scripts/deadcode-baseline.txt` | Same; `scripts/check-test-only-code.sh --update`. A shrinking baseline is the honest evidence that wiring landed. |
| `migrations/{postgres,mysql,mssql}/` | Numbered per dialect. **Take the next free number above the dialect's high-water mark** — see below; the reserved blocks are gone. |
| `.golangci.yml` | One linter per PR, repo-wide, and say in the PR which one you are taking. |
| `tiers.yaml` | The support manifest. Do not claim in prose what it does not grant. |
| `engine/testutil/` | WS-1's this round. Ask before adding test-schema columns. |
| `engine/store_lifecycle.go` | Shared: WS-1 owns the idempotency block, WS-2 the event and flush paths. Expect textual conflicts, not semantic ones; rebase often. |
| `.github/workflows/`, `cmd/cleat-worker/` | WS-3's. Another stream may add there when leaving the mechanism unwired would be worse — say so in the comment, as `ci.yml` and `setup.go` both do. |

**The per-stream migration ranges are retired, and this is the second time that has been
written.** `010–019`/`020–029`/`030–039` were "sparse and above the high-water mark so no
stream renumbers another"; four weeks of merges consumed them. Re-derived 2026-09-04 with
`for d in postgres mysql mssql; do ls migrations/$d/*.sql | sed 's#.*/##;s/_.*//' | tr '\n' ' '; echo; done`:

| dialect | numbers in use | high-water | free in the `030` block |
|---|---|---|---|
| postgres | 001–005, 010, 020–024, 031–040 | `040` | `030` |
| mysql | 001–004, 010, 020–022, 030, 033–039 | `039` | `031`, `032` |
| mssql | 001–004, 010–013, 020–022, 031, 033–043 | `043` | `030`, `032` |

Note what that shows, beyond the block being full: the numbers are **not** aligned across dialects
— `033` is a different migration in each — and the `030` block was used by whoever needed the next
number, not by WS-3. A block is a collision-avoidance device between concurrent writers, not a
per-stream namespace, so **a migration numbered in another stream's block is not a defect to go
fix.** `migrations/postgres/010_idempotency_keys_tenant_id.sql` carries a comment naming the old
reservation; it is a record of why that file is `010`, not a rule still in force.

**Do not re-reserve fresh blocks.** An earlier draft did exactly that (`040/050/060`) before #563
landed. #563's rule is better for the reason it gives about section numbers: a reservation needs
every writer to remember it every time, and the table above is the evidence that they do not.

**On the linter backlog, read `.golangci.yml` and not a count written anywhere else** — including
here. Restating a number in two files is how the two come to disagree, and the count that used to
live in the retired file was four measurements stale when it was retired. One caveat worth keeping
because it produces a *tidy table of zeroes* rather than an error: on this machine the pinned
`golangci-lint` v1.64.7 cannot read the installed Go 1.27 toolchain's export data
(`export data version 4 is greater than maximum supported version 2`), so it emits typecheck
errors instead of findings and every type-aware linter reads `0`. CI is unaffected — `lint-go`
pins Go 1.25. Locally, pin it too, and confirm a non-zero count for a linter you know has findings
before believing a zero for one you hope does not:

    GOTOOLCHAIN=go1.25.11 golangci-lint run --timeout=20m -c <one-linter.yml> ./... | grep -c '(<linter>)'

### What did not come across, and where the old citations point

Two bullets from `WS3-STATUS.md` were **retracted by WS-3 rather than ported**: one said
`CGO_ENABLED=0` "runs everything on wazero", which stopped being true when the wazero *backend*
was deleted in #459 — CLAUDE.md carries the correct version, which is that there is no backend
left at all — and one said `componentize-py` cannot run on this machine, which was a WS-3-machine
fact stated as a general one. The `CREATE TABLE IF NOT EXISTS` warning is not repeated here
because CLAUDE.md already carries it.

`IMPROVEMENT-PLAN-CLOSED.md` and `REMEDIATION-PLAN-2026-08-09.md` still name the three retired
files. Those citations were **deliberately left alone**: both documents are historical records —
one an archive of closed items, the other explicitly superseded — and repointing a dated quotation
at a file that did not exist when it was written would falsify it. Read them as history and use
`git log --follow` for the text. Every *live* pointer was repointed here or to CLAUDE.md in the
same PR, so nothing a reader is told to go and read is missing.

One of those citations was already stale before any of this: `REMEDIATION-PLAN-2026-08-09.md:19`
cites `PARALLEL-WORKSTREAMS.md:108` for the migration ranges, and line 108 had drifted onto an
unrelated paragraph about `errcheck`. **A line-number citation into a living document is a dead
citation with a delay**, which is its own argument for putting the reference material somewhere it
can be cited by section name.
