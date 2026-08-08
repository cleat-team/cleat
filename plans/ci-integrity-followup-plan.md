# CI Integrity — what is left after the 2026-08-07 sweep

**Date:** 2026-08-07
**Follows:** #363–#378, which found and fixed seven checks that were green and measured nothing.
**Method:** every claim below carries the command that re-derives it. Nothing here is estimated.

---

## What the sweep established

Seven required or advertised checks were producing `success` while asserting nothing:

| PR | Check | Why it measured nothing |
|----|-------|-------------------------|
| #363 | `DCO Check` | required, and could never fail |
| #364 | `CLA Check` | recorded nothing durable |
| #366 | Pull Request Labeler | v4 config under a v5 action |
| #367 | `Lint` / `lint-go` / `Vulnerability Check` | analysed a CGO-off build, missing 8 wasmtime files |
| #370 | AssemblyScript jobs | `continue-on-error` while every step passed |
| #373 | Release Notes Check | keyed on labels nothing could apply — four reasons, any one sufficient |
| #378 | AI PR Review | posted "AI review unavailable" on 29 of 29 runs |

Plus: tier 2 had never run at all (#369), every SDK test job existed and none gated (#370, #371), and
one required check ran on a `:latest` tag (#377).

**The pattern, stated once so the rest of this document can refer to it.** In six of the seven cases
the check *ran*, *reported success*, and in two cases *posted output*. Every signal a reader would use
to judge it said it was working. The only way any of them was found was by asking what the check would
do if the thing it guards were broken — and then breaking it.

The corollary is the organising principle for everything below: **a check is not evidence until
someone has watched it fail.**

---

## The items

Ordered by what would hurt most if it stayed as it is.

---

### 1. There is no security scanning, and three documents say there is

**Severity: high.** This is the only item here that is a live false claim rather than a gap.

Measured 2026-08-07:

```
$ gh api repos/cleat-team/cleat/code-scanning/default-setup --jq '{state}'
{"state":"not-configured"}

$ gh api repos/cleat-team/cleat/code-scanning/alerts
gh: no analysis found (HTTP 404)

$ gh api repos/cleat-team/cleat --jq '.security_and_analysis'
{"dependabot_security_updates":{"status":"disabled"},
 "secret_scanning":{"status":"disabled"},
 "secret_scanning_push_protection":{"status":"disabled"}}

$ ls .github/workflows/codeql.yml
ls: no such file
```

So: no CodeQL workflow, CodeQL default setup not configured, no analysis has ever run, secret scanning
off, push protection off, Dependabot security updates off.

What the repo says instead, in `plans/public-launch-polish-plan.md`:

- line 33 — `| **CI/CD** | CodeQL scanning (codeql.yml) | Done |`
- line 35 — `| **CI/CD** | Semantic PR title check (semantic-pull-request.yml) | Done |`
- line 176 — *"CI/CD: 9 workflows including CodeQL, DCO, semantic PRs, release notes check, ecosystem
  CI — competitive with Biome (23) and Astro (20)"*
- line 18 — *"12 badges (CI, Go version, license, Go Report Card, Codecov, OpenSSF Scorecard,
  govulncheck, Discord, Go Reference, etc.)"*

Of the five workflows named on line 176, three do not exist and one never worked. `README.md` carries
six badges, and none of them is Codecov, OpenSSF Scorecard or govulncheck:

```
grep -c '^\[!\[' README.md          # 6
ls .github/workflows/ | wc -l       # 14
```

**Why this one is first.** Being unscanned is a gap. Telling a prospective contributor — or a security
researcher reading `SECURITY.md` before deciding whether to report privately — that the project runs
CodeQL when it has never run any analysis is a different kind of problem, and it is the exact failure
`CLAUDE.md` opens the tiers section with: *do not claim support in a doc that the manifest does not
grant.*

**Do:**

1. Enable CodeQL default setup, secret scanning, and secret-scanning push protection. Repo settings,
   minutes, no code change. Default setup rather than a `codeql.yml` — it is one fewer workflow to rot,
   and this repo's evidence on hand-written workflows is not encouraging.
2. Expect an initial alert backlog. Triage it before making the check required; requiring it first
   would block every PR on unrelated findings, which is how a security gate gets disabled.
3. Correct the four lines above. `govulncheck` genuinely runs (`Vulnerability Check`, required, and
   fixed in #367 — it had been analysing a CGO-off build); say that and drop the rest.
4. Decide on the semantic-PR-title check separately. It is absent, and `Validate branch name` already
   enforces a prefix vocabulary, so this may be a row to delete rather than a workflow to write.

**Verify:** `gh api .../code-scanning/default-setup --jq .state` reports `configured`, and
`gh api .../code-scanning/alerts` returns a list rather than 404.

---

### 2. `wasm-demo` has not compiled since 2026-08-02 and nothing builds it

**Severity: medium-high** — the cost is that it is invisible, not that it is broken.

```
$ go build ./...            # clean, exit 0
$ cd wasm-demo && go build ./...
# cleat-wasm-demo/worker
worker/versioned_loader.go:37:6:  WorkflowInstance redeclared in this block
        worker/failover_worker.go:42:6: other declaration of WorkflowInstance
worker/versioned_loader.go:201:6: Worker redeclared in this block
        worker/failover_worker.go:262:6: other declaration of Worker
worker/versioned_loader.go:208:17: unknown field ID in struct literal of type Worker, but does have id
... 7 errors
```

The root build is clean **because `wasm-demo/` is a separate module** (`module cleat-wasm-demo`), so
`./...` never reaches it:

```
go list ./... | grep -c wasm-demo    # 0
```

Broken since `c26c332` (2026-08-02). `tiers.yaml` has a `modules:` list precisely so a separate module
gets built — it names `cleat/` and not this one.

**Do:** decide first, then wire. Three defensible answers and no way to pick from the code alone:

- **Fix it** — the duplicate declarations look like an incomplete refactor, not a design problem — then
  add it to `tiers.yaml`'s `modules:` so the tier gate compiles it.
- **Delete it** — if the demo has been superseded by `examples/`, six days of nobody noticing is
  evidence.
- **Tier 3 it** — explicitly parked, excluded from the build, and *say so*, per the tier-3 contract.

The one unacceptable option is the current state, which is tier 3 in effect and undeclared.

**Verify:** whichever is chosen, `go build ./...` inside every module named in `tiers.yaml` is green in
CI, and the module list matches what exists on disk.

---

### 3. `mcr.microsoft.com/mssql/server:2022-latest` is a moving tag on three required checks

**Severity: medium.** Same class as the MinIO `:latest` fixed in #377, and larger.

```
$ grep -rn 'image:' .github/workflows/ | sed 's/.*image: //' | sort | uniq -c
   8 postgres:16
   5 mysql:8.4
   5 mcr.microsoft.com/mssql/server:2022-latest
   1 quay.io/minio/minio@sha256:14cea493...
```

`2022-latest` moves. Its five uses include `Test SQL Server` (required since 2026-08-07), `Tier 1 Gate`
and `Tier 2 Gate` — a Microsoft republish can turn every PR in the repo red, on a schedule unrelated to
anything the project did, and the failure lands on whichever PR happens to be open.

`postgres:16` and `mysql:8.4` float within a minor series. Lower risk, same class.

**Do:** pin `2022-latest` by digest, exactly as #377 pinned MinIO — take the digest it currently
resolves to, so the pin is a no-op on the day it lands and a guarantee afterwards. Renovate
(`config:recommended`, github-actions manager) then proposes bumps as reviewable PRs that must go green.

Treat `postgres:16` and `mysql:8.4` as a follow-on, not a blocker.

**Verify:** the pinned digest pulls and reports the expected release, and the three required checks are
green on it before merge. Watch for one thing #377 did not have to: five call sites must move together,
and a partial pin is worse than none because it hides which one drifted.

---

### 4. Two skips that are failures wearing a skip's clothing

`CLAUDE.md`: *a skip that hides a crash is not a skip.* Two remain, and `scripts/skip-budget.txt`
already records both as deliberate baselines with instructions to zero them:

```
$ grep -rn 't.Skip' --include='*_test.go' tests/ | grep -iE 'crash|panic|compat'
tests/plugin-harness/multi_db_plugin_test.go:18: skipping: wazero v1.11.1 nil Sys context panic
tests/plugin-harness/wasm_plugin_test.go:417:    wasmtime-go compatibility issue with this WASM module
tests/plugin-harness/wasm_plugin_test.go:501:    AS WASM runtime trap — likely AS/transform incompatibility
tests/plugin-harness/wasm_plugin_test.go:652:    wasmtime-go compatibility issue with Java/TeaVM modules
tests/plugin-harness/wasm_plugin_test.go:660:    Java module crashed (wasmtime-go compat)
```

**4a. `TestPluginCalls_MultiDB` skips unconditionally.** Budget `plugin-harness/multi-db 1`, whose own
comment says *"that job starts PostgreSQL, MySQL and SQL Server and then tests nothing at all… Drop it
to 0 when the skip is removed."* It is the only test under `./tests/...` touching MySQL or SQL Server.
The stated cause is a wazero v1.11.1 panic — and `plugin-harness-ci.yml` now runs CGO-on throughout,
which is what removed the sibling skip in `TestPluginCalls_Wasm_Go`. **Check whether this skip is still
true before doing anything else**; it may already be stale, in which case the fix is deleting a line.

**4b. `TestTenantIsolationOverHTTP_MSSQL` skips unconditionally,** blocked on §2.71 (`SESSION_CONTEXT`
cleared by `sp_reset_connection` on a pooled connection). Budget `test-go/commands 6`. Correctly
recorded as the acceptance test for §2.71 rather than deleted — leave it until §2.71 lands, then drop
the budget.

**4c. The four `wasm_plugin_test.go` compat skips are conditional and already neutralised**, because
`plugin-harness/wasm` has a budget of 0 — any of them firing turns the job red. That is the right
outcome reached by an indirect route, and it is fragile: the skip *text* still says "compatibility
issue", so the next reader sees a skip and a budget failure and has to work out which is lying.
Convert them to `t.Fatalf` with the same message. No behaviour change under the current budget; it
stops the budget being the only thing standing between a crash and a green.

**Verify (4a):** remove the skip, run `./tests/plugin-harness/... -run TestPluginCalls_MultiDB` with
CGO on and all three DSNs. If it passes, drop the budget to 0 in the same PR — the budget is what stops
it regressing.

---

### 5. `tests/exhaustion` cannot be gated

`tiers.yaml` records this as the blocker keeping the suite ungated. Two separate defects:

```
$ grep -n 'postgres://' tests/exhaustion/exhaustion_test.go
43:  dsn = "postgres://cleat:cleat@localhost:5432/cleat?sslmode=disable"
```

- It hard-fails rather than skipping when the cluster compose stack is absent — so it cannot run on a
  developer machine or in a job that does not provision the stack.
- The DSN is hardcoded and ignores `CLEAT_TEST_DB` / `CLEAT_TEST_POSTGRES`, so it is unaffected by the
  configuration every other suite reads.

**Do:** read the DSN from the standard variables with the current string as the default, and apply the
distinction `testutil.TestDB` already makes (§1.13): **no DSN configured → skip; DSN configured and
unreachable → fail, naming it.** That is the only shape that is both runnable locally and safe to gate.
Then wire it into a job and give it a skip budget.

**Verify:** with no DSN set it skips; with an unreachable DSN set it fails and prints the DSN
(redacted). Both, in that order — the second is the one that has been getting skipped past.

---

### 6. `pluginapi` is the external contract and has no tests

```
$ go list -f '{{if and (eq (len .TestGoFiles) 0) (eq (len .XTestGoFiles) 0)}}{{.ImportPath}}{{end}}' ./... \
    | sed 's|github.com/cleat-team/cleat/||'
```

23 packages, of which most are `examples/*` and are fine. Three are not:

- **`pluginapi`** — the public re-export surface for external plugin authors. Nothing detects a
  re-export that stops resolving, is renamed, or silently changes type. This is the highest-value
  entry on the list because it is the one with users outside the repo.
- **`wasmrw`** — WASM read/write helpers. `CLAUDE.md` notes production code duplicates this inline,
  which makes divergence between the two copies the interesting risk and a property test the natural
  shape.
- **`monitoring/prometheus`** — metric names and label cardinality are an API that dashboards depend on.

`cmd/cleat-plugin-verify`, `cmd/deploy-workflow`, `cmd/wit-rewrite` also have none. Lower value.

**Do:** `pluginapi` first, and the test that pays for itself is a compile-time assertion that every
re-exported symbol still resolves to the type it claims — that is the failure an external author sees
and the maintainer does not.

---

### 7. §2.60d — `CleanupPostgresTestData`, and the `-p 1` tax

Open in `IMPROVEMENT-PLAN.md`. An unqualified `DELETE FROM` across eleven tables, which is why every
multi-package database run in this repo needs `-p 1`, which is why `Tier 1 Gate` takes ~11 minutes and
is reliably the last check to finish on every PR.

It stays open because the fix is a design decision across ~40 call sites — database-per-package,
tenant-per-package, or per-test transaction rollback — not a mechanical change. **It is listed here for
its cost, not because it is ready.** Costing every PR ~11 minutes of serialised wall clock is the
argument for picking one; picking one is not something to do in passing.

---

### 8. The mechanism the sweep is missing

`CLAUDE.md` asks whether the answer is a sweep or a mechanism. Seven inert checks were found by hand,
one at a time, over one session. There is no reason to think that process was exhaustive, and every
one of them predated the session by months.

Two guards already exist and work — the pattern to extend, not invent:

- `Lint`'s **"Workflow files parse"** step, added after §1.12, walks every file under
  `.github/workflows/` and fails on one that does not load or has no `jobs:` key.
- `scripts/tier2-gate.sh` asserts that **every `job:` named in `tiers.yaml` still exists** in some
  workflow file — the anti-rot check that stops a renamed job silently dropping its suite.

Three cheap extensions in the same spirit, each catching a class rather than an instance:

1. **Every required status context resolves to a job that exists.** A required context naming a job no
   longer defined blocks every PR forever; a job renamed out from under a context stops gating and
   nothing says so. Compare `gh api .../required_status_checks --jq '.contexts[]'` against the `name:`
   fields across all workflow files.
2. **No `continue-on-error` on a job whose context is required.** This is #370 and #373's shape
   directly: a required check that cannot report failure. Currently zero — worth keeping there.
3. **No floating tag in a `services:` image.** #377's shape. `:latest` and `-latest` are the cases seen
   so far.

Each is a few lines in the existing `Lint` job, and each has already been paid for once in a session
spent finding an instance by hand.

**Test each guard by breaking the thing it guards** before trusting it. That rule is what this whole
document is about, and a guard is not exempt from it.

---

## Sequence

Items are independent. Recommended order by cost-to-benefit:

| # | Item | Effort | Why here |
|---|------|--------|----------|
| 1 | Enable CodeQL + secret scanning; correct four doc lines | ~1h + triage | Only live false claim; settings change, no code |
| 4a | Check whether `TestPluginCalls_MultiDB`'s skip is still true | ~30m | May already be stale — cheapest possible win |
| 3 | Pin `mssql:2022-latest` by digest | ~1h | Mechanical; removes a third-party trigger from three required checks |
| 8 | Three `Lint` guards | ~2h | Turns this document's findings into checks |
| 2 | Decide `wasm-demo`: fix, delete, or park | ~1h to decide | Needs a call before any work is meaningful |
| 5 | `tests/exhaustion` DSN + skip/fail split | ~2h | Unblocks gating a whole suite |
| 6 | `pluginapi` re-export assertions | ~2h | Only untested package with external users |
| 4c | `wasm_plugin_test.go` compat skips → `t.Fatalf` | ~30m | Removes a lie; no behaviour change |
| 4b | `TestTenantIsolationOverHTTP_MSSQL` | — | Blocked on §2.71; leave |
| 7 | §2.60d `CleanupPostgresTestData` | days | Design decision; costs ~11 min/PR until then |

---

## The rule this document exists to enforce

Everything above is one of two shapes:

- **A claim nothing checks** — items 1, 2, and the doc half of 3.
- **A check that cannot fail** — items 4, 5, and 8.

Both are the same failure seen from either end, and the repair for both is the same: make the thing
fail on purpose, and watch it. Not "it went red" — red *for the expected reason*. Twice in this repo a
test has passed for the wrong reason, and once a fence test passed with its SQL guard deleted because a
Go-level rollback covered for it.

Any number in this file carries the command above it. If a command stops reproducing its number, the
number is wrong and this file is what needs correcting — not the command.
