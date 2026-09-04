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
  admin-dashboard entry a month; `WS3-STATUS.md` five days without a touch while WS-3 merged daily).

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
Retire them into this one.

**R6 — Never hold two open PRs that touch the same declaration file.** Land the first, rebase the
second. WS-1's own #680 and #682 collided on the three SDK parity lists because both were open at
once; the merge order decided which one had to be resolved by hand.

**R7 — Freeze the surface while converging.** No new SDK capability, no new host call, no new
dialect until the existing matrix is guarded. Every addition multiplies 5 languages × 3 dialects,
and §3.111 measured what that costs: seven remaining calls ≈ 40 guest-side edits.

**R8 — A cross-stream claim carries the command that produced it.** This already works and is the
one thing not to change: WS-1 and WS-2 each caught real errors in the other's work this week, every
time by re-deriving rather than accepting. Keep the evidence attached.

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

Three streams, no shared files between them by construction. **Each stream's first task is the one
that removes its own future friction.**

### WS-1 — remove the shared state

| | task | done when |
|---|---|---|
| A1 | `skip-budget.txt` total becomes derived from per-test declarations | ~~done (#696)~~ `skip-budget.txt` is deleted; budgets are summed from `scripts/skip-ledger.tsv`, one line per reason |
| A2 | per-stream section blocks (R2), enforced | ~~done~~ `check-section-numbers.sh` rejects any 3.x number outside every block, with a `--self-test` negative control; `next-section-number.sh` hands each stream its next free number |
| A3 | archive closed sections out of `IMPROVEMENT-PLAN.md` (R3) | plan holds open items; every `§N.M` cited from code or docs still resolves — verified by a reference check, not by eye |
| A4 | retire `PARALLEL-WORKSTREAMS.md`, `WS2-STATUS.md`, `WS3-STATUS.md` into this file (R5) | one coordination file |

A1 and A2 first: they are what let A3 and the other streams proceed without conflicts.

### WS-2 — close the Python gap, then the frontier it blocks

| | task | done when |
|---|---|---|
| B1 | §3.113 — Python SDK binds and checks host results | a refused `signal_workflow` raises rather than returning `None` |
| B2 | the seven "start something" calls of §3.111, now unblocked by B1 | each guarded host-side with its guest half in all five SDKs |

B1 is a live defect, not only a prerequisite: a Python workflow whose signal is refused by the auth
check is currently told nothing.

### WS-3 — make discovery systematic

| | task | done when |
|---|---|---|
| C1 | enumerate the boundaries: host↔guest ABI (5 SDKs × ~60 calls), store↔dialect, doc↔code | a table with a row per boundary and a column for "guarded by" |
| C2 | guard the highest-yield unguarded boundary from C1 | one new guard, falsified per the protocol above |
| C3 | §3.33 — gosec's 281 classified findings: fix or justify the top 20 by severity | none of the 20 is unexplained |

C1 is the point. Every guard built this week found a defect **on its first run**, so the expected
value of the next one is high and known — but the choice of where to look is currently made by
stumbling. C1 converts a random walk into a sweep with a finish line.

---

## The convergence metric

New `IMPROVEMENT-PLAN.md` sections created per day:

| 08-31 | 09-01 | 09-02 | 09-03 | 09-04 |
|---|---|---|---|---|
| 8 | 37 | 37 | **48** | 9* |

\* partial day at time of writing.

Re-derive:

    git log --since="<day> 00:00" --until="<day> 23:59" -p --format="" -- IMPROVEMENT-PLAN.md \
      | grep -cE '^\+### [0-9]+\.[0-9]+ '

**This is the number that says whether the project is converging, and it has not bent yet.** The
open-item count does not say it: 129 of 151 sections were marked fixed on 09-04 while the discovery
rate was at its peak. Closing items fast and finding them fast is not convergence.

Read it once a day. The first day it falls while the fix rate holds is the first evidence that the
work is finishing rather than continuing.
