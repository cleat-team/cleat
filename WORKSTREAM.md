# The workstream — one at a time

Written 2026-08-06 during a GitHub Actions outage. **Supersedes `PARALLEL-WORKSTREAMS.md`**
(round 2, three concurrent streams).

> **"Kept only as history" was wrong, corrected 2026-09-02.** The *concurrency* claim this file
> makes has held; the claim that the other file is inert has not. Four things in
> `PARALLEL-WORKSTREAMS.md` are load-bearing today and are recorded nowhere else, so a reader who
> takes "history" literally loses them. See "What survived the supersession" below.

**The concurrency claim held, and here is the measurement.** Workstream labels on
`IMPROVEMENT-PLAN.md` section headings, by month, re-derivable with

    grep -oE '\(WS-[0-9], (written )?2026-[0-9]{2}-[0-9]{2}' IMPROVEMENT-PLAN.md \
      | sed -E 's/\(WS-([0-9]), (written )?([0-9]{4}-[0-9]{2})-.*/WS-\1 \3/' | sort | uniq -c

| | 2026-08 | 2026-09 |
|---|---|---|
| WS-1 | 11 | 9 |
| WS-2 | 8 | 12 |
| WS-3 | 9 | 21 |

**Re-measured 2026-09-03, and the conclusion this table used to carry is now false.** It read
`WS-1 12/0`, `WS-2 8/0`, `WS-3 10/11` when measured on 2026-09-02, and the sentence under it said
*"One stream has been writing since 2026-09-01, which is what this file asked for."* Every cell has
moved, two of them from zero, and all three streams wrote in September. The file's own
re-derivation command is what showed it.

So the concurrency claim in this file's title — *one at a time* — **does not describe how this
project is being worked today**, whatever it recommends. That is a statement about the measurement,
not an argument for or against the practice; what three concurrent streams cost is recorded in
§3.100 and in `scripts/skip-budget.txt`'s history, and it is not nothing.

**The August figures moved too, by one each for WS-1 and WS-3, and that is worth not
hand-waving.** A count of *past* months should be stable. It is not, so either a section's label
or date was edited after the fact, or a section was removed — §3.95's `restore-workflow` entry was
rewritten on 2026-09-03 and one command was deleted outright. Whichever it was, it means this
table is a measurement of the file's current text and not a ledger of what happened, and it should
be read that way.

**The labels did not stop, and that remains the source of the confusion the note below
resolves:** work is stamped `WS-1`/`WS-2`/`WS-3` because the *ownership map* is in force. The
difference from 2026-09-02 is that the stamps now also reflect three streams actually running.

> **What this table cannot tell you, added 2026-09-02 after it nearly caused the opposite
> error.** It counts *authorship of new plan sections*. That cannot distinguish **"no stream is
> running"** from **"a stream is running and its board is empty"** — and on the day it was
> written the second was the case: #564 re-derived the WS-1 and WS-2 boards and found every item
> on both already closed.
>
> The conclusion above survives, because #564 checked it independently and adopted it. The
> *inference* did not: it happened to be right. **Both boards now carry real items again**
> (§3.12, §3.15, §2.60d, §3.38 for WS-1; the §3.35 phase-5 record shape, §2.35, §1.4 phase F for
> WS-2), so from here a silent stream means idle, not empty, and this table would read the same
> either way. Re-derive from the boards, not from this count.
>
> **That item list is now historical, checked 2026-09-03.** §3.12 and §3.15 have closed, and
> §2.60d no longer matches a section heading at all. §3.38 is still open and still
> `OBSERVED, not reproduced`. The instruction above survives; the examples did not, which is the
> ordinary way a list of *current* items ages.
>
> Worth adding because it is the case this paragraph warns about, live: **WS-1's board was empty
> again on 2026-09-03**, at the end of a day with twelve merges on it. A reader checking this file
> for whether that stream is running would get no signal from the count — which is exactly why the
> instruction is to re-derive from the boards.
>
> **How the near-miss happened is the part worth keeping.** Two sessions, hours apart, each read
> the other's *unmerged* claim and deferred to it — in opposite directions. #563 measured "one
> stream writing" and concluded the project was sequential. #564, in review at the same time,
> asserted "three streams are running again" from three sandboxes existing. Each of us then
> wrote a correction adopting the other's position: #564's second commit says *"resolved in
> #563's favour throughout"*, while #565 was drafted to say #564 was right. Nobody was lying and
> both corrections were sincere; the measured claim was simply stronger than the inferred one,
> and neither of us checked which was which before conceding.
>
> **A claim in an open PR is not evidence.** Re-derive it, or wait for it to land, before
> correcting something you measured.

---

## What survived the supersession

`PARALLEL-WORKSTREAMS.md` is the only place these live. Nothing here replaces them, and this file
did not carry them when it declared that one superseded.

| in `PARALLEL-WORKSTREAMS.md` | status |
|---|---|
| Checkout → stream map (`:17`) — `/localssd/rcownie/cleat-agent2` is **WS-3, "Execution boundaries: what stops a guest that will not stop"** | **live.** It is how a session in a sandbox learns which stream it is. |
| Database ports (`:80`) — WS-3 is Postgres `5434`, MySQL `3308`, SQL Server `1435` | **live.** Used 2026-09-02 for a full three-dialect `go test ./engine/... -p 1` (133s wall; Postgres-only is ~21s, so all three connected). |
| Migration ranges (`:108`) — WS-1 `010–019`, WS-2 `020–029`, WS-3 `030–039` | **live.** §3.35 phase 5 still cites `030–039` as reserved. |
| `IMPROVEMENT-PLAN.md` section allocation (`:105`) — WS-1 `§3.10+`, WS-2 `§3.20+`, WS-3 `§3.30+` | **already breached, and not worth restoring.** See below. |
| "Three concurrent streams", the coordination rituals, the per-stream boards | **retired.** That is what this file supersedes. |

**The section-allocation rule failed and the workaround is the thing to copy.** Measured
2026-09-02 with

    grep -oE '^### (3\.[0-9]+) .*\(WS-[0-9]' IMPROVEMENT-PLAN.md

WS-1 holds **§3.34, §3.37 and §3.38** — all inside WS-3's declared `§3.30+` block — and §3.35 was
allocated to two streams at once (fixed by renumbering WS-1's concurrency item to §3.39). A
prefix reservation did not survive contact, because it needs every writer to remember it every
time.

What did survive is WS-3's response: move to **§3.66+**, above the high-water mark. That is the
same tactic the migration ranges use ("sparse and above the high-water mark so no stream
renumbers another"), and it works for the same reason — a new section that cannot collide beats a
convention that must be remembered. **Take the next free number above the highest in the file
rather than the next one in a reserved block.**

**One more thing that stopped being true, recorded 2026-09-02: Phase A and Phase B below are
complete.** A6 is no longer "blocked on CI" — `.github/workflows/tier1-gate.yml` runs
`scripts/tier-gate.sh`, and `develop` requires **32** status checks with `enforce_admins` on
(`gh api repos/:owner/:repo/branches/develop/protection --jq '{n: (.required_status_checks.contexts|length), admins: .enforce_admins.enabled}'`).
D1–D4, which Phase B lists as the gate on everything after it, were all decided 2026-08-06, and
D5 since; `tiers.yaml`'s own status block says so. Read the phase tables below for their
measurements and their reasoning, not for their status markers.

**One stream, sequential, until the ground rules hold.** Round 2's three streams each closed
their named items, so the split worked — but nearly every operational hazard recorded in the
three status docs was a coordination artifact rather than the work itself: shared files edited
by section, `§3.35` allocated twice, a stash leaking between worktrees, another stream's
migration landing in a third stream's database, one shared GitHub account making
`gh pr list --author @me` useless, and a `develop` run cancelled by the next merge landing
forty seconds later. None of that is inherent. All of it is the cost of three writers.

Infrastructure and ground rules are one theme. Splitting them across streams would recreate
the tax to do work that does not need it.

---

## The goal

Three states, and the point is to be able to *say which is which* and have it be true:

1. **Core that works reliably** — tier 1 in `tiers.yaml`. Must pass, and a skip is a failure.
2. **A frontier with known issues and a plan** — tier 2. Must run; may fail against a list
   that only shrinks.
3. **Aspirational things, parked** — tier 3. Not built, not shipped, not claimed.

The hard part was never the fixing. It is that until today nothing in this repo could tell a
green run that tested everything from a green run that tested nothing.

---

## Measured starting point, 2026-08-06

Re-derive with `scripts/tier-gate.sh --measure`.

| | result |
|---|---|
| `go build ./...`, `go vet ./...` | clean |
| whole test tree compiles | yes, ~70 packages, zero build failures |
| `go test -p 1 ./...` on three dialects | **52 packages pass, 3 fail**, 24 have no tests |
| `go test ./engine/` on three dialects | **3846 tests, 3842 pass, 0 fail, 4 skip**, 60s |
| `go test ./engine/` with no DSN set | 2544 tests, 166 skip, 16s — **and also prints `ok`** |

The three failing packages:

| package | cause | kind |
|---|---|---|
| `examples/dag` | `AwaitAnyChild` called outside a workflow context (§3.14) | **real defect** |
| `tests/exhaustion` | cluster compose stack absent; DSN expects user `cleat` | environmental |
| `tests/plugin-harness` | Java/Gradle toolchain absent | environmental |

**One real code failure in the entire tree.** That is the finding that should reset
expectations: the core is not broken and never was. It was unprovable, which is a different
problem with a different fix — and the fix is a gate, not a bug hunt.

The four engine skips are three missing toolchains (`componentize-py` twice, Java once) and
one known mock gap. Notably, Python and Java cannot be *verified at all* on the macOS dev
machines — `componentize-py` dies on a mach port guard — so Linux CI is their only proof.
That is a tiering input, not a defect.

---

## Phase A — ground rules and instruments

Everything here is CI-independent and can be done during the outage.

| | status |
|---|---|
| **A1** `CLAUDE.md` rewritten: durable rules separated from volatile state; false-green catalogue; falsification, fix-the-prose, and dated-claims rules; stale "engine does not compile" note removed | **done** |
| **A2** `tiers.yaml` drafted, grounded in the measurement above | **done — needs D1–D4** |
| **A3** `scripts/tier-gate.sh` — enforces tier 1, refuses to run when a DSN is unset or CGO is off, treats a skip as a failure | **done, falsified three ways** |
| **A4** Fix `IMPROVEMENT-PLAN.md`'s four record defects (below) | **done** |
| **A5** Delete resolved branch refs | **done — 81 refs → 13** |
| **A6** Wire `tier-gate.sh` into CI as a required check | **blocked on CI** |
| **A7** `scripts/docker/python-toolchain.Dockerfile` — run `componentize-py` + `wasm-tools` on a Mac | **done** |

### A4 — the plan's four record defects, all measured

- **§3.20 has no heading.** Its writeup starts mid-paragraph at line 5640, *inside* §3.33's
  body, so nothing finds it by section number. WS-2's status doc points readers at "§3.20" and
  they will not find it.
- **§3.35 was allocated twice** — WS-3's defer design and WS-1's concurrency-key item. **Fixed 2026-08-06:** the concurrency item is now §3.39.
- **§1.6 is written as planned work and shipped long ago.** All three dialects bump
  `generation` on both reap and terminate (`db.go:1086`, `mysql_ops.go:1177`,
  `mssql_operations.go:192`, plus the reap paths). The heading never got a marker.
- **§1.5/§2.28's body contradicts its own heading.** Lines 4234 and 4450 still describe
  `engine/component_cgo.go` as sitting behind the `wasmtime_component_cgo` build tag. Measured:
  that file carries only `//go:build cgo`, the tag exists nowhere outside past-tense narrative,
  and `engine/engine.go:341` lists python in `WasmtimeLanguages`. **This one has now cost four
  sessions**, the fourth being a survey agent today that read those lines and reported a
  working feature as broken.

### A5 — the branch backlog, measured

81 remote refs. By PR state: **46 MERGED, 16 CLOSED, 9 OPEN, 10 with no PR.**

Merging any of these ten into `develop` provably changes nothing — they are pure debris:

```
docs/ws3-errcheck-triage              fix/ws3-guest-pointer-bounds
fix/ws3-cancellation-reason-length    fix/ws3-loopctx-data-race
fix/ws3-defers-on-fenced-backend      fix/ws3-python-build-wasm-tools
fix/ws3-embedded-defers-never-ran     fix/ws3-scratch-base-overflow
fix/ws3-failover-comment-stale        fix/ws3-signal-payload-length
```

The other 36 MERGED refs are also safe in principle — GitHub merged their PRs — but
`git merge-tree` no longer reports them as no-ops because `develop` has moved on by up to 330
commits, so the equivalence cannot be re-proved locally. **Deleting on PR state alone is a
judgement call and needs your sign-off.** The 16 CLOSED ones are rejected work; deleting those
discards it.

Note for whoever updates `BRANCH-TRIAGE.md`: its method (`git rev-list --left-right --count`)
cannot see through a squash-merge, and it reported already-merged branches as outstanding —
including the two it recommended taking first. Re-derive with `gh pr list` state, not commit
counts.

---

## Phase B — make tier 1 real

Starts the moment Actions dispatches again.

1. **Diagnose the dispatch gap.** No run repo-wide since `2026-08-06T03:51:18Z`; Actions is
   `enabled`, nothing queued or in progress. A clean cutoff at one timestamp is the shape of
   quota exhaustion — the org billing endpoint needs `admin:org` scope, which the working token
   lacks, so this needs the account owner.
2. **Land the standing queue:** #345, then #333 (stacked on it), then #346. Check and probably
   close #293 — its content appears to be on `develop` already.
3. **Fix the dependabot CLA check once.** All five dependabot PRs fail `Contributor License
   Agreement`; that is a bot-exemption gap, not flake, and re-running will never clear it.
4. **Wire `tier-gate.sh` as a required check**, and run one deliberate `develop` verification
   of the *current head* — not of any individual SHA, because a merge's own run can be
   cancelled by the next merge.
5. **Answer D1–D4 in `tiers.yaml`.** The gate enforces whatever they say, so they gate
   everything after this.

Expect tier 1 to come back redder in CI than it is locally: CI runs dialect matrices, the
cluster compose stack and toolchains this machine does not have. **Every red item is then a
two-way decision — fix it, or demote it to tier 2 with an entry in `known_failures`.** That
demote option is what makes this converge in days rather than weeks; the schedule does not
depend on fixing everything, only on ceasing to claim everything.

---

## Phase C — the frontier, in priority order

Only after tier 1 is enforced and green. Full item detail is in `IMPROVEMENT-PLAN.md`;
this is the order.

1. **`examples/dag` (§3.14)** — the one real failure in the tree. Fix or park; do not leave a
   red package sitting in a repo whose whole problem has been false green.
2. **Plugin secrets** — Slack webhook URLs, PagerDuty routing keys, webhook HMAC secrets and
   OAuth tokens are reported as readable in plaintext through `GET` endpoints, and the OAuth
   plugin stores a plaintext session token beside its own hash. **Verify each site before
   writing code** — this is reported, not measured.
3. **`kafkaconnect.produce()`** returns `Success: true` without publishing when unconfigured.
4. **The vacuous multi-DB harness job** — three databases start to run a `t.Skip`. Ask why the
   harness is on wazero at all before fixing the panic.
5. **§3.24 / §3.23 / §2.35** — error classification. Cheap, no ABI change, and the
   operator-facing payoff for everything §1.4 built across two rounds.
6. **G115 in the ABI layer (§3.33)** — ~200 sites, four real defects so far and not one of them
   an overflow. Treat as an index of places where a length was converted, and ask whether the
   answer is a property test over that boundary rather than 200 readings.
7. **Test isolation (§2.60d)** — the reason a green run on a reused database means little.

**Blocked on you, not on the stream:** MySQL RLS (§1.7), `workflow_defs` namespacing (§3.12),
concurrency-key re-entrancy (§3.39), the six defer-design decisions (§3.35), and
positioning (Phase 4). Each was escalated in round 2 rather than guessed at, and each is still
unanswered.

> **Re-derived 2026-09-02: three of the five were answered and one was never yours.**
>
> | | state |
> |---|---|
> | MySQL RLS (§1.7) | **Answered 2026-08-06 by D1** — MySQL is single-tenant only, a documented product boundary. README and `docs/reference/multi-tenancy.md` already say so. |
> | concurrency-key re-entrancy (§3.39) | **Answered and fixed 2026-08-31.** |
> | the defer-design decisions (§3.35) | **Answered; phases 1–4 shipped**, the last on 2026-09-02. Phase 5 is open but is *not* blocked on you — it is blocked on WS-2 agreeing a durable-record shape with WS-3, which is a cross-stream question the two streams can settle. |
> | `workflow_defs` namespacing (§3.12) | **Answered 2026-09-02 by D7, and implemented 2026-09-03.** Per-tenant: *"it doesn't make any sense for one tenant to need to worry about clashes with some other tenant's workflows."* It turned out to be three tables, not one, and all three shipped (#594, #601, #605). Note this row is itself the lesson below in miniature -- it said "still yours, and now the only one" for a day after `tiers.yaml` recorded D7. |
> | positioning (Phase 4) | **Still yours.** |
>
> The general lesson, since this list is the third place it has bitten: an escalation is only
> unanswered until someone answers it, and the answer lands in `tiers.yaml` or a `§` body
> rather than back in the file that asked. **Check `tiers.yaml` for a `decision:` before
> re-escalating anything on a list like this.** Three of these five sat here as blocking
> questions for up to 26 days after they were settled.

---

## Environment

This sandbox, `/localssd/rcownie/cleat`. The other two are idle for now; do not run against
their databases.

```
CLEAT_TEST_POSTGRES='postgres://postgres:postgres@localhost:5432/cleat?sslmode=disable'
CLEAT_TEST_MYSQL='root:cleat@tcp(127.0.0.1:3306)/cleat?tls=false&parseTime=true&multiStatements=true'
CLEAT_TEST_MSSQL='sqlserver://sa:CleatTest123!@localhost:1433?database=cleat'
```

SQL Server runs in a separate colima profile with Rosetta — manage it with
`docker --context colima-cleat-ws1`, and note that `colima start` rewrites the global docker
context, so set it back with `docker context use colima` afterwards.

`docker compose down -v` destroys the user's database — the `cleat` project owns
`cleat-postgres-1`. Remove containers by name.
