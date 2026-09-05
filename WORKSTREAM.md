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

Three streams, no shared files between them by construction. **Each stream's first task is the one
that removes its own future friction.**

### WS-1 — remove the shared state

| | task | done when |
|---|---|---|
| A1 | `skip-budget.txt` total becomes derived from per-test declarations | ~~done (#696)~~ `skip-budget.txt` is deleted; budgets are summed from `scripts/skip-ledger.tsv`, one line per reason |
| A2 | per-stream section blocks (R2), enforced | ~~done~~ `check-section-numbers.sh` rejects any 3.x number outside every block, with a `--self-test` negative control; `next-section-number.sh` hands each stream its next free number |
| A3 | archive closed sections out of `IMPROVEMENT-PLAN.md` (R3) | ~~done~~ 139 of 157 sections moved to `IMPROVEMENT-PLAN-CLOSED.md`; plan 16,236 → 3,179 lines. Every number keeps a stub, so all citations still resolve (checked in Python, 0 unresolvable). Re-run `scripts/archive-closed-sections.py` as items close. |
| A4 | retire `PARALLEL-WORKSTREAMS.md`, `WS2-STATUS.md`, `WS3-STATUS.md` into this file (R5) | ~~done~~ all three deleted; the reference half is the last section of this file, and the 19 live citations to them across `CLAUDE.md`, `scripts/`, `.github/workflows/`, `engine/`, `cmd/` and `IMPROVEMENT-PLAN.md` were repointed in the same PR. Both other streams agreed unreserved. |

A1 and A2 first: they are what let A3 and the other streams proceed without conflicts.

### WS-2 — close the Python gap, then the frontier it blocks

| | task | done when |
|---|---|---|
| B1 | §3.113 — Python SDK binds and checks host results | ~~done~~ shipped as §3.201 (`e4de0a4`), wider than §3.113 measured: `SetState`/`DeleteState` were discarding a non-determinism report too. A refused `signal_workflow` raises. §3.113's marker was left 🔴 until #714 closed it. |
| B2 | the seven "start something" calls of §3.111, now unblocked by B1 | each guarded host-side with its guest half in all five SDKs |

B1 was a live defect, not only a prerequisite: a Python workflow whose signal was refused by the
auth check was told nothing. **B1 is done** (§3.201, §3.202). **B2 is four of seven** — the scalar
calls are guarded (§3.302 and earlier), the three string-returning ones are not, and cannot be
without a WIT change. Measured 2026-09-04 on `develop`:

    for f in SignalWorkflow SendSignalAndWait DurableSend SideEffect AcquireLock \
             ScheduleCron DurableScheduleInvoke; do
      echo -n "$f "; sed -n "/func (s \*execSession) $f(/,/^}/p" engine/*.go \
        | grep -c callSuspendSentinel
    done
    # 19:55 -- 1 0 1 0 1 0 1, the zeros being SendSignalAndWait, SideEffect, ScheduleCron
    # 20:30 -- 1 1 1 1 1 1 1, after §3.300 (1d70483) closed exactly those three

A `string` return has nowhere to put the sentinel, so those three needed
`result<string, call-failure>` in `python-sdk/wit/cleat.wit` first — §3.110's situation. **WS-2
did it as §3.300 at 20:14 on 2026-09-04, so B2 is now seven of seven and both rows are done.** The
19:55 reading above is kept because it was accurate when taken; see §3.113's closing note on why a
dated remainder rots where a dated measurement does not.

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
| **WS-1** | `postgres:postgres@localhost:5432` | `root:cleat@tcp(127.0.0.1:3306)` | `1433` — **broken**, below |
| **WS-2** | `cleat:cleat@localhost:5433` | `root:cleat@tcp(127.0.0.1:3307)` | `1434` — connects, migrations fail |
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

### SQL Server: only one stream can run it locally

Every part of this has cost a session at least once.

| port | server | version | state |
|---|---|---|---|
| 1433 | `colima-cleat-ws1/cleat-ws1-mssql` | SQL Server 2022 image | **no TDS handshake** |
| 1434 | `colima/cleat-ws2-mssql` | Azure SQL Edge 15.0 | connects; migrations fail |
| 1435 | `colima-cleat-ws3/cleat-ws3-mssql` | SQL Server 2022 16.0 | works |

**1433 reports `Up 4 weeks` and is not serving.** The port listens, so a connection is accepted
and then dropped — four different encryption settings all return `EOF` — while `docker logs`
shows `Error: 17300 … Failed to start system task` repeating in real time. A container's `Up` is not a
statement about the database inside it.

**1434 cannot run this repo's migrations.** `migrations/mssql/011_json_scalar_payloads.sql` uses
the two-argument `ISJSON`, introduced in SQL Server 2022; Azure SQL Edge is 15.0, so all 540
failures come from that one file. CI covers MSSQL, so this bounds local work only — but it means a
"passes on three dialects" claim made from that sandbox is not one.

**SQL Server needs its own VM**: it cannot start under QEMU (`Invalid mapping of address …`), and
only the `cleat-ws3` colima profile has Rosetta. Manage it with an explicit
`docker --context colima-cleat-ws3`, and note that **`colima start` rewrites the global docker
context** — set it back with `docker context use colima` or every other stream's bare `docker`
silently retargets.

**A container name is not unique across docker contexts, and `docker ps` in one context will lie
to you about a port another context owns.** Two containers named `cleat-ws1-mssql` exist in
different colima VMs, and *two different containers both publish host port 1435* — the default
context's `cleat-ws1-mssql` and `colima-cleat-ws3`'s `cleat-ws3-mssql`. Only one can hold it, bind
order decides, and nothing records which won. Today it is WS-3's.

That ambiguity is not cosmetic. `engine/testutil`'s `CleanupMSSQLTestData`
(`engine/testutil/mssql_schema.go:118`) is an unqualified `DELETE FROM` across **15 tables**. If
the default VM restarts and takes 1435, WS-3's suite silently starts wiping WS-1's server — and
`-p 1` cannot help, because it serialises packages inside one `go test`, not streams across VMs.

**So probe the port; do not read the table.** `docker ps` answers a different question:

    docker context ls                       # there are five; a bare `docker ps` sees one
    docker --context colima-cleat-ws3 ps -a
    # then ask the server which server it is:
    SELECT @@SERVERNAME, SERVERPROPERTY('ProductVersion')

`@@SERVERNAME` is the container ID, so it distinguishes two instances that share a name and a port.
That is what settled the 1435 question: it returned `1a4890c33e6e`, WS-3's container.

### Shared files, and the protocol for each

| file | protocol |
|---|---|
| `IMPROVEMENT-PLAN.md` | Edit only your own `§` sections. **Do not pick a number — run `scripts/next-section-number.sh`.** Blocks are per stream (WS-1 `3.200–299`, WS-2 `3.300–399`, WS-3 `3.400–499`; `scripts/section-blocks.sh`), and `.githooks/pre-commit` refuses a commit that adds one outside yours. Closed sections are archived by `scripts/archive-closed-sections.py`, never deleted. |
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
