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

Written 2026-09-04, after a review of the tree, both gates, and the two plan files. **The previous
round is done**: A1–A4, B1–B2 and C1 all closed, C2 ranked. C3 is the only task that did not move,
and it returns below as WS-3's C3.

The review's conclusion is that the *engineering discipline* here is in better shape than the
*release claims*. The tree is clean by every count that was checked — 6 `TODO`s in all of Go, 20
dead exports, no 🔴 heading left in the plan (`grep -cE '^### .*🔴' IMPROVEMENT-PLAN.md` → 0; a
bare `grep -c '🔴'` returns 3, all of them prose inside §3.113's note about the last one), and
4642 pass / 0 fail / 6 skip locally across all three dialects.
What is not clean is the set of places where a **claim** and a **check** have drifted apart. All
six findings below are that same shape, which is why they are worth a round.

| # | finding | evidence, re-derivable |
|---|---|---|
| 1 | §2.26's remainder — 2 of the MSSQL store's files still have unretried transaction boundaries, and its stated blocker cleared a month ago | `grep -c 'withRollbackGuaranteedRetry(' engine/mssql_events.go engine/mssql_signals_promises.go` → 0 and 0; every other MSSQL file uses it (28 production call sites) |
| 2 | `Test Go (scale)` is a **required check** on a **tier-2** package, whose contract permits failure | required contexts include it; `tiers.yaml` `tier2.packages` includes `./tests/scale/...` |
| 3 | The convergence metric double-counts, and inflates when a status marker is corrected | 2026-09-03: 48 `+###` lines, **27** distinct sections, §3.94 counted 5× |
| 4 | Assertion-shaped skips are grandfathered into the skip baseline | `tests/plugin-harness/wasm_plugin_test.go:757,760` skip on a JSON decode failure |
| 5 | `gosec` is disabled, so §3.33's 281 classified findings are unenforced | `.golangci.yml` `disable:` lists `gosec`, `errcheck`, `unused`, `gocyclo` |
| 6 | `CONTRIBUTING.md`'s release section documents three things the repo does not do | no `windows` in `.goreleaser.yml`; `git log -S'cargo publish'` is **empty**; no `ghcr.io` under `.github/`, no `dockers:` in `.goreleaser.yml` |

Finding 2 is the release blocker: a published claim that no check defends. Finding 1 is a month-old
task whose blocker cleared and whose marker never moved. Finding 3 is why nobody can currently say
whether the project is close to done. Each stream takes one substantive item, one guard, and the
falsification of its own guard.

**Take section numbers from `scripts/next-section-number.sh --stream WS-N`, not from this table.**
As of writing it hands out WS-1 `3.200`, WS-2 `3.303`, WS-3 `3.401`, and those move as siblings land.

### WS-1 — a tier-1 dialect whose retry path is dead code

| | task | done when |
|---|---|---|
| A1 | §2.26's remainder: wrap the transaction boundaries in `engine/mssql_events.go` and `engine/mssql_signals_promises.go` in `withRollbackGuaranteedRetry` | both files use the wrapper, a test proves a 1205 deadlock victim is replayed on those paths, and §2.26's "Still to do" paragraph is retired |
| A2 | replace the convergence metric with a first-appearance count | `scripts/convergence.py` prints one row per day, carries a `--self-test` that fails on the double-counting input below, and the table in this file is regenerated from it |
| A3 | falsify A1 | unwrap one boundary, watch the A1 test go red, and **check the message names the deadlock** — not a connection error or a constraint violation standing in for it |

A1 is the single `OPEN` marker left in `IMPROVEMENT-PLAN.md`, and it is narrower than that marker
makes it sound. **The first draft of this plan got it wrong in a way worth recording**, because the
same mistake is available to whoever picks it up. Grepping for `mssqlRetry` finds it called from
nothing but tests, which reads as "SQL Server has no retry path" — a tier-1 dialect claim with
nothing behind it. That is not the situation. The live wrapper has a different name,
`withRollbackGuaranteedRetry`, and it has **28 production call sites across 7 files**.

`mssqlRetry` is uncalled *by design*, and wiring it would be a defect. Its own doc comment says so:
it gates on `isMSSQLRetryable`, which includes timeouts (258) and dropped connections — errors that
leave the outcome **unknown**, where the commit may have succeeded and only the acknowledgement was
lost. Replaying a non-idempotent transaction after one of those double-applies it, which for a
workflow engine is a duplicated side effect. `withRollbackGuaranteedRetry` gates on the narrower
set — deadlock victim (1205), snapshot conflicts (3960, 41301–41325) — where the server has
definitively undone the work. **Do not wire `mssqlRetry`.**

What is actually left is §2.26's own last paragraph: `mssql_events.go` and
`mssql_signals_promises.go` were deferred because §2.60 was changing them, and told to wait. §2.60
landed as #283 on 2026-08-04. **The task has been unblocked for a month and the marker never
moved** — the same stale-marker shape as §3.113, which cost a session last week.

One number to re-derive rather than inherit: §2.26 says 9 boundaries, and `grep -c BeginTx` over
the two files gives 3. That section has already corrected its own count once — it says so, "there
are ~20 transaction boundaries in the MSSQL store, not 8." Count them before wrapping them.

A2 matters because it is the instrument the project steers by. Measured 2026-09-04, the published
command counts `+### ` diff lines, so a section is counted once per commit that rewrites its
heading — and rewriting a heading is exactly what correcting a status marker does. **The metric
punishes the discipline CLAUDE.md most insists on.**

    git log --since="2026-09-03 00:00" --until="2026-09-03 23:59" -p --format="" \
      -- IMPROVEMENT-PLAN.md | grep -cE '^\+### [0-9]+\.[0-9]+ '     # 48
    # distinct section numbers among those 48: 27. Thirteen counted 2-5 times; 3.94 five times.

### WS-2 — the Java result path, and the skip that hid it

| | task | done when |
|---|---|---|
| B1 | convert the four assertion-shaped skips in `tests/plugin-harness/wasm_plugin_test.go` (757, 760, 311, 350) to failures | a Java/TeaVM module returning an unparseable result **fails** the harness |
| B2 | falsify B1 against the code the test actually compiles | each converted assertion goes red on its own line, under its own perturbation of `testdata/javaworkflow/` |
| B3 | correct the release section, and explain the version spread rather than flattening it | every claim in `CONTRIBUTING.md`'s release section is true of the repo or removed; the four SDK versions are explained, **not** normalised |

B1 is not hygiene. Lines 757 and 760 read `t.Skipf("failed to decode outer wrapper: %v")` and
`t.Skipf("failed to parse result JSON: %v")` — so a Java plugin workflow that returns garbage is
reported as a skip, which CI reads as a pass. It landed as #724: **five** sites, not the four this
plan first named.

**B2's first draft named the wrong falsification target, and it failed in the direction that looks
like success.** It said to revert #455 — "a Java workflow's result is now a JSON object, not JSON
in a string" — calling that the exact defect these lines had been swallowing. Two things were
wrong. #455 is 2026-08-09, not recent; `develop` is at #722. And its edits under
`crates/cleat-java/` are **javadoc only**:

    git show 115b421 -- crates/cleat-java/ | grep -E '^[+-]' | grep -vE '^(\+\+\+|---)' \
      | sed 's/^[+-]//;s/^[[:space:]]*//' | grep -vE '^(\*|/\*\*|\*/|$)'    # prints nothing

Its code changes went to `examples/saga-java-port`, `engine/java_workflow_e2e_test.go` and
`tiers.yaml`. `TestPluginCalls_Wasm_Java` compiles `tests/plugin-harness/testdata/javaworkflow/`,
which #455 never touched — so the revert leaves the test green, and the draft's own wording ("if
reverting #455 does not turn B1 red, B1 is not done") would then have rejected a **correct** B1.
**A fix and a test can be about the same defect and share no code.** Check the revert reaches the
test's build inputs before trusting it as a control.

The general form, which is what WS-2 ran instead: perturb the code the test actually compiles,
once per assertion, and confirm each goes red on *its own* line — which also proves the assertions
are independent rather than one being reached twice. Applying #455's own fix to this workflow's
`return` hits the outer decode; returning a non-JSON literal hits the inner parse; a bad `ReadDir`
path hits the third. All three printed `SKIP` before the change.

`scripts/check-skips.sh` will not catch any of this on its own — it is a set-membership guard, so
it blocks a *new* silent skip but grandfathers the 214 already in the baseline. Converting a skip
lowers a count, which the guard reports and never fails on. **Regenerate the baseline after,** per
this file's protocol for that file. #724 took it 214 → 209.

**B3 turned out larger than a version mismatch, and "one release-version story" was the wrong
instruction.** The four numbers are not drift to be reconciled; they record which packages have
ever shipped. `CONTRIBUTING.md`'s release section claims Windows binaries, crates.io publishing of
`cleat-macro` and `cleat-sdk`, and a GHCR Docker push. The repo does none of the three, and
`git log -S'cargo publish'` is **empty rather than stale** — it was never true. Only Go is
versioned by the repo tag; of the other four only Python has a publisher at all, and
`publish-pypi.yml` has never run. Normalising the three `0.1.0`s would have asserted a `0.2.0` for
packages whose `0.1.0` never shipped. Left open as §3.304 on purpose: the repair publishes
irreversibly, so it is the owner's call.

### WS-3 — two CI contracts that contradict the manifest

| | task | done when |
|---|---|---|
| C1 | reconcile the required-check list with `tiers.yaml` | a script asserts that every required context maps to a tier-1 package, or that its tier-2 package is named with a reason; `tests/scale` lands on one side or the other |
| C2 | remove the wall-clock thresholds from `tests/scale/latency_test.go` | no assertion in the scale suite compares a measured duration to a constant |
| C3 | §3.33 — re-enable `gosec` behind a baseline file | `gosec` is out of `.golangci.yml`'s `disable:` list, its findings are a ceiling that can only shrink, and CI fails on 282 |

C1 and C2 are one incident seen from two sides. `Test Go (scale) on 1.26` is one of the required
contexts on `develop`, and `./tests/scale/...` is in `tier2.packages` — tier 2's contract is "must
*run*; may fail against a tracked list", and `tier2.known_failures` is `[]`. So a package the
manifest permits to fail is blocking merges, and the list that was supposed to make that visible is
empty. It failed on `491a0f71` (#720, already merged) and passed on the 13 other recent runs.

C2 is the reason it failed, and CLAUDE.md is explicit: *"If an assertion depends on wall-clock
time, remove the timing rather than widening it."* **Do not widen the threshold.** The distribution
says something more interesting anyway — 200 samples, P50 2.676ms, and two samples at 623.876ms and
625.203ms:

    latency_test.go:144: P99 latency 623.876463ms exceeds threshold 500ms

That is bimodal, not runner noise: a P50 of 2.7ms with a pair of near-identical ~624ms outliers
looks like a fixed stall — a lock wait, a retry backoff, a connection-pool timeout — not a slow
machine. **Find what the 624ms is before deleting the assertion**, and if it is a real stall, that
is a finding worth its own section rather than a threshold to remove.

C3 fits `.golangci.yml`'s one-linter-per-PR protocol; say in the PR that you are taking `gosec`.
Note that §3.33's own count is recorded as not re-derivable — `golangci-lint` was not installed when
it was checked — so **re-measure before writing the baseline**, and let the new number be the one
that carries the date.

---

## The convergence metric

New `IMPROVEMENT-PLAN.md` sections per day, counted by **first appearance** of each section number
anywhere in the tree. Corrected 2026-09-04; the previous table on this line counted `+###` diff
lines and roughly doubled every figure. WS-1's A2 turns this into a script.

| 08-31 | 09-01 | 09-02 | 09-03 | 09-04 |
|---|---|---|---|---|
| 4 | 27 | 16 | 23 | 11* |

\* to 21:30 local. The superseded row read 8 / 37 / 37 / 48 / 9.

Re-derive by walking every commit that touched either plan file, oldest first, and recording the
first day each `### N.M` is present in the tree — **not** by counting `+###` lines, which counts a
section again every time its heading is rewritten, and a heading is rewritten precisely when a
status marker is corrected. On 2026-09-03 that difference is 48 versus 27.

**It has still not bent.** 27 → 16 → 23 → 11 is noise around roughly twenty a day, and 09-04's 11
is a nearly-complete day, so it is the first plausible dip — one point, not a trend. The corrected
numbers change the magnitude and not the conclusion: **the project is still finding work faster
than a converging project would.** The open-item count says the opposite (0 🔴, 157 of 158 closed)
and it is the less honest of the two, because closing fast and finding fast look identical in it.

Read it once a day. The first day it falls while the fix rate holds is the first evidence that the
work is finishing rather than continuing.

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

### SQL Server: two of the three streams can run it locally

Every part of this has cost a session at least once. Rewritten 2026-09-04 after 1433 was revived;
the three corrections are marked, because each of them was believed for weeks.

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

**WS-2 cannot have a local SQL Server 2022 on this machine, and that is a capacity fact.** The host
has 24 GiB; the three VMs now commit 20 (default 8, cleat-ws1 6, cleat-ws3 6). A fourth Rosetta VM
does not fit. Shrinking `default` is the only route and it runs all six Postgres and MySQL
containers.

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
