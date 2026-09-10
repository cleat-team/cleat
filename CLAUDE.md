# cleat — Durable Workflow Engine

The core cleat engine: workflow execution, WASM runtime, plugin system, worker daemon, CLI,
admin dashboard.

**This file carries rules that stay true for months.** Volatile facts — what is supported, what
is broken, what the current counts are — live in `tiers.yaml` and `IMPROVEMENT-PLAN.md`, which
are checked. If you find a fact in here that has a date on it, treat the date as part of the
fact. If you find one without a date that turns out to be wrong, fix it in the same PR.

---

## What is supported: `tiers.yaml`

`tiers.yaml` is the source of truth for what this project claims to support. Three tiers:

- **Tier 1 — core.** Must pass. **A skip is a failure.** If it is in tier 1 and it does not run
  green on every dialect and backend named there, that is a release blocker.
- **Tier 2 — frontier.** Must *run*. May fail, against a tracked list of known failures that can
  only shrink without a written justification.
- **Tier 3 — parked.** Not built, not shipped, not claimed. Excluded from the default build or
  deleted.

**Do not claim support in a doc that `tiers.yaml` does not grant.** Every prior attempt to record
status in prose here has rotted within days — `docs/review-status.md` declares the project
production-ready off a stale audit, `README.md` has claimed backend parity that was not true, and
`IMPROVEMENT-PLAN.md` has carried ✅ headings over unfixed prose. The manifest exists so status is
a property CI checks rather than a sentence someone wrote.

---

## Is this result real?

Most expensive recurring failure in this project: **a green result that measured nothing.** Check
these before believing any test outcome.

**An unset DSN skips its dialect silently and the suite still prints `ok`.** Measured 2026-09-03,
`go test ./engine/` on one tree, each configuration run twice in opposite orders:

| `CLEAT_TEST_*` set | passed | skipped | wall (two runs) |
|---|---|---|---|
| none | 3462 | **876** | 158s, 148s |
| postgres only | 3813 | **581** | 182s, 163s |
| all three | 4510 | **4** | 206s, 222s |

All three printed `ok`. **Check what failed and what skipped, not the clock:**

    go test ./engine/ -count=1 -json > /tmp/t.json
    grep '"Action":"fail"' /tmp/t.json | grep -c '"Test":'    # test failures; must be 0
    grep '"Action":"fail"' /tmp/t.json | grep -vc '"Test":'   # PACKAGE failures; must also be 0

**Read the skip NAMES, not the skip count.** This line used to say `# 4 means all three
dialects ran`, and 4 was correct on 2026-09-03. It is 7 now and rising, because the suite
grows: a census of a growing population is guaranteed to go wrong, and the only question is
when. Worse, a stale one inverts — a reader who sees 7 against a documented 4 concludes
something is broken on a run that is fine.

What you actually need is a predicate, and it does not drift: **no remaining skip is
dialect-gated.** That was true at 4, is true at 7, and will be true at 12.

    grep '"Action":"skip"' /tmp/t.json |
      python3 -c 'import sys,json; [print(" ", json.loads(l)["Test"]) for l in sys.stdin if "Test" in json.loads(l)]'

Every name it prints should be an environmental precondition you can point at — a toolchain
that is not installed, a fixture that needs a binary. A name mentioning postgres, mysql,
mssql or a dialect means a DSN did not take, whatever the count is. See cleat#986: four of
five counts in this file had drifted when checked, and every one of them was a census.

**That third line is not decoration, and this file shipped without it.** A package that does
not compile emits a **package-level** fail event carrying no `"Test"` field, so a count keyed
on `'"Test":'` cannot see it. Measured 2026-09-07 on a two-package module, one compiling and
one not:

| | |
|---|---|
| fail events **with** a `"Test"` field | **0** — the check above says "must be 0", and it is |
| fail events **without** one | 1 — the package that did not compile |
| pass events with a `"Test"` field | 1 |

So the documented check reports a clean run on a tree where a package never built. Worse than
a skip, because a skip at least appears in the skip count: **a package that does not compile
contributes nothing to any of the three numbers.** It reads cleanest exactly where it measured
least, which is this section's whole subject, in this section's own command. Reported by the
cleat-ports session after WS-3 lost a lint run to it; verified here before being written down.

That column is exact, reproducible, and machine-independent: both orderings gave an identical
876 / 581 / 4.

**The wall-clock check this file used to recommend is dead — do not use it.** It read "roughly 20s
means Postgres only, roughly 60s means all three", off a 2026-08-06 measurement of 16s and 60s.
A no-DSN run now takes about 150s, so a run that tested *no database at all* clears the old
"all three" threshold by 2.5× — the exact false green this section exists to prevent. And the gap
between adjacent configurations (~19s from none to postgres-only) is now the same size as
run-to-run variance on one configuration (postgres-only measured 182s and 163s), so the clock
cannot separate them even in principle. The suite grew about 8% in test functions over that
month; the wall times grew tenfold, most of it machine and load.

**A DSN that is set but does not connect looks exactly like one that works.** Setting the variable
is what stops a test skipping. Connecting is a separate question, and neither the skipped count
nor the clock asks it — those tests fail on connect instead of skipping. Writing this section I
reconstructed all three DSNs from memory, got the database name and two passwords wrong, and
measured a tidy 876 → 581 → 4 progression whose final `4` matched the old table exactly. It was
1086 connection failures wearing the right costume, and the matching `4` read as corroboration.
**So count failures too**, or probe first:

    go test ./engine/ -run TestPluginMigrations_AllDialects -count=1

**The command this file gave here until 2026-09-04 was `-run TestTenantIsolationAcrossDialects`,
and no such test exists.** It was named only here, never in the tree
(`grep -rn TestTenantIsolationAcrossDialects --include='*.go' .` → nothing). A `-run` pattern that
matches nothing is not an error: `go test` prints `ok … [no tests to run]` and **exits 0**. So the
probe returned a green with a wrong password, against a nonexistent database, and with no DSN set
at all — all three measured. The command this file offered as the cure for "a green result that
measured nothing" was itself one, for as long as anyone ran it.

That generalises past this one name. **Before trusting any `-run` probe, check that it selects
something:**

    go test ./engine/ -run <name> -count=1 -v 2>&1 | grep -c '^=== RUN'
    # 0 means the pattern matched nothing. TestTenantIsolationAcrossDialects: 0.
    # TestPluginMigrations_AllDialects: 4 (the parent and its three dialect subtests).

`TestPluginMigrations_AllDialects` replaces it because it has the property a probe needs and the
old name only implied: `BootstrapScratchDB` distinguishes configured-but-unreachable (`Fatal`)
from nothing-configured (`Skip`), so a DSN that is **set and does not connect fails** rather than
skipping. Negative control, measured 2026-09-04, under a second per run:

| DSN varied | result |
|---|---|
| all three good | `ok` |
| postgres password wrong | `FAIL … password authentication failed for user "cleat" (28P01)` |
| mysql password wrong | `FAIL` |
| mssql password wrong | `FAIL` |

A probe with no negative control is a claim, not a check — the same rule this file states for
`gh pr checks` watchers, which is where it was learned the first time.

The DSNs are written down in `WORKSTREAM.md`, under "Sandboxes, databases, and shared files" —
every port, with the credential matrix, because the credentials are port-specific and a probe that
varies only the port answers the wrong question. Read them rather than rebuilding them from memory.
(They were in `WS3-STATUS.md` and `PARALLEL-WORKSTREAMS.md`; both were retired 2026-09-04.)

**Build and test with CGO on — the default.** `CGO_ENABLED=0` does not skip a check. It swaps
`NewWasmtimeBackend` for the `//go:build !cgo` stub in `engine/backend_wasmtime_stub.go`, which
returns `ErrWasmtimeCGOUnavailable`, so **there is no backend left at all** — `cleat-worker`
logs "wasmtime is the only WASM backend cleat has, there is no fallback" and exits 1
(`cmd/cleat-worker/main.go:790`). **An engine result obtained that way is not evidence about the
engine.** If a genuine toolchain failure forces it, say so in the PR rather than leaving the
reader to assume wasmtime was exercised.

Note what that does *not* do: `CGO_ENABLED=0 go build ./...` still exits 0 (measured
2026-08-30). Nothing tells you at build time; the failure is at worker startup. This paragraph
used to say a CGO-less build "silently runs everything on wazero" — that stopped being true
when the wazero backend was deleted, and it is the opposite of what happens now.

**Use `-p 1` when running more than one database-backed package in one invocation.**
`engine/testutil`'s `CleanupPostgresTestData` issues an **unqualified `DELETE FROM`** over
`postgresCleanupTables`, and that list includes `workflow_instances`. Run concurrently against
one database and packages delete each other's fixtures mid-test; the failures look like
unrelated flakes.

The length of the list is not the point and this used to give it as eleven, which was wrong by
four when checked (cleat#986). What makes it dangerous is that the DELETE carries no `WHERE`
and the list reaches the table every test depends on. Ask that, rather than counting:

    python3 -c "
    import re
    src = open('engine/testutil/schema.go').read()
    body = re.search(r'postgresCleanupTables\s*=\s*\[\]string\{(.*?)\n\}', src, re.S).group(1)
    print('workflow_instances in the list:', 'workflow_instances' in body)
    "

I first published a `grep -c '\"'` here and it answered **16** against a list of **15** — a
comment inside the block carries a quoted name. A census got the census wrong, in the
paragraph explaining why not to publish one.
**And `-p 1` does not help across two `go test` PROCESSES.** It serialises packages within one
invocation, so two concurrent runs against one instance both obey it and both wipe the same
tables. `CleanupMSSQLTestData` is the same shape. See cleat#982, where that is the leading
explanation for four SQL Server failures that would not reproduce alone.

**A skip that hides a crash is not a skip.** `t.Skipf("... crashed")` and
`t.Skipf("... compatibility issue")` are failures wearing a skip's clothing, and they make a
regression invisible forever. A skip is legitimate only for a genuine environmental precondition
(a toolchain that is not installed, a DSN that is not set).

**"No pending checks" also matches "checks never started."** Guard `gh pr checks` wait-loops on a
total count, not on the absence of pending.

**But do not hardcode that total, because it is path-dependent.** Measured 2026-09-01, after
every check had settled:

| PR | touches | checks |
|---|---|---|
| #504 | `CLAUDE.md` only | 46 |
| #500 | docs + `engine/` | 46 |
| #503 | `engine/` + `cmd/` | **50** |

The four `{AssemblyScript,Java,Python,Rust} SDK Integration` jobs are the whole of the gap; they
trigger only on some paths. `Benchmarks` and `Coverage` report `skipping` on a normal PR, so a
green run shows two fewer passing than the total. Re-derive with
`gh pr checks <pr> | grep -c .`, and diff two PRs with `comm -23` over the sorted name column to
see which jobs a path triggers.

**"After every check had settled" is load-bearing, and I got it wrong writing this.** The first
draft of the table above said #504 ran 45, because I ran `grep -c .` 25 seconds after pushing —
before the 46th check had been registered. A total sampled while checks are still being created is
itself a "checks never started" reading, and it is the more dangerous kind, because it looks like
a settled fact rather than a pending state. If you are recording a total, take it from a PR whose
checks have all finished, not from one you are still watching.

A fixed floor is therefore weaker than it looks: gate at 46 and a PR that should run 50 passes the
moment its 46th check settles, with four SDK jobs not yet queued — which is precisely the
"checks never started" case this paragraph is about. **The reliable arbiter is the branch policy,
not a count.** `gh pr merge` refuses while required checks are outstanding, and
`gh pr view <pr> --json mergeStateStatus` reports `BLOCKED` rather than `CLEAN`. On
2026-08-31 that refusal was the only thing that caught a watcher reporting green over six
pending checks.

**Both halves of that were observed live on 2026-09-08, in one run, and the second is the
direction this file did not have.**

The first: a watcher on `cleat-ports#50` sampled `total` at **4**, repeatedly, and later at
**5**. The fifth check registered mid-run, so the total a watcher reads is not stable *within a
single run* — the table above was measured across *settled* PRs and describes that hazard
without ever catching it moving.

**State what was and was not observed, because the difference is this section's whole subject.**
Observed: `total=4` on samples 1-3 and `total=5` at settle. NOT observed: a four-of-four-passing
sample. `pending` was 2 while the total read 4, so the dangerous state — a complete-looking set
that is merely the registered subset — did not occur on this run. The floor was never actually
fooled here; what was demonstrated is that the quantity it gates on moves underneath it.

The second, and it is the mirror image: **a floor tuned to one repo is not a floor in another.**
That watcher was carried over from this repo with its 40 hardcoded, and `cleat-ports` runs
**five** checks (`gh pr checks <pr> --repo cleat-team/cleat-ports | grep -c .`). A floor of 40
in a five-check repo can never be satisfied, so the loop cannot report a false green — it runs
to timeout reporting nothing at all.

**That one was caught by reading before arming, not by observation, and the distinction is
recorded rather than smoothed over.** The floor was lowered to 4 before the watcher was started,
so the timeout never happened; what is measured is the check count, and the consequence follows
from the loop's exit condition rather than from a run. A prediction and a measurement are not
the same evidence, and this file is the wrong place to blur them.

That failure is nastier than the one above, because **a watcher that never finishes looks like
patience rather than a bug.** A false green is at least an answer someone can check; a loop still
sampling at attempt 200 invites "CI must be slow tonight". Same root cause — a count treated as a
constant when it is a property of the repo and the path — and it is the reason the floor should be
a sanity bound rather than the gate. Gate on `mergeStateStatus == CLEAN`, held across two
consecutive samples; keep the floor only to catch a total of zero.

**And parse that output with `awk -F'\t'`, because check names contain spaces.** The total-count
guard above is necessary but not sufficient: it does not help if the *pending* count is itself
silently zero. `gh pr checks` is tab-delimited with names like `Tier 1 Gate` and
`Test Go (engine) on 1.26`, so the natural-looking

    pend=$(echo "$out" | grep -cE '^\S+\s+pending')     # blind to 40 of 46 checks

cannot match them — `^\S+` takes `Tier`, `\s+` takes the space, and the next token is `1`, not
`pending`. Measured 2026-08-31 on #500, **40 of the 46 names contain a space**
(`gh pr checks <pr> | awk -F'\t' '$1 ~ / /' | grep -c .`); the six it can still see are `Build`,
`CodeQL`, `Lint`, `lint-go`, `Benchmarks` and `Coverage`, none of which are the ones that matter.
`Tier 1 Gate` and `Test Go (engine) on 1.26` are both in the blind 40 — so the count does not
degrade, it reads zero for exactly the checks worth waiting on. That reported #500 as green with
six still running; `gh pr merge` refusing with "the base branch policy prohibits the merge" was
the only thing that caught it. Use

    pend=$(echo "$out" | awk -F'\t' '$2=="pending"' | grep -c .)
    fail=$(echo "$out" | awk -F'\t' '$2!="pass" && $2!="pending" && $2!="skipping"' | grep -c .)

The general rule this is an instance of: **a verification script needs its own negative control.**
Before trusting a new watcher, run its parse once against a PR that has known-pending checks and
confirm the count is non-zero. A loop that cannot see the state it looks for does not fail —
it prints a confident green, which is the same failure this file's whole "Is this result real?"
section is about.

**And a negative control is not enough — it needs a KNOWN-POSITIVE too.** The rule above asks
"can it see the state it looks for". This one asks the harder question: **"does it report a case
that is genuinely broken?"** Those come apart, because *"it passes when everything is fine"* is
satisfied by every broken version of a guard.

Measured 2026-09-05 on one new guard (#749), which shipped three bugs, all found this way, all
**permissive** — each made the guard pass a tree it should have failed:

  * It parsed `go test` **to end of line**, so a `-run` on a continuation line read as
    *unfiltered* — and unfiltered means "selects everything". The step the guard existed to
    protect is itself line-continued.
  * It treated any `./...` as covering any module. `./...` is module-relative, so an unfiltered
    run in one module vouched for a test in another.
  * It walked the tree with `rglob`, descending into `.claude/worktrees/` — a whole second copy
    of the repo — and attributed a test to a module that exists only in a scratch checkout.

All three passed a green tree and a negative control. What found them was declaring one case
already known to be broken — a test that no CI job selected — and checking the guard said so. It
did not, three times. The third is the one to watch for, because it is not a parsing mistake: it
is a scope mistake, and it makes the guard **more** likely to pass as the working tree gets
messier. Prefer `git ls-files` over `rglob`/`find` for anything that reasons about "the repo".

The same defect has an over-reporting sign, and it is the cheaper one to have: a checker that
greps for `go test` without excluding comment lines reports a line of prose as an invocation, and
sends you chasing a defect that is not there (measured 2026-09-05, #748). Neither direction
models the difference between a command and text that looks like one. Join line-continuations and
drop comments **before** asking any question about a shell command in a workflow.

**A count answers "did this go up". It never answers "is anything still missing".** Same day,
same family: a `-run` pattern was widened to select a test that was running nowhere, and the fix
was verified by counting that test's subtests, 0 before and 24 after. That is proof about one
test and silent about its siblings — a sibling guard was still selected by nothing, because the
pattern `TestHostCalls` stops matching one character into `TestHostCallTable…`: `s` against `T`.
A widened pattern that fixes one name can leave the next one out, and the count cannot say so.

**Use `-list`, which answers directly what a pattern selects, and diff the SET rather than
comparing counts:**

    go test ./... -list '<pattern>'      # what does this pattern actually select?

`-run` answers that only by implication, and is silent when the answer is "less than you think".
This is the same shape as **"No pending checks" also matches "checks never started"** — earlier in
this same section, not two sections up as this line said until #755. Cite a rule here by its
quoted phrase, which `grep` finds; a positional reference rots the moment anything is inserted
between, and this one was wrong the day it was written.

**These are one rule, and naming it is cheaper than meeting it a fifth time.** A check can tell
you whether it is **consistent with itself**. It cannot tell you **what it is not looking at**.
Every fix above is the same move: a *second, deliberately different* reading of the same source,
chosen so that it goes wrong in the opposite direction.

| what the check answers about itself | the second reading that answers what it misses |
|---|---|
| "does it pass when the tree is fine?" | a **known-positive** — a case already proven broken |
| "does `-run` run what I meant?" | **`-list`**, which names the set it selects |
| "does the extractor parse without error?" | a **looser parse** of the same file, over-matching on purpose |

The right-hand column is not the more correct one. The loose parse is deliberately wrong and would
make a bad extractor; its only job is to **disagree**, so the difference can be examined. A strict
parse that runs clean and a loose parse that finds nothing more is evidence. A strict parse alone
is a claim.

The fourth instance arrived the same day, and is the one that shows the cost. `rust_surface()`
matched `pub fn <name>(`, and a Rust generic method is `pub fn <name><T: …>(`, so **ten of
seventy-one methods on `HostCalls` were invisible to it**. Nothing failed, nothing was skipped, no
count went down. Coverage was reported as **61/61 = 100% when it was 63/71 = 88.7%** — and that
100% had been written into four places in `IMPROVEMENT-PLAN.md` and repeated to the user before
anyone read the same file a second way (#753). And one host call is reachable **only** through
methods the scan could not see: the `cleat_call_retry` extern is referenced from exactly one place
in the SDK, inside `cleat_call_with_host_retry<T, R>`, which is generic — so a fixture exercised
it and neither metric counted it.

**Watch the direction, because it is not random.** All four of these inflate rather than break: a
smaller surface is a smaller denominator, so a method the scan cannot see *raises* the percentage;
an under-selecting `-run` reduces the failures available to find; a permissive guard passes a tree
it should fail. **The measurement errors that survive are the ones that flatter the number** —
nobody re-derives a figure that looks good. That is the same asymmetry as the UTC-offset error in
*Ground rules for changes* below, which inflated a count "in the direction that flattered the
finding — which is why it was not questioned."

**The fifth instance is the sharpest, and it is one line of source.** A scan for binding names
scored the AssemblyScript SDK as *having* `cleat_register_query_handler` — by matching this, at
`packages/cleat-as/assembly/host-calls.ts:391`:

    // There is no import_cleat_register_query_handler here (removed 2026-08-09).

An explicit denial, read as a confirmation. A second scan got the right answer only because `\b`
cannot match after the `_` in `import_cleat_` — not a mechanism anyone chose, and not one that
survives a rename. The two agreeing would have proved nothing; their *disagreeing* is the only
reason anybody looked, and it is why #757 publishes no binding counts until they reconcile. This
is the "grep a retraction satisfies" trap under *Build*, one level up, and worse: that line is the
**only** occurrence of `register_query_handler` in the file
(`grep -c register_query_handler packages/cleat-as/assembly/host-calls.ts` → 1). The sole evidence
the scan had was a sentence denying the thing it recorded.

So when a check and the thing it checks agree, ask what a differently-wrong reading would say
before recording the agreement as a result.

**The mechanism under all of them: a tool applied to a format it does not model.** Four separate
failures on 2026-09-05, plus one already recorded above from 2026-08-31 — one shape —

| the read | the format it did not model | what models it |
|---|---|---|
| `cmd \| tail; echo $?` | a pipeline's exit status is the *last* command's | redirect, or `${PIPESTATUS[0]}` |
| `grep -oE '"Output":"[^"]*"'` | a JSON string can contain `"` | a JSON decoder |
| `grep -cE '^\S+\s+pending'` | a tab-delimited table whose fields contain spaces | `awk -F'\t'` |
| `pub fn <name>(` | a declaration can carry generics | a parse that admits them |
| a name scan over `.ts` | source contains prose *about* names | anchor to where the artifact lives |
| `` `[^`]{10,600}` `` | a rejected literal leaves the scan mid-string | pair first, filter after |

"Check your regex" is the weak form of this. The strong form is that **a text search cannot tell a
thing from a sentence about the thing**, and neither can a line-oriented read of a structured
format. When the answer matters, read it with something that knows the shape.

**That last row is a different failure from the four above it, and the difference is the reason it
is here.** Every other row MISSES what it aimed at, which is bad and visible: a count comes out
low and you go looking. A bound applied *while pairing delimiters* does something worse — it
**re-phases the rest of the file and consumes an unrelated statement as a delimiter.**

Measured 2026-09-10, surveying `cmd/` for SQL that reaches an RLS table.
`cmd/cleatctl/replay.go` holds exactly two backtick literals: a 698-character usage string, then a
303-character `SELECT`. With `` `[^`]{10,600}` `` the first is rejected for length — and the regex
resumes *inside* it, so its closing backtick pairs with the SELECT's opening one and the statement
is swallowed as a delimiter. The bound excluded nothing it was aimed at and hid something it was
not. **And a count cannot see it**: both readings return exactly ONE literal from that file,
just not the same one — a substitution rather than a shortfall, so a sanity check on the total
agrees with itself.

Two scans of that population, run independently, disagreed at 6 against 12; five separate
narrowings were found reconciling them, and **every one had been written as a parsing
convenience** — a length bound, a 60-character proximity rule, `…Context`-only call forms,
backticks-only, and anchoring on the call site at all. That last one is the least visible: a query
living in a `[]struct{label, query string}` executed later in a loop is invisible to a call-site
scan **by construction**, and it held nine of the seventeen.

Neither session found its own. The correct total, 17, came out of the disagreement — which is the
case for two derivations that CAN disagree rather than one that gets checked. Cite it with the
command, and pair before you filter:

    # WRONG: the bound is inside the pairing
    re.finditer(r'`([^`]{10,600})`', src)
    # RIGHT: pair, then discard
    [m for m in re.finditer(r'`([^`]*)`', src) if 10 <= len(m.group(1)) <= 600]

**And the portable version of that, which needs no parser: anchor to where the artifact lives, not
to what it is called.** "Extract the declarations" requires a tool per language. `^` does not. A
retraction is prose in a body and can never sit at a declaration site; a changelog row recording a
removal cannot start a heading line. That one move covers every row above — and it is applicable
to a file you have never seen, which "write a real parser" is not.

**The unifying question, and it is the one to ask before recording any confirmation: could this
check have disagreed?** Every trap above is a check that was going to say yes whatever the truth
was. Three ways that happens — the first two measured 2026-09-08, the third 2026-09-10 — and none
looks like a weak check at the time. **The first two are checks that could not see far enough. The
third never ran at all.**

**1. A documented failure mode absorbs every instance of its symptom, including the ones it does
not explain.** `401 invalid or revoked API key` from the port harness has *four* causes — a stale
key file, another session rewriting that file, your worker stopped by a stranger's `worker.sh
ensure`, and your tests reaching a stranger's worker on a shared default port. The Makefile
documents the first one completely and correctly. So the first one is what every 401 gets read as.

I had the disconfirming evidence in hand and did not use it: the key file's mtime had no
corresponding row of that age in my own database, which only another writer explains. I ran that
query, read "no matching row" as staleness, and stopped. **A sufficient explanation terminates
the search** — and a documented hazard is worse than an undocumented one here, because it supplies
a ready, plausible, locally-correct story at exactly the moment you are confused enough to take
it.

So a documented failure mode needs **a stated way to tell it apart from its neighbours**, not just
a description of itself. cleat-ports#69 does that: one discriminator for all four —
*does the process serving my API port have my DSN?* — answered by `pgrep -fl cleat-worker`.

**2. Corroboration from a shared scope is one derivation run twice.** cleat#1009 reported that the
engine's `error_code` is never returned to any client. Two sessions checked independently: every
JSON tag in `cmd/cleat-worker/` enumerated, `map[string]any` responses separately ruled out, and
only three tags mentioning error or code — `error_code` and `error_message`, both fields of the
ForceFail *request*, and a bare `error`. Careful work, and both agreed.

Both were wrong. `handleGetWorkflow` builds no response struct — it serialises
`engine.WorkflowInstance` whole, and that type's `error_code` tag lives in `engine/`, one package
away from where both scans looked. The field is returned; a live HTTP response shows it.

The agreement proved nothing because **both derivations had the same denominator**. This file
already records that trap for *numbers* — two export counts agreeing at 55 while differing on six
members — and it works identically on conclusions. **"Independent" has to mean differently scoped,
not separately performed.** When a second party confirms, ask what they *searched*, not whether
they agreed.

**3. A check whose SETUP destroys the state it is measuring.** This is the hardest of the three to
spot, because the setup is where you are being careful — the damage is done by the part of the
procedure you added on purpose.

Measured 2026-09-10, twice, by two sessions, in opposite directions, within an hour. The question
both times: *does this test re-run when the fixture it reads changes?* — a shared JSON table at the
repo root, read at runtime by consumers in several languages (cleat#1136).

| | how the cache was cold before the "measurement" | what was concluded |
|---|---|---|
| one session | the previous step had already changed another tracked file | "the change was noticed" |
| the other | the first run used `-count=1`, which writes no cache entry | "the change was noticed" |

Both readings were of a run that was going to re-run regardless. **An experiment about caching must
begin from a WARM cache and change exactly one thing** — and the sting is in the setup: any
`-count=1`, `clean`, or `--rerun-tasks` there destroys the state under test. Those are exactly the
flags careful practice tells you to add, so **the more disciplined the habit, the more reliably it
erases the evidence.**

Done properly, one change at a time from a warm cache, the answer is a fact about Go worth knowing
on its own: **`go test` caches on files opened INSIDE the package directory, and a fixture read
through `..` is invisible to it.**

| change | `go test` (no `-count=1`) |
|---|---|
| nothing | `ok (cached)` |
| a file the test read, **outside** the package dir | `ok (cached)` — not noticed |
| a file the test read, **inside** the package dir | `ok 0.205s` — re-ran |

So a test whose fixture lives outside its package silently passes against an edited fixture. CI is
sound here only because `ci.yml` passes `-count=1` to every matrix package; a local `go test ./...`
is not, and **the tell is the word `(cached)` where a duration should be**.

Three build systems concealed that same fact three ways in one afternoon, and none of the three
reports anything wrong. Gradle hid it in the **duration** — `BUILD SUCCESSFUL in 578ms` against a
real 3s, on a deliberately corrupted fixture. Go hides it in a **parenthesis**. pytest does not
cache at all.

**Note which of those was caught and how**, because it is the argument for what follows: the gradle
staleness was real and was spotted, from the duration alone. The verdict said `SUCCESSFUL` and was
useless; the one field nobody reads was the only one telling the truth.

**The remedy generalises past caching, and it is the one thing to take from this item: print the
PRECONDITIONS beside every row of a result table, not just the verdict.**

    B [stream=WS-1 patched=1 MERGE_HEAD=yes] exit=0 want=0

Those three bracketed fields are what separate *the check ran and allowed it* from *the check never
looked*. A verdict column alone has no way to say "I did not run" — which is this whole section's
subject, and the reason every trap in it reads as a pass.

That remedy was paid for rather than thought up: cleat#1162 collected **three more empty greens in
one sitting**, each from a different mechanism and none reporting anything wrong — a clean merge
runs no pre-commit hook at all; `git reset --hard` silently reverted the test's own sandbox
registration, so the hook skipped for "unknown stream"; and an already-merged `HEAD` staged nothing,
so the commit failed with "nothing to commit". Read that issue before writing a test harness for a
guard.

**A merge's own `develop` run could be cancelled by the next merge** landing seconds later, and
`cancelled` is not `success`. Verifying `develop` after merging means verifying the *current
head*, which contains your commit — not your own SHA.

That cancellation was fixed in two halves — #634 scoped `cancel-in-progress` to pull requests,
#661 gave each push its own concurrency group — so it should no longer happen. The reading skill
outlives the defect, and it is one line:

    gh run view <run-id> --json jobs --jq '.jobs | length'

**A `cancelled` run with zero jobs never started.** It was evicted from a concurrency queue
before any runner picked it up; a run killed mid-flight has jobs, each with a `cancelled`
conclusion. The two look identical in `gh run list` and have completely different causes, and
telling them apart is what separated the two halves above: of 22 cancelled `Tier 1 Gate` runs on
2026-09-03, the 17 before #634 had jobs (one exception) and all 5 after it had none. Measured
2026-09-04; see IMPROVEMENT-PLAN §3.100.

**Read it as zero versus non-zero, never as a count.** The number grows while a run proceeds, so
it is only final once the run is. The same run sampled twenty minutes apart gave `1` and then `3`
here, and the `1` went into a table before this sentence was written.

**"Should no longer happen" is true of `develop` and false of a PR, and the trigger is one you
reach for constantly: EDITING THE PR BODY.** `ci.yml` fires on
`types: [opened, synchronize, reopened, edited]`, and `edited` is the activity type a **title,
body or base change** produces — it is there deliberately, because a retargeted PR would otherwise
never trigger the workflow at all (see the comment above the line). PR runs share one concurrency
group per ref and supersede, by design. So `gh pr edit --body-file` while checks are running
cancels the in-flight run and starts a fresh one.

Measured 2026-09-10 on #1158, by doing it. A push at 17:57:59 created **9** runs; a body edit
**thirteen seconds later** created **7** more against *the same SHA*, and the in-flight members of
the first set were cancelled where they stood:

    gh run list --branch <branch> --limit 30 --json createdAt,conclusion,status \
      --jq 'group_by(.createdAt)[] | "\(.[0].createdAt) count=\(length)"'

| | |
|---|---|
| runs at 17:57:59, sha `64204be9` | 9 — the ones still running were cancelled |
| runs at 17:58:12, sha `64204be9` | 7 — the live set, same SHA |
| steps in the cancelled `Layer 1 — SDK` job | **every one `success`** |
| its job conclusion | `cancelled` |

Note the two counts differ, so this is not a clean "one set replaces another": the trigger and the
path filters between them are not identical, and a run that had already finished stays finished.
Do not read the pair as a constant — re-derive it with the command above.

**That last pair is what makes it expensive.** `gh pr checks` reports a cancelled job as `fail`,
and any watcher whose parse is `$2!="pass" && $2!="pending" && $2!="skipping"` — the one this file
recommends, correctly — counts it as a failure and prints RED. So a body edit produces a red PR
whose failing job has no failing step, which reads exactly like a real breakage and sends you into
a log that says `ok`. The discriminator is the `jobs | length` line above plus the job's own
`conclusion` field:

    gh api repos/<owner>/<repo>/actions/jobs/<job-id> --jq '.conclusion'   # cancelled, not failure
    gh api repos/<owner>/<repo>/actions/jobs/<job-id> --jq '.steps[].conclusion' | sort -u

All-`success` steps under a non-success job means the job was killed, not that it failed. The
practical rule is cheaper than the diagnosis: **get the body right before pushing, and if you must
edit it, do so before the checks start or after they settle.**

**When a schema migration lands, recreate your test databases.** `CREATE TABLE IF NOT EXISTS`
never adds a column, so a long-lived database keeps its old shape and dozens of tests fail on a
missing column. Drop and recreate; do not debug the code.

---

## Ground rules for changes

**Prove every regression test can fail — and read *why* it failed.** Remove the fix, watch it go
red, put it back. This catches a test that *cannot* fail; it does not catch one that fails
*sometimes*. If an assertion depends on wall-clock time, remove the timing rather than widening
it. Twice this has caught a test passing for the wrong reason, which is why "it went red" is not
enough on its own — check that it went red *for the reason you expect*.

**And before either of those: check it went red AT ALL, because a falsification that prints
nothing is not a falsification that passed.** Measured 2026-09-08 while proving a regression test
for cleat#995. The mutation removed a struct field, which left a local variable unused, so the
package did not compile:

    cmd/cleat-worker/server.go:1756:2: declared and not used: loc
    FAIL    github.com/cleat-team/cleat/cmd/cleat-worker [build failed]

The output was filtered with `grep -E '_test.go:[0-9]+:|^---'`, which matches **test** failures.
A build failure produces neither, so the command printed nothing and was read as "the test still
passes" — the conclusion being that the fix was unnecessary.

This is the `-json` trap in *Is this result real?* arriving through the plainest possible door.
There it is a package-level fail event carrying no `"Test"` field, invisible to a count keyed on
`'"Test":'`. Here it is a compile error invisible to a grep keyed on `_test.go:`. **A filter that
can only see one kind of failure reports the other kind as success**, and silence is the most
convincing form that report takes.

The cheap discipline: on a falsification, read the last few lines unfiltered, and satisfy yourself
the test **ran**. `go test` prints `ok` for a pass and `[build failed]` for a mutation that did not
compile, and those are the two cases a filter is most likely to render identically.

A mutation that does not compile is also telling you something: the thing you removed was load
bearing enough that the surrounding code stopped making sense without it. Rewrite the mutation to
keep the tree compiling — assign the zero value rather than deleting the line — and the
falsification becomes possible again.

**And the mirror of that: "it stayed red" is not enough either.** A fix that does not change the
symptom has not been shown to be unnecessary; it has been shown not to be *sufficient*. Before
concluding a change is inert, check whether the failing step **moved** — not just whether it still
fails. #777 needed two fixes in the child-spawn path (#781), and the second was implemented, tested,
judged ineffective and reverted, because the workflow still failed. It still failed because the
*first* defect was unfixed at that moment, and the test happened to be a three-child fan-out
failing at step 3 either way — so the step number was identical before and after, and the reasoning
felt sound. It was then re-derived from scratch hours later and turned out to be correct as
written. The artefact discarded here is a correct fix, discarded with evidence in hand, which makes
this more expensive than a test that passes for the wrong reason.

That rule has a second edge, and it is the mechanism that catches you while you are being
careful: **a falsification has two steps, and only one of them announces failure.** Applying the
revert is loud — the suite goes red and you read the message. Restoring afterwards is silent, so a
restore that does not restore looks exactly like a fix that does not work. `git checkout -- <file>`
restores from the *index*, and a revert applied with `git checkout <commit> -- <file>` is staged
there, so the "restore" puts the broken version straight back and the suite stays red. The reading
that follows is "my fix does not work", and the artefact discarded is again a correct fix. Verify
the restore the same way you verify the revert: `git diff` against the **commit**, not against the
index, and rebuild before believing the second result.

**And the version with no signal at all is a TIMEOUT, which skips the restore entirely.** Measured
2026-09-10, falsifying the scheduler fixes in cleat#1138. The falsification ran as one command —
mutate, `go build`, `go test`, `cp <backup> <file>` — and the `go test` leg exceeded the harness's
two-minute limit. SIGTERM, so the `cp` never ran and the tree kept the mutation. Nothing announced
it: the output ended with the falsification's evidence, which is exactly what a *successful*
falsification looks like.

**And the failure that ran alongside it taught the larger lesson, by fooling me twice.** A full
suite was running against that tree, and reported a **package-level `engine` failure with zero
test failures**. I read that as the mutation's doing — the signature matches, and `engine_test`
imports every plugin, so a plugin that does not build takes `engine` down with it. That mechanism
is real; I reproduced it deliberately in a separate worktree, appending one non-compiling function
to `plugins/scheduler/background.go` and running a match-nothing `-run` in `engine/`:

| | |
|---|---|
| fail events **with** a `"Test"` field | **0** |
| fail events **without** one | **1** — `github.com/cleat-team/cleat/engine` |

**It was not what happened.** Re-run on a clean tree with no mutation anywhere, `engine` failed
the same way again, and the run's own output says why:

    panic: test timed out after 25m0s
    FAIL    github.com/cleat-team/cleat/engine      1500.883s

Both suite runs were **timeouts**, and `grep -c 'build failed'` over either one returns **0**. The
mutation was innocent, and the reproduction I had performed corroborated a conclusion it did not
support — a mechanism that *can* produce a signature is not evidence that it *did*.

**So the rule this file already gives is necessary and not sufficient.** "PACKAGE failures must
also be 0" is right, and a package-level failure with zero test failures has **at least two**
causes that the count cannot separate:

| cause | what it means | what to do |
|---|---|---|
| the package did not build | a real breakage, possibly in another package | fix the code |
| the package ran out of time | says nothing about correctness | raise `-timeout`, or split the run |

They are indistinguishable in the three numbers, and the suite grows, so the second gets more
likely over time on a `-p 1` run over several packages. **Read the package's own output before
concluding anything** — one names `build failed`, the other `panic: test timed out`:

    grep -c 'build failed' /tmp/t.json          # non-zero: something did not compile
    grep -o 'panic: test timed out after [0-9a-z]*' /tmp/t.json | head -1

Both were checked against a known case of each, which is the only way to know a discriminator
discriminates: on the timed-out run, `build failed` is 0 and the timeout line is present; on the
deliberately-broken-build run, `build failed` is 1 and the timeout line is empty.

Two rules follow. **Check the restore by CONTENT, as its own step** — never as the last clause of
a long command:

    diff -q <backup> <file> && echo restored || cp <backup> <file>

And **do not run a suite and a falsification against one working tree at the same time.** Use a
`git worktree` for whichever is the longer of the two.

**And the version of that with NO signal at all: `git stash` on a clean tree stashes nothing and
exits 0.** Measured 2026-09-08, reproduced independently in a second clone:

| | |
|---|---|
| working tree clean | yes |
| `git stash` exit status | **0** |
| stash entries before / after | **0 / 0** |

So the `stash` → `checkout` → `checkout back` → `stash pop` sequence, used to run a test on another
branch, pops **whatever was already on the stack**. Here that was a parked WIP from another branch:
86 files and 1987 deletions applied onto an unrelated feature branch, with no conflict, because
there was nothing to conflict with. The `git stash drop` that followed then dropped *that* entry,
correctly and as documented — `stash@{0}` is exactly what it says it drops.

This is the paragraph above one layer up, and **strictly worse, because the failure is silent by
design**. A `checkout --` restore that does not restore at least leaves a tree someone may notice.
A stash that stashed nothing leaves nothing to notice at all.

**Check that the push created an ENTRY, not that the command succeeded:**

    before=$(git stash list | wc -l)
    git stash push -m "why"          # read its output: "No local changes to save"
    [ "$(git stash list | wc -l)" -gt "$before" ] || echo "nothing was stashed"

Recovery, if it has already happened: `git fsck --unreachable | grep commit`, match the stash by its
message, and `git stash store -m "<original description>" <sha>` puts it back. Untracked files the
stash carried reappear in the working tree — move them aside rather than deleting them, since they
belong to whoever parked the stash.

**The general rule, which this file now records three instances of: the operation reports success
without doing the thing.** Gating on "no pending checks" rather than a check *total*; reading a
background job's output file rather than its completion; and this. **This is the sharpest of the
three, because success is the CORRECT report** — nothing went wrong, and nothing happened.

**A probe that does not fire is a measurement, not a dead end.** Chasing the same defect, a
temporary print in `recordEvent`'s persist branch never printed while rows were demonstrably being
written. That was read as a failed experiment; it was in fact the strongest available signal —
the writer was somewhere else entirely, which was true and load-bearing. An absence is data.

**When output disagrees with expectation, instrument the function that produced it.** The same
defect survived five hypotheses, each argued from reading the code and each killed by measurement:
a field set after the checksum, missing columns, the chain, a late response overwriting the row,
the batch path. What resolved it was one `fmt.Fprintf` inside `computeEventChecksum` printing its
own inputs and result, which showed the writer chaining from `prev=""` in one line. Reading code
predicts which input differs; instrumenting shows it.

**Watch which layer is holding the test up.** An assertion can pass because of a layer other than
the one under test: a fence test passed with its SQL guard deleted because a Go-level rollback
covered for it; a cross-tenant assertion passed against a wide-open security policy because the
store's own SQL carried `tenant_id = ?`. Break the specific layer and watch.

**The sharpest form is a test whose NAME asserts the mechanism.**
`TestFinalizeDeferPhaseIsFencedOnTheClaimAndOnTheMarker` (§3.112) stayed green with the marker
predicate deleted. What refused the repeated finalize it pointed at was the finalize clearing
`assigned_to`, so the ordinary fence no longer matched — the predicate in the name had nothing to
do with it. Three cases in one test, three different things doing the refusing, and the name
attributed all three to one.

**And when a falsification comes back green, the obvious repair is usually the wrong one.** "It
did not go red, so the line is dead code" would have deleted a predicate that has a real case:
a claimed workflow that owes no defer phase, where the fence *is* satisfied and only the marker
stops a `status = NULL` write over a running workflow. Once that case was written, the same
falsification failed — with a NOT NULL violation where `ErrFenceLost` was expected, which is a
refusal by database constraint rather than by the code under test. **A falsification that stays
green is telling you which case you did not write, not which line to remove.**

**There is a second reading of a green falsification, and it costs you a change rather than a
test: the fix was never needed.** Measured 2026-09-10 on cleat#1138. Reverting a
`now()` → `SYSUTCDATETIME()` "fix" in a plugin left every test green. The first suspicion was that
the mutation had not applied — it had. `plugin.Rebind` already rewrites `now()` for MSSQL, so the
fix was redundant and the causal story written around it was wrong.

The rule above sends you looking for a case you did not write. That is right when the reverted line
is load-bearing and wrong when it is not, and **the green alone cannot tell those apart.** Ask
both: *which case have I not written*, and *what already handles this*.

The second is answered by reverting **one part of a multi-part fix at a time**. Reverting only the
`now()` passed; reverting only the `enabled = true` failed. That pair identified a single real
defect inside what had been committed as two, and no whole-fix revert could have — reverting both
together goes red, which reads as confirmation of the whole thing.

**The cause of that one generalises past SQL: a scan of source text cannot see a rewrite applied at
runtime.** The defect list came from grepping SQL literals for non-portable constructs, and 80 of
them were `now()` — every one already rewritten by `Rebind` before reaching a database. The literal
is not the artifact; the string that is *executed* is. Same shape as the "tool applied to a format
it does not model" table below, with the twist that the format was read correctly and the wrong
**pipeline stage** was measured. Where a rewrite layer exists, run the check on its output.

**When you fix something, fix the prose that describes it — not just the status marker.** A ✅ on
a heading over a stale body is worse than no marker at all, because it stops the next reader from
checking. Four separate sessions were lost to one sentence describing a build tag that had
already been removed; three of them concluded that a working feature was broken.

**Any number you write down carries a date and the command that re-derives it.** If you cannot
write the command, do not write the number. Every count in this repo's docs was wrong when
checked — linter totals, finding counts, skip counts, branch counts, all of them.

**And the ones that rot are a PREDICTABLE class, not bad luck: a count of a growing
population is guaranteed to be wrong.** Measured across this file on 2026-09-08 (cleat#986),
using the commands the file itself published:

| claim | as written | re-derived | |
|---|---:|---:|---|
| tables `CleanupPostgresTestData` deletes | 11 | **15** | drifted |
| skips when all three dialects run | 4 | **7** | drifted |
| engine exports | 50 | **52** | drifted |
| `IMPROVEMENT-PLAN` §3.x headings | 99 | **235** | drifted |
| the `comm -3` difference between the two export derivations | six | **6** | **held** |

Four of five wrong, and the fifth names the mechanism. The one that held is a **set
difference** — three workflow-API names against three plugin names. It describes a
relationship in the design, so it cannot drift as the tree grows. The four that rotted are
censuses: tables in a list, skips in a suite, exports in a file, headings in a document. Each
grows with ordinary work.

**So a count of a growing population is a proxy for a predicate, and the predicate is what a
reader needs.** The skip line is the clearest case: "4 means all three dialects ran" was true
on one day, and today it makes a clean run look broken. *"No remaining skip is
dialect-gated"* was true at 4, is true at 7, and will be true at 12. Replace the census with
the question it stands for, keep the command, and drop the number. Three such replacements
are in this file — the skip check, the cleanup-table warning, and the unmarked-heading scan —
and each is written out where it is used rather than summarised here.

**And when the value moves faster than a reader arrives, carry ONLY the command.** A date
plus a command is enough for a fact that changes monthly. It is not enough for one that
changes hourly, because the reader takes the number and skips the command — that is what
the number is *for*. Two instances, both measured 2026-09-06, both by the author of this
paragraph:

  * The engine's export count moved **three times in one day** — 58 → 52 when #767 removed
    the durable-state family, → 50 when #843 removed the two inert signal calls. Each value
    was correct when measured and stale within hours. Two of the three were written into
    documentation before they aged out.
  * `finalize_workflow_status` is `CREATE OR REPLACE`d by four migrations. A design doc
    cited 004 as authoritative, which had been true that morning; 043 (#844) and 044 (#846)
    both landed the same afternoon, so it shipped four behind.

**The rule this generalises — and it is the one that failed, not the number rule: a live
query has to be RE-RUN, not remembered.** The *Project state* section below already says to
find the highest-numbered migration that defines a routine before concluding anything. That
was done. The answer was then written down, which converted a live query into a stale fact
and reintroduced exactly the defect the instruction exists to prevent. **Running a check
once and recording its output is not the same as having the check.** Where the answer moves,
publish the query and let the reader run it:

    # not "004_fix_finalize_workflow_status_fence.sql", but:
    python3 - <<'EOF'
    import re, glob, os
    def strip(s):
        s = re.sub(r'/\*.*?\*/', '', s, flags=re.S)
        return '\n'.join(re.sub(r'--.*$', '', l) for l in s.split('\n'))
    for f in sorted(glob.glob('migrations/postgres/*.sql')):
        if re.search(r'CREATE\s+(OR\s+REPLACE\s+)?(FUNCTION|PROCEDURE)\s+\S*<routine>',
                     strip(open(f).read()), re.I):
            print(os.path.basename(f))     # the LAST line is authoritative
    EOF

Strip comments first, or a header quoting a `CREATE` counts as a definition — the same
"a text search cannot tell a thing from a sentence about the thing" trap as *Build* above.

**And the command has to answer the question you think it does.** A command that runs clean is
not a command that is right. `git log --date=iso` prints a *local* time with an offset —
`2026-09-03 14:20:48 -0400` — and pasting that clock reading into a UTC comparison moves the
window four hours:

    gh run list ... --jq '[.[] | select(.createdAt > "2026-09-03T14:20:48Z")]'   # wrong by 4h

`gh` compares strings, so nothing errors; it silently answers about a different window. That one
inflated a measured count from 5-of-24 to 11-of-36 and the conclusion from "a fifth" to "roughly
a third", in the direction that flattered the finding — which is why it was not questioned. Use
`%cI`, which carries the offset, and convert:

    git log -1 --format=%cI <sha>                    # 2026-09-03T14:20:48-04:00
    python3 -c "import datetime,sys; print(datetime.datetime.fromisoformat(sys.argv[1])
      .astimezone(datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ'))" "$(git log -1 --format=%cI <sha>)"

**What caught it was checking the story, not the number.** The mechanism being claimed — eviction
from a queue — can only produce runs with zero jobs, and six of the eleven had one job. A number
that supports your conclusion is the one to re-derive, not the one to keep.

**And `grep` in an interactive shell here is not the `grep` your script gets.** It is a shell
function wrapping **ugrep 7.5.0**; a script with `#!/usr/bin/env bash` gets `/usr/bin/grep` (BSD),
and CI gets GNU grep. Confirm with `type grep`, which reports the function, then `grep --version`.

Two independent divergences, both measured 2026-09-04, same directory and same `LANG`:

**1. `-c` combined with `-o` means different things.** ugrep counts *matches*, BSD grep counts
*lines*. On `§[0-9]+\.[0-9]+` over `IMPROVEMENT-PLAN.md`:

| | ugrep | BSD |
|---|---|---|
| `grep -oE ... \| wc -l` | 819 | 819 |
| `grep -cE ...` | 731 | 731 |
| `grep -coE ...` | **819** | **731** |

The pattern is not the problem and neither tool is wrong; `-co` is simply underspecified. Write
`-o \| wc -l` when you mean matches and `-c` when you mean lines, and never combine them. No
tracked script or doc currently does (`grep -rn 'grep -[a-z]*c[a-z]*o\b' --include='*.sh'`).

**2. A multibyte character inside a bracket expression parses differently.** On
`IMPROVEMENT[- ]PLAN[^§0-9]{0,3}§?[0-9]+\.[0-9]+` over `--include='*.go'`: ugrep **399**,
`/usr/bin/grep` **1379**, Python `re` **1379**. The interactive tool is the outlier, which is the
wrong way round — every number derived by hand is measured with the one that disagrees.

Plain-ASCII patterns were checked rather than assumed, and agree: `^### [0-9]+\.[0-9]+ `,
`^\| [0-9]+\.[0-9]+ \|`, and a `✅|FIXED|DONE` alternation all return identical counts under both.

So the rule above — write the command that re-derives the number — needs one more clause: **run it
the way the reader will run it.** For any pattern with non-ASCII or `{n,m}`, check it under
`bash -c '...'` before writing the number down, or compute it in Python, whose `re` is the same
everywhere. A guard that greps for something exotic should not be a shell script at all.

This surfaced as a script reporting 1627 citations where the identical pipeline pasted into the
terminal reported 546 — and neither was right. A survey in Python found 2354, because the pattern
missed four of the six forms a citation actually takes. Three tools, three answers, one command.

**Search the tracker before writing a fix for a defect you found by reading code.** Not before
starting to look — before starting to *build*. On 2026-09-08 I found the API-created-schedule
`next_run_at` bug by reading `handleCreateSchedule`, fixed it, and merged it as #998. It was
already cleat#995, filed 30 minutes earlier by another session that had a verified fix in hand and
claimed it three minutes after my merge landed. One `gh issue list` would have caught it.

Claiming on the issue is the other half and it only works if the next person reads it first, so the
order is: search, claim, then build. **Claim outright or not at all** — "I might pick this up
later" has produced duplicate work here more than once.

**One PR, one thing.** Every PR that bundled a second concern was harder to review than the two
would have been apart.

**Never merge on red, and never re-run a failing job hoping it passes.** Read the log. If it is
genuinely infrastructure, say so out loud and re-run *that*.

**Branch prefixes are exactly** `feature/`, `bugfix/`, `fix/`, `docs/`, `release/`, `hotfix/`.
Not `feat/`, not `test/`. `Validate branch name` fails the whole PR, and a PR's head branch
cannot be renamed, so each mistake costs a close-and-reopen.

**Ask whether the answer is a sweep or a mechanism.** A backlog of 200 similar findings is
usually one missing abstraction, not 200 fixes. Four real defects have come out of the ABI
layer's integer-conversion sites and none of them was an overflow — in every case the value meant
the wrong thing on one side of the boundary, which a property test over that boundary would find
faster than reading the remaining sites.

---

## Repo structure

- `cmd/` — CLI entrypoints (cleat, cleatctl, cleat-worker, cleat-bench, cleat-gen,
  cleat-gen-plugin, cleat-plugin-verify, deploy-workflow, wit-rewrite)
- `engine/` — Core engine: workflow execution, host functions, DB backends (~174 files)
- `wasm/` — WASM build, module loading, and codegen
- `wasmrw/` — WASM read/write helpers (small; production code duplicates this inline)
- `plugin/` — Plugin runtime and interface
- `auth/` — Tenant and auth stores
- `pluginapi/` — Public re-exports for external plugin authors
- `internal/` — Non-public support packages (analyzer, callgraph, closure, plugingen,
  telemetry, transform)
- `cleat/` — Public Go API (cleattest, embedded, localdev, wasmtest, ai, backendkit)
- `plugins/` — 21 built-in plugins (llm, slacknotify, pagerdutyalert, scheduler, etc.)
- `web/` — Svelte 5 admin dashboard
- `crates/` — Rust SDK + Java SDK
- `python-sdk/` — Python SDK
- `packages/` — AssemblyScript SDK
- `examples/` — Example workflows
- `tests/` — Integration test suites (cluster, cross-language, integrity, plugin-harness,
  scale, soak, upgrade)
- `benchmarks/` — Go benchmarks + comparative Temporal/DBOS benchmarks

> **Note on paths in older commits and branches.** Commit `3eeb74e` (2026-06-01),
> "promote internal packages to public — engine as a library", moved `internal/host/` →
> `engine/`, `internal/wasm/` → `wasm/`, `internal/plugin/` → `plugin/`, and
> `internal/wasmrw/` → `wasmrw/`. Anything referring to those `internal/` paths predates
> that commit. Branches based before it will not merge cleanly.

---

## Build

- Go 1.25+, module `github.com/cleat-team/cleat`
- WASM workflows are compiled with the standard Go toolchain (`--target go`, default)
- Tests use `go test`, fuzz tests, and behavioral test suites

### One WASM backend, and a wazero runtime that is not it

**wasmtime** (`engine/backend_wasmtime.go`, `//go:build cgo`) is the only `WasmBackend` cleat
has. `engine.WasmtimeLanguages` is the single source of truth for which guest languages run on
it, and membership there means *verified to load and execute*, not *ought to*.

There is no second backend and no fallback. `engine/backend_wazero.go` was **deleted** in #459
(2026-08-10) — this file described it as "the CGO-less fallback" for twenty days after it
stopped existing. Confirm with `ls engine/backend_wazero.go`.

**wazero has not left the tree, though, and the distinction matters.** `engine.Runtime`
(`engine/runtime.go`) is still a wazero runtime, and it still executes guest code on these
paths:

- `cleat/wasmtest`, `cmd/cleat run_embedded`, `cmd/cleatctl replay`, `cmd/cleatctl debug`,
  `cmd/cleat-bench` — all call `engine.NewRuntime`. **Dev and CLI tooling, all of it.**
- `RunDefer`, but only when the engine registers *no backends at all* — which is those same
  tools. The worker calls `RunDefer` too (`cmd/cleat-worker/setup.go:480`) and never reaches
  this path, because it registers wasmtime.
- `RunDeferCompiled`, which is wazero by signature — it takes a `wazero.CompiledModule`, so no
  backend can serve it. **It currently has no callers**, and is listed in
  `scripts/deadexports-baseline.txt`.

Re-derive: `grep -rn "NewRuntime(" --include="*.go" . | grep -v _test.go`

**This section used to describe the `RunDefer` bullet as an unfenced hazard, quoting a comment
that said "the CGO-less build, where wazero is the only runtime there is. Unfenced, and
unavoidably so" at `engine/executor.go:706`.** Every part of that was wrong by 2026-09-01: the
comment had already been rewritten in the tree, line 706 is now unrelated `continue_as_new`
code, and #503 closed the case that made it dangerous. Confirm the quote is gone with
`grep -n 'CGO-less build, where wazero is the only runtime' engine/executor.go`.

What made it dangerous was **guest-controlled**: `wasm.DetectLanguage` returns the guest's own
`cleat.metadata` Language field verbatim, so a module declaring `"tinygo"` — or `"GO"`, since
the lookup is exact — matched no backend and fell through to wazero. `Engine.resolveBackend`
now fails closed on exactly that, distinguishing "this engine does no routing" from "this
engine routes but not for this language". Read its doc comment for the measurements.

So "wazero is gone" is still wrong, but the reason has changed. **wazero cannot be fenced for a
compute-bound guest** — measured three ways, all failing: `WithCloseOnContextDone` breaks all
execution, fuel only decrements on function entry, and closing the module has no effect on a
tight loop. That now bounds *developer tooling*, not anything a worker runs: a runaway guest
under `cleatctl replay` or `cleat run` is not stopped.

**"wazero removal, part 2" was decided against on 2026-09-01 — do not start it.** See
IMPROVEMENT-PLAN.md §3.56. The safety case was gone once #503 made routing fail closed, and
removal would force `cleat` onto CGO (ending pure-Go cross-compilation for the CLI) while
breaking exported API — `engine.Runtime`, `engine.NewRuntime`, `wasmtest.WasmTestEnv.Runtime()`.
The parked WIP stash and the `REMEDIATION-PLAN-2026-08-09.md` section describing it are
superseded.

The price of keeping two implementations is that something must compare them, because the host
ABI is written twice — `engine/imports.go` for wazero, `engine/wasmtime_hostfuncs*.go` for
wasmtime. `engine/hostabi_runtime_parity_test.go` does. It found a real defect on its first run:
`cleat_create_promise` was registered on wasmtime with a parameter no guest passed, so durable
promises could not link on the worker at all (§3.55). **Note what a name-only comparison would
have said** — both sides register the same names, and did then too.

**This paragraph described a gap that no longer exists, and that is the more expensive kind of
error than a stale number.** It read: the test filters both sides on the `cleat_` prefix
(citing `engine/hostabi_runtime_parity_test.go:114` and `:332`), so it compares 55 names while
`plugin_call`, `plugin_call_streaming` and `set_query_state` are *never compared*.

All of that was true, and stopped being true on 2026-09-05 when the filter was removed. `:114`
is now a blank line and `:332` an unrelated `switch`. A session reading this would go to close a
hole that is closed — and the paragraph was persuasive precisely because it was specific.

The textual check is not what settles it, and that is worth noting here because it is this
file's own trap: `grep -c 'strings.HasPrefix(name, "cleat_")'` over that test returns **2**,
which reads like a live filter and is two comments *describing* the removed one. What settles
it is behavioural — adding a real filter makes the test fail, so a filter cannot already be
there.

**What replaced it is a test, which is why this is worth reading as a pattern rather than a
correction.** `TestParityCoversEveryRegisteredHostFunction` fails if the filter returns in any
form. Verified 2026-09-08 by putting a `strings.HasPrefix(name, "cleat_")` back:

    the runtime parity check does not compare 3 of 52 registered host functions:
    plugin_call, plugin_call_streaming, set_query_state

A prose warning about a guard's blind spot rots the moment someone fixes it, and rots *silently*,
because nothing re-reads the prose. A test that asserts the blind spot is absent cannot: it goes
red when the blind spot returns, and it stays green — saying nothing, correctly — when it does
not. **Prefer converting a gap into a failing test over describing it here.**

**The two counts still answer different questions**, and that part survives: the engine exports
52, of which 49 carry the `cleat_` prefix and three do not. The parity test now compares all 52 —
it no longer compares a subset, which is what the deleted paragraph got wrong.

**This paragraph said 58 and 55 until 2026-09-06, and it is the sharpest example of its own
rule.** The section exists to warn that a count in prose rots, and its count rotted: six exports
went with the durable-state family (§3.216), and two more with the inert signal calls (§3.220) a
few hours after this very paragraph was corrected to 52. It then rotted a third time, upward:
`cleat_poll_update` and `cleat_complete_update` arrived with workflow updates (#868), so the run
is 58 → 52 (#767) → 50 (#843) → **52** (#868) — four values in three days, every one correct when
written, and the section above still said 50 on 2026-09-08. Note the direction: the first two
were removals and the third an addition, so "the number only goes down" is not available as a
sanity check either. Nothing failed any of the three times, because **no test asserts these
numbers**. `ABI.md` stayed correct over the same period —
it and `engine/imports.go` agree on all 52 with an empty set difference — so the drift was in this
file alone. That is no longer luck: since #952, `scripts/check-doc-consistency.sh` compares the
two SETS on every CI run and fails on either difference, which is the same "convert it into a
failing test" move as the parity filter above. Nothing yet checks the numbers in *this* file, so
re-derive before quoting, including from here.

    grep -oE '\.Export\("[^"]+"\)' engine/imports.go | sed 's/.*Export("//;s/")//' | sort -u | grep -c .

**Do not put `cleat_` inside that pattern when you want the export total.** Three exports are
unprefixed — the same three above — and all three are workflow API, so a prefix-anchored scan
silently drops the plugin calls. A pattern that can only return names of the shape
it assumes cannot test the assumption; it encodes the conclusion. That error produced a **55** on
2026-09-05 whose digits matched a differently-derived 55 — the workflow-facing target, the export
total less two handshake calls and one deliberately unbindable one — with **six** members
different, three in each direction.

**The collision is not a coincidence of that one day: it recurred at 49, and again at 47 within
hours**, both derivations moving together as exports were removed, still differing on the same six
members.
Which is the point — the number agreeing tells you nothing, and will keep telling you nothing. Two derivations agreeing on a number while disagreeing on more than a tenth of its
membership is the 876/581/4 costume from this file's opening section (IMPROVEMENT-PLAN §3.213).
Re-derive the difference rather than the totals — this runs, and prints six lines:

    E=$(grep -oE '\.Export\("[^"]+"\)' engine/imports.go | sed 's/.*Export("//;s/")//' | sort -u)
    comm -3 <(grep '^cleat_' <<<"$E") \
            <(grep -v -e '^cleat_poll_work$' -e '^cleat_complete$' \
                      -e '^cleat_register_query_handler$' <<<"$E")

The six are `cleat_poll_work`, `cleat_complete` and `cleat_register_query_handler` on one side,
`plugin_call`, `plugin_call_streaming` and `set_query_state` on the other. **Compare the sets, not
the counts** — the same instruction as "diff the SET rather than comparing counts" under the
`-list` rule, and this is what it looks like when nobody does. **One prefix assumption produced
all three errors above**: a wrong denominator, a stale doc number, and a guard that has never
compared three of the names it exists to compare.

**"Which backend runs this" and "which code path inside that backend runs this" are different
questions.** The wasmtime backend has **two** execution paths — core module and native
component — and they had different answers about limits until IMPROVEMENT-PLAN §3.31 wrote the
story down for each. Tell the limit story about the second question, not the first.

There were three. Decomposition was deleted in #528 (2026-09-01) after being measured against
the only Component Model binary in the repo: the native path reached CPython and ran guest
code, while decomposition failed at instance 81 of 85. A second, mirror implementation on
wazero failed at instance 8. Confirm with `grep -rn "func.*ExecuteComponent" --include="*.go" .`
— exactly one line, `ExecuteComponentCGo`.

**Two things went wrong writing that one-line command, both worth the warning.** The first
version was `grep -rn "ExecuteComponent\b"`, which returns a hit: a comment in
`engine/wasmtime_options.go` explaining that the function it names was deleted. A grep a
*retraction* satisfies is the §1.1 trap, in a file that documents the §1.1 trap. The second was
`func.*ExecuteComponent(` — anchoring on the open paren, which does not follow the name in
`ExecuteComponentCGo(`, so it matched nothing at all and the "only X should match" claim beside
it was false in the other direction. **Run the command and read its output before writing the
sentence about what it prints.**

---

## Plugin development

Fuller guidance lives outside this repo at
`cleat-internal/prompts/plugins-and-apps-guidance.md`. That checkout is not present on
every machine — if it is missing, the conventions below are sufficient to start; do not
spend turns hunting for it.

Key conventions:
- Plugin names are hyphenated: `"slack-notify"`, `"email-notify"`, `"pagerduty-alert"`
- HostCall operations are snake_case: `"send_message"`, `"trigger_incident"`
- Plugins share the main go.mod (no separate go.mod per plugin)
- Study `plugins/slacknotify/` and `plugins/scheduler/` as reference implementations
- The Plugin interface is in `plugin/plugin.go`

---

## Project state

- **`tiers.yaml`** — what is supported, and at what tier. Start here.
- **`IMPROVEMENT-PLAN.md`** — the item backlog. Each `§` heading carries a status marker; the
  marker is the source of truth, not any summary table derived from it. Read the body too — it
  has been stale under a fixed heading more than once.

  **A heading with *no* marker is a defect, and it costs more than a wrong one.** §1.1 and §1.2
  — the two highest-severity items in the document — carried no marker until 2026-09-01 while
  their bodies had recorded the fixes as done for weeks. A scan for open work reported a
  closed data-loss bug as the project's top outstanding item, and a session went into
  re-deriving what the body already said. "No marker" is indistinguishable from "not started",
  so it is read as the latter. Prose in the heading (`— fixed in 9fc2a81`) counts; nothing at
  all does not.

  **The predicate is "every heading carries a status", not a ratio.** This said "87 of 99" on
  2026-09-01; it is 235 headings now, so both halves were wrong within days and the ratio told
  a reader nothing they could act on. What is actionable is the list of headings with no status
  at all, which is short and is the thing to fix:

      python3 - <<'EOF'
      import re
      hs = [l.rstrip() for l in open('IMPROVEMENT-PLAN.md')
            if re.match(r'^### [0-9]+\.[0-9]+ ', l)]
      st = re.compile(r'[\U0001F300-\U0001FAFF✅❌⬜⚪]|—\s*(?:\*\*)?\s*'
                      r'(?:fixed|done|open|wontfix|declined|superseded|parked|deferred|'
                      r'partly|partially|core fixed|shipped)', re.I)
      for l in hs:
          if not st.search(l):
              print(l[:110])
      EOF

  Two on 2026-09-08. Note the emoji class has to be a RANGE rather than a list: writing out
  the markers you have seen misses the next one someone uses, and a scan that silently stops
  matching reports zero unmarked headings — which reads exactly like success.

  **When a section names the files it will change, those names go stale too, and in the
  direction that fools you.** §1.1's `Files:` bullet pointed at
  `migrations/*/003_procedures.sql`. The fix shipped as `004_fix_finalize_workflow_status_fence.sql`,
  which *redefines* the procedure — so 003 still contains the original unguarded body, exactly
  as the bug report described. Checking the claim against the file the claim named confirmed
  the bug, and the confirmation was worthless. For anything defined by `CREATE OR REPLACE`,
  find the highest-numbered migration that defines it before concluding anything.
- **`WORKSTREAM.md`** — what is being worked on now, by whom, and in which sandbox.
- **`BRANCH-TRIAGE.md`** — assessment of unmerged remote branches. Its method
  (`git rev-list --left-right --count`) cannot see through a squash-merge, so it has reported
  already-merged branches as outstanding. Re-derive with
  `git merge-base --is-ancestor <PR-merge-commit> develop` before acting on it.
