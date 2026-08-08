# CI Integrity — what is left after the 2026-08-07 sweep

**Date:** 2026-08-07. **Outcomes recorded in place the same day, and corrected on 2026-08-08.**
**Follows:** #363–#378, which found and fixed seven checks that were green and measured nothing.
**Method:** every claim below carries the command that re-derives it. Nothing here is estimated.

**Status:** items 1, 2, 3, 4a, 4c, 5, 6 and 8 are done — see [Sequence](#sequence--as-executed)
for the PR against each. 4b stays blocked on §2.71 and 7 was not started, both deliberately. Each item
heading below carries its outcome, and the original finding is folded under it rather than overwritten,
because the evidence is what made the case. **Four of this document's own claims turned out to be
wrong** — noted at the item that carried each, and summarised in Sequence. The fourth was written into
this file while recording the outcomes and was wrong the next morning, which is the sharpest thing this
document has to say about itself: writing an outcome down is not checking it.

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

### 1. ~~There is no security scanning, and three documents say there is~~ — DONE 2026-08-07

**Severity: high.** This was the only item here that was a live false claim rather than a gap.

**What was enabled**, later the same day this was written:

```
$ gh api repos/cleat-team/cleat/code-scanning/default-setup --jq '{state,languages}'
{"state":"configured",
 "languages":["actions","go","javascript","javascript-typescript","python","typescript"]}

$ gh api repos/cleat-team/cleat --jq '.security_and_analysis'
secret_scanning: enabled, secret_scanning_push_protection: enabled,
secret_scanning_non_provider_patterns: enabled
```

CodeQL **default setup**, not a `codeql.yml`, for the reason given under "Do" below. Four languages
were requested — `actions`, `go`, `javascript-typescript`, `python` — and GitHub stores six, because
it expands `javascript-typescript` into its `javascript` and `typescript` aliases. The command above
prints what is stored, which is why it does not match what was asked for.

`c-cpp`, `java-kotlin` and `rust` were deliberately left out: they need autobuild, the `C` GitHub
attributes to this repo is vendored, and a scanner that fails to build is a scanner that reports
nothing — which is this document's entire subject.

**What it immediately found, and the reason step 2 below was the important one.** Turning on
Dependabot alerts surfaced **68 open vulnerabilities on the default branch — 16 critical, 25 high,
26 medium, 1 low** — and opened 8 update PRs. None of that was new; nothing had been reporting it.
(The API says `medium` where the web UI says `moderate`; the command below prints the API's word.)
Re-derive:

```
gh api repos/cleat-team/cleat/dependabot/alerts --paginate \
  --jq 'map(select(.state=="open"))|group_by(.security_advisory.severity)|map({(.[0].security_advisory.severity):length})|add'
```

**Still open, and deliberately so:** none of this is a required check yet. Triage first. Requiring a
scanner before its backlog is triaged blocks every PR on unrelated findings, which is how a security
gate gets switched off. That was step 2 when this was written and it is still step 2.

The doc corrections (step 3) landed alongside; the semantic-PR-title decision (step 4) is recorded
in `plans/public-launch-polish-plan.md` — not doing it, because `Validate branch name` already
enforces the same convention and is required.

---

<details>
<summary>The original finding, kept because the evidence is what made the case</summary>

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

</details>

---

### 2. ~~`wasm-demo` has not compiled since 2026-08-02 and nothing builds it~~ — DELETED in #391 (`dadf574`)

**The heading's date is wrong, and the error it led to is the interesting part.** `wasm-demo/` did not
break on 2026-08-02. It has never compiled, once, since it entered the repository:

```
$ git log --diff-filter=A --format='%h %ad %s' --date=short -- \
    wasm-demo/worker/versioned_loader.go wasm-demo/worker/failover_worker.go
0a1f6af 2026-05-05 initial commit
```

Both halves of the collision arrived in the same commit — the initial one. `c26c332` touched the
directory; it did not break it. That single wrong attribution is what made "the duplicate declarations
look like an incomplete refactor" (below) sound plausible, and it is not: there is no earlier state to
refactor *from*. A module that has never once built is not a regression to repair, it is a directory
nobody ever wired up, which settled the three-way decision immediately. Deleted rather than fixed or
parked; `prompts/versioning_plan.md`, its only referrer, now points at `engine/versioned_loader.go`
and `engine/store_versioning.go` instead.

The general lesson is the one this document already argues in the other direction: **a date is a claim
and needs its command too.** `git log -1 <path>` answers "when was this last touched", which is not the
question, and reads like it is.

---

<details>
<summary>The original finding</summary>

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

</details>

---

### 3. ~~`mcr.microsoft.com/mssql/server:2022-latest` is a moving tag on three required checks~~ — PINNED in #390 (`09322ba`)

Pinned to `mcr.microsoft.com/mssql/server@sha256:ba4c8329f48fb8f02e1416be6a930ebfd71268caee78aa985f3af4315e457c89`,
the digest `2022-latest` resolved to on 2026-08-07, so the pin was a no-op on the day it landed.

**"Five call sites must move together" undercounted, in two directions.** Re-derived on `develop`,
2026-08-07:

```
$ grep -rn 'image: *mcr.microsoft.com/mssql' .github/workflows/ docker-compose*.yml | wc -l
7
$ grep -rn 'ancestor=mcr.microsoft.com/mssql' .github/workflows/ | wc -l
4
```

Seven `image:` sites, not five — the original count grepped only `.github/workflows/`, missing
`docker-compose.dev.yml` and `docker-compose.cluster.yml`. And there is a second kind of site the
original did not contemplate at all: four `docker ps --filter ancestor=...` invocations. **Docker's
`ancestor=` filter matches on the reference the container was created from, not on the repository**, so
pinning `image:` while leaving `ancestor=mcr.microsoft.com/mssql/server:2022-latest` would have left
those filters matching nothing and every subsequent `docker exec "$CID"` running against an empty
container id. Probed before trusting it, using `alpine` as a stand-in so the real containers were not
disturbed:

```
docker ps -q --filter "ancestor=alpine@sha256:c64c687c..."   # 1 match
docker ps -q --filter "ancestor=alpine:3.20"                 # 0 matches
```

That is a partial pin that reports green while testing nothing — this document's subject exactly, and
it would have been introduced *by the fix for* this document's item 3.

Eleven sites now have to move together, which is more than a reviewer will hold in their head.
`scripts/check-workflow-guards.py` (item 8, #399) has a guard for precisely this: every `ancestor=`
reference must equal an `image:` reference in the same file.

`postgres:16` (8 sites) and `mysql:8.4` (5) still float within a minor series — same class, lower risk,
still a follow-on.

---

<details>
<summary>The original finding</summary>

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

</details>

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

**4a — DONE, and the estimate was wrong by an order of magnitude.** The sequence table below called
this *"~30m — may already be stale — cheapest possible win"* and guessed the fix was deleting a line.
The skip's stated cause **was** stale, exactly as predicted: it names a wazero v1.11.1 panic, and under
CGO-on the test reaches PostgreSQL and passes. That is where the prediction stopped being right.

Behind the stale skip were **three unrelated, real defects**, none of which anything else in the repo
was positioned to find, because this is the only test under `./tests/...` that touches MySQL or SQL
Server at all:

| | Defect | Fix |
|---|---|---|
| mysql | `tests/plugin-harness` carried its own private SQL splitter that did not understand `DELIMITER`, so it cut stored-procedure bodies in half — while `migration.Runner` had a correct splitter it declined to reuse | #392 (`d99943e`) |
| mssql | the same private splitter cut on every `;`, ignoring `GO` batch separators | #395 (`f9c6ba6`) |
| mssql | `migrations/mssql/001_schema.sql` dropped its `SECURITY POLICY` objects *after* `CREATE OR ALTER FUNCTION dbo.fn_tenant_filter`, which a policy's `FILTER PREDICATE` holds a hard dependency on — so the file could not be re-applied to a database that already carried it, despite a header claiming it was idempotent | #396 |

The third is in a **shipped migration**, and the file's own header said `Idempotent: all statements use
IF NOT EXISTS / IF EXISTS guards`. That was true statement-by-statement and false of the file, which is
its own small instance of this document's thesis: the guard was present on every statement and guarded
nothing about the order they ran in.

The skip removal and the budget drop to 0 are #400, which is what makes the three fixes stay fixed.

**What to take from the estimate being wrong.** "The stated reason is stale" and "there is nothing
behind it" are different claims, and the first does not imply the second. A skip suppresses whatever
would have happened next, not only the thing its message names — so the cost of removing one is
unknowable until it is removed, and estimating it as a line-delete is estimating the size of something
nobody has looked at. Three defects had been sitting behind this one since before it was written.

---

**4a (original).** `TestPluginCalls_MultiDB` skips unconditionally. Budget `plugin-harness/multi-db 1`, whose own
comment says *"that job starts PostgreSQL, MySQL and SQL Server and then tests nothing at all… Drop it
to 0 when the skip is removed."* It is the only test under `./tests/...` touching MySQL or SQL Server.
The stated cause is a wazero v1.11.1 panic — and `plugin-harness-ci.yml` now runs CGO-on throughout,
which is what removed the sibling skip in `TestPluginCalls_Wasm_Go`. **Check whether this skip is still
true before doing anything else**; it may already be stale, in which case the fix is deleting a line.

**4b — checked, still correctly blocked. Left in place.** `TestTenantIsolationOverHTTP_MSSQL` skips
unconditionally, blocked on §2.71 (`SESSION_CONTEXT` cleared by `sp_reset_connection` on a pooled
connection). Budget `test-go/commands 6`. §2.71 is now 🔶 PARTLY FIXED; the part still open is that the
test schema is missing two tenant-scoped tables, so the acceptance test cannot yet assert what it
exists to assert. Correctly recorded as §2.71's acceptance test rather than deleted — leave it until
§2.71 closes, then drop the budget. Re-checked 2026-08-07; unchanged.

**4c — DONE in #381 (`d6c057f`).** The four `wasm_plugin_test.go` compat skips are conditional and were
already neutralised, because
`plugin-harness/wasm` has a budget of 0 — any of them firing turns the job red. That is the right
outcome reached by an indirect route, and it is fragile: the skip *text* still says "compatibility
issue", so the next reader sees a skip and a budget failure and has to work out which is lying.
Converted to `t.Fatalf` with the same message. No behaviour change under the current budget; it stops
the budget being the only thing standing between a crash and a green.

One thing worth recording from doing it: `scripts/check-skips.sh` is a **set-membership** guard over
`scripts/skip-baseline.txt`, not a threshold, so removing skips is not automatically safe either —
adding an unrelated `t.Skipf` elsewhere in the same PR fails `Lint` until the baseline is regenerated.
It caught one during this batch (#394). That is the guard working, and the baseline is machine-written:
`--update` does `printf '%s\n' "$fresh" > "$BASELINE"`, a whole-file overwrite, so a hand-added comment
in it is deleted on the next regeneration without saying so. Annotate the `t.Skipf` call site instead.

**Verify (4a):** remove the skip, run `./tests/plugin-harness/... -run TestPluginCalls_MultiDB` with
CGO on and all three DSNs. If it passes, drop the budget to 0 in the same PR — the budget is what stops
it regressing.

---

### 5. ~~`tests/exhaustion` cannot be gated~~ — DONE in #394 (`96a6247`)

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
(redacted). Both, in that order — the second is the one that has been getting skipped past. Both were
run, in that order.

---

### 6. ~~`pluginapi` is the external contract and has no tests~~ — DONE in #388 (`a55d375`)

Compile-time contract assertions over the re-export surface. **The obvious form of this test is
vacuous, which is the part worth writing down.** For a type alias, `var _ A = B{}` and `var _ B = A{}`
both compile whether the declaration is `type X = Y` (an alias, what external authors depend on) or
`type X Y` (a distinct type that silently breaks every caller passing one to the other). Two-way
assignment proves nothing about an alias. The assertion has to compare `reflect.Type` identity, which
is the only thing that separates the two declarations.

That is the same shape as everything else in this document — a check that runs, passes, and cannot
fail — and it would have been introduced by the fix for the item complaining about it.

`wasmrw` and `monitoring/prometheus` are untouched and still stand as written.

<details>
<summary>The original finding</summary>

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

</details>

---

### 7. §2.60d — `CleanupPostgresTestData`, and the `-p 1` tax — NOT DONE, deliberately

Open in `IMPROVEMENT-PLAN.md`. An unqualified `DELETE FROM` across eleven tables, which is why every
multi-package database run in this repo needs `-p 1`, which is why `Tier 1 Gate` takes ~11 minutes and
is reliably the last check to finish on every PR.

It stays open because the fix is a design decision across ~40 call sites — database-per-package,
tenant-per-package, or per-test transaction rollback — not a mechanical change. **It is listed here for
its cost, not because it is ready.** Costing every PR ~11 minutes of serialised wall clock is the
argument for picking one; picking one is not something to do in passing.

---

### 8. ~~The mechanism the sweep is missing~~ — BUILT in #399, as four guards not three

`scripts/check-workflow-guards.py`, run from the required `Lint` job. Each guard was tested by breaking
the thing it guards and **reading the message**, not just the exit status. What they cover, and what
each cost to get right:

**Guard 1 — every required context resolves to a job that exists.** Needs matrix expansion, since ten
of the 32 contexts are matrix arms (`Test Go (engine) on 1.26`). The expander refuses `include:` and
`exclude:` rather than approximating them:

```python
raise Unexpandable(f"{where}: strategy.matrix uses '{key}', which this guard does not expand. "
                   f"Teach it, or the guard is weaker than it looks.")
```

A guard that silently under-expands is this document's subject in miniature, so it fails loudly instead.

**Guard 2 — no `continue-on-error` on a required job.** *"Currently zero — worth keeping there"* was
**wrong**, and wrong in a way that matters: it is true at *job* granularity and false at *step*
granularity. Two steps inside the required `Lint` job carried `continue-on-error: true` — Ruff and
ShellCheck. A required check with a step that cannot fail is #370's shape exactly, in the one job the
sweep did not look inside. The guard now checks both levels.

**Guard 3 — no floating tag in a `services:` image.** As specified; `:latest`, `-latest`, and a bare
repository with no tag at all (which Docker resolves as `:latest`) are the recognised cases.

**Guard 4 — every `docker ps --filter ancestor=` reference equals an `image:` in the same file.** Not
in the original three; it exists because item 3 turned up the failure mode. It scans **parsed `run:`
bodies**, not raw file text — scanning raw text flagged the worked example inside `tier1-gate.yml`'s
own explanatory comment, which is the guard being right about a string and wrong about the repo.

**What guard 2 found, and how the finding was nearly missed.** Having removed the two waivers, I
recorded that both linters "were already passing," on this evidence:

```
gh api repos/cleat-team/cleat/actions/runs/<id>/jobs \
  --jq '.jobs[] | select(.name=="Lint") | .steps[] | .conclusion'     # success
```

That reads `success` for **any** step with `continue-on-error: true`, because the waiver rewrites the
field. It is the output of the waiver, not of the tool — the same mistake as trusting a green check,
one level down, and made while writing the guard against it. Running the tools directly, 2026-08-07:
**ruff 260 errors** (210 fixable), **shellcheck 7** (5×SC2164, 2×SC2034, 2×SC2016, 1×SC2181). What
caught it was making the step blocking and watching CI go red — `Lint` failed at step 15, Ruff. Nothing
else would have.

Both moved to a new non-required `Lint (advisory)` job. That is not a rename of the waiver: the job
goes **red**, visibly, on the PR — it simply does not gate. Making 267 findings blocking on every PR is
how a check gets switched off; leaving them invisible is how they stayed at 267. Promote each back into
`Lint` as its backlog reaches zero.

**Ruff was promoted back on 2026-08-08 (#403, #404), and its 260 turned out not to be a backlog.**
This document said the number "need[s] a rule-set decision first, since this repo ships no ruff config
and is being measured against defaults." Both halves of that were wrong, and the second is the
interesting one. `python-sdk/pyproject.toml` had always carried a `[tool.ruff]` section — line-length,
target-version, exclude — with no `select`. With no `select` the rule set is ruff's built-in default,
and **that default is not a fixed thing**: it changes between ruff releases, and CI installed ruff
unpinned. Measured on the same tree with the same config, only the version changing:

```
$ pip install ruff==0.6.9  && ruff check python-sdk/    #  60 rules enabled,   3 errors
$ pip install ruff==0.16.2 && ruff check python-sdk/    # 413 rules enabled, 260 errors
```

So the strictness of a lint check was set by a third party's release schedule — item 3's defect
exactly, in a place nobody had thought to look for it, and this one had already fired. The 260 was
about to be triaged as if it described the code.

Fixed by naming the rules (modelled on httpx's set, extended after measuring every family against this
tree) and pinning the version in three places that have to agree: `required-version` in pyproject, the
`pip install ruff==0.16.2` in `ci.yml`, and the `dev` extra. Ruff refuses to run on a mismatch, so they
cannot drift silently. `--fix` cleared 230 of 256; of the 42 left, one was a real defect (a mutable
default argument shared across every call), six were a rule that was wrong at all six sites (`PERF203`,
unselected with the reason rather than suppressed six times), and twelve were deliberate boundary
catches that now say so at the call site.

The loop closes on the mistake that started this: re-adding `continue-on-error: true` to the promoted
Ruff step now makes guard 2 fail the build. The trap cannot be set silently on a required job again.

**ShellCheck's 7 are still open** and are now the only thing in `Lint (advisory)`. When they reach
zero, promote it and *delete* the job rather than leave an empty green one.

**One thing the guards deliberately do not do**, stated in `.github/required-checks.txt` itself: they do
not verify that file against GitHub. Reading branch protection needs an admin token and a workflow's
`GITHUB_TOKEN` is not one, so the list can drift from the live setting and `Lint` will not notice.
`--verify-against-api` does the comparison when run by hand with a token. **Do not read a green `Lint`
as proof the required set matches the API** — what the guard buys is that the intended set is in git and
reviewable in a diff, and that the far more common failure (a context naming a job that is gone) is
caught.

Current state, 2026-08-07 — `scripts/check-workflow-guards.py`: *checked 14 workflow files, 49 distinct
job names, 32 required contexts, 0 problems.* The only remaining `continue-on-error` in the repo is on
`Coverage`, which is not required — and guard 2 enforces that pairing in both directions.

---

<details>
<summary>The original finding</summary>

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

</details>

---

## Sequence — as executed

| # | Item | PR | Merge |
|---|------|----|-------|
| 1 | CodeQL + secret scanning on; five false doc claims corrected | #393 | `bc67ab3` |
| 4c | `wasm_plugin_test.go` compat skips → `t.Fatalf` | #381 | `d6c057f` |
| 3 | Pin the SQL Server image by digest (11 sites) | #390 | `09322ba` |
| 2 | `wasm-demo` deleted | #391 | `dadf574` |
| 5 | `tests/exhaustion` DSN + skip/fail split | #394 | `96a6247` |
| 6 | `pluginapi` contract assertions | #388 | `a55d375` |
| 4a | plugin-harness MySQL splitter | #392 | `d99943e` |
| 4a | plugin-harness MSSQL `GO` batches | #395 | `f9c6ba6` |
| 4a | MSSQL `001_schema.sql` re-application ordering | #396 | `0a1a097` |
| 4a | skip removed, budget → 0 | #400 | `12f2ae2` |
| 8 | Four workflow guards; two linters that could not fail | #399 | `740203e` |
| — | CodeQL's 3 highs triaged and dismissed with the reasoning in code | #397, #398 | `428b422`, `b795601` |
| — | CodeQL's 11 `missing-workflow-permissions` fixed | #401 | `c97ceac` |
| — | These outcomes recorded here | #402 | `ea54155` |
| 8 | Ruff's rule set named and version pinned | #403 | `cabc89c` |
| 8 | Ruff's last 42 cleared; promoted into required `Lint` | #404 | `993a051` |
| 4b | `TestTenantIsolationOverHTTP_MSSQL` | — | blocked on §2.71, left |
| 7 | §2.60d `CleanupPostgresTestData` | — | not started; listed for cost, not readiness |

Item 4a verified end to end on merged `develop`, 2026-08-08, CGO on with all three DSNs set:

```
$ go test ./tests/plugin-harness/... -run TestPluginCalls_MultiDB -v -count=1
--- PASS: TestPluginCalls_MultiDB (5.52s)   # postgres, mysql, mssql — 3 run, 0 skipped
```

**Four of this document's own claims were wrong**, each caught by running something rather than
reading it — the estimate in item 4a, the date in item 2, *"currently zero"* in item 8 guard 2, and
the ruff paragraph in item 8's outcome. All four are corrected in place above with what replaced them.
A plan is a claim nothing checks, which is the first of the two shapes this document names at the end,
and it does not exempt itself. The fourth was written *into this file* on 2026-08-07 as part of
recording the outcomes, and was wrong by the next morning — recording an outcome is not the same as
checking one.

**Open, untriaged, and named so it is not mistaken for finished:** the 68 Dependabot vulnerabilities (16
critical, 25 high) that item 1 surfaced, and the 8 update PRs it opened, are still untriaged — that was
step 2 when item 1 was written and it is still step 2. **ShellCheck's 7** findings are open and are now
the only occupant of `Lint (advisory)`.

Ruff was on this list on 2026-08-07 and is not any more: its 260 was an artefact of an unpinned tool
rather than a backlog, and it now gates with a backlog of zero — see item 8 above for what that turned
out to mean.

---

<details>
<summary>The original sequence, as planned</summary>

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

</details>

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
