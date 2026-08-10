# The workstream — one at a time

Written 2026-08-06 during a GitHub Actions outage. **Supersedes `PARALLEL-WORKSTREAMS.md`**
(round 2, three concurrent streams), which is kept only as history.

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
