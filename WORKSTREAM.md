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
| go | 11 | 38 | 38 (on #735) |
| python | 10 | 73 | 73 (on #736) |
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

**Wave 1 is the 23 result-carrying calls, not all 50.** Those are the adapters that decode
something the guest must interpret — an out buffer, a packed length, a host message — and every
one of the six defects above lived in one:

    AwaitAllChildren AwaitAnyChild AwaitChild AwaitPromise ChildWorkflow
    ChildWorkflowWithOptions CreatePromise DurableAwaitSignals DurableCall
    DurableCallWithHeartbeat DurableCallWithRetry DurableDefer DurableDeferFunc
    ListCrons PluginCall PluginCallStreaming PollCancellation PollChild
    PollSignal RunID ScheduleCron SideEffect WorkflowID

The 15 scalar-only calls — `AcquireLock`, `Now`, `Random`, `DurableSleep` and the rest — carry no
buffer to mis-decode and are wave 2. Re-derive both lists from `wasm/adapter_metadata.go`; the
split is "does the error path read a buffer".

### The dependency, stated rather than wished away

**WS-2 and WS-3 cannot start their main task until WS-1's harness exists.** A plan that pretends
otherwise produces three streams writing three harnesses. So each stream has an independent first
task, and the harness lands before the second.

### WS-1 — the harness, and Go as its reference implementation

| | task | done when |
|---|---|---|
| A1 | a table-driven host-call execution harness: one fixture per language, one row per call, expected-outcome table | the 23 wave-1 calls run for **Go**, each with a recorded expected outcome, and a call whose outcome changes fails |
| A2 | executed-coverage measurement becomes a guard | `sdk-host-call-coverage.py` grows an `--executed` mode over the fixtures tests actually run, with its own ratchet |
| A3 | falsify A1 | revert §3.200's fix; the harness must redden **on the Go row of a specific call**, not on a whole-fixture failure |

A3 is the acceptance test for the harness design, not a formality. §3.200 was a guest discarding
the host's message on `plugin_call`; if the harness cannot localise that to one call in one
language, it will not localise the next one either.

### WS-2 — Python and Java through the harness

| | task | done when |
|---|---|---|
| B1 | *(independent, start now)* the expected-outcome table for the 23 calls: what each returns with no backend configured | a reason per call, written down with **why**, in the §3.303 style |
| B2 | Python and Java fixtures through WS-1's harness | both languages run the 23, and their outcomes match Go's table or differ with a recorded reason |
| B3 | falsify B2 | revert §3.201; the Python row must redden on the calls that discarded their result, and Java's must not |

B1 is the part that cannot be rushed and does not need the harness. §3.303's lesson is that the
table is the test: "a key is present" passed over 16 failures, and "a reason that matches, with a
why" did not.

### WS-3 — Rust and AssemblyScript, and the CI shape

| | task | done when |
|---|---|---|
| C1 | *(independent, start now)* decide where the harness runs and what it costs | a written answer for whether wave 1 fits the existing tier-2 jobs or needs its own, with measured job times |
| C2 | Rust and AssemblyScript fixtures through WS-1's harness | both run the 23 with recorded outcomes |
| C3 | tier and required-context wiring for whatever C1 concludes | `tiers.yaml` and `check-required-contexts.py` agree with reality, as §3.402 established |

C1 first because it may change A1. If wave 1 cannot run in CI at a tolerable cost, the harness
needs to be sampleable by call or by language, and that is a design input rather than a
retrofit — the scale suite's `assertAllSampled` (§3.401) is the cautionary case for sampling
added late.

### What would make this round a failure

Not "wave 1 is incomplete" — that is a schedule outcome. It fails if **the harness lands and finds
nothing**, because six defects in two days says the next one is there, and a harness that
reports green over it is §3.303 again one level up. Every stream's falsification is a real reverted
defect for exactly that reason.

---

## The convergence metric

New `IMPROVEMENT-PLAN.md` sections per day, counted by **first appearance** of each section number
anywhere in the tree. **Regenerate with `scripts/convergence.py --markdown`** — do not retype it,
and do not count `+###` diff lines (see below).

| 08-31 | 09-01 | 09-02 | 09-03 | 09-04 | 09-05 |
|---|---|---|---|---|---|
| 4 | 27 | 16 | 23 | 17 | 4* |

\* partial day. The superseded row read 8 / 37 / 37 / 48 / 9.

**Partial days undercount, and by a lot.** 09-04 read **11** when it was measured at 21:30 local
and closed at **17**. The last row of this table is always partial, so a low final figure is not
the metric bending — it is the day not being over. That reading was published as evidence of a
possible dip; it was not.

Re-derive by walking every commit that touched either plan file, oldest first, and recording the
first day each `### N.M` is present in the tree — **not** by counting `+###` lines, which counts a
section again every time its heading is rewritten, and a heading is rewritten precisely when a
status marker is corrected. On 2026-09-03 that difference is 48 versus 27.

**It has still not bent.** 27 → 16 → 23 → 17 is noise around roughly twenty a day. This paragraph
previously read "27 → 16 → 23 → 11" and called 09-04 "a nearly-complete day, so it is the first
plausible dip". **That was wrong, and wrong in the way this section is about**: 09-04 was measured
at 21:30 local and closed at 17, so the dip was six sections of day left. A partial reading was
published as evidence of the very trend the metric exists to detect.

The conclusion is unchanged: **the project is still finding work faster than a converging project
would.** The open-item count says the opposite — 1 🔴 heading out of 168 in the plan — and it is the less honest
of the two, because closing fast and finding fast look identical in it.

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
