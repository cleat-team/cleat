# Integration hub — DBOS port (the wedge, DBOS side)

The DBOS counterpart to [`examples/integration-hub`](../integration-hub/)'s wedge, for
cleat#2597's second cleat-vs-DBOS pair. Unlike the [order-lifecycle
pair](../order-lifecycle-dbos-port/), which is the **control** (an honest,
equally-scoped comparison where DBOS is not worse), this pair exists to
locate a specific structural boundary: **DBOS has no primitive for accepting
a tenant's own code at runtime and executing it, sandboxed or not.**

**Read both pairs' claims as a set, not in isolation.** The order-lifecycle
pair's claim is that the comparison itself is honest. This pair's claim is
about where a real boundary sits. Neither is phrased as a win for either
side — a claim phrased that way would not survive the next release of
either project, and would not be worth writing down.

## The finding, checked against current `docs.dbos.dev` (2026-09-28)

Pages checked, with what each one actually says:

- `/architecture` — describes annotating workflows and steps in **your own**
  application. Nothing about accepting code from an external party.
- `/typescript/tutorials/step-tutorial`, `/typescript/tutorials/workflow-tutorial`
  — `DBOS.registerWorkflow(fn)` / `DBOS.runStep(fn)` both take a function
  **reference**, resolved when your process starts. There is no analogue of
  loading a module from a database row, an uploaded file, or any other
  runtime source.
- `/typescript/tutorials/queue-tutorial` — "multi-tenant" here means
  **partitioned concurrency** ("this fair queue runs at most one task per
  user"), explicitly a fairness feature. The page never mentions sandboxing
  or isolating what a queued task is allowed to do.
- `/typescript/tutorials/upgrading-workflows` — covers deploying a new
  **version of your own code** across in-flight workflows (patching vs.
  versioning). Not third-party code intake.

**So making DBOS express cleat's `POST /api/definitions` + sandboxed
`ChildWorkflow` at all needs two things DBOS supplies neither of:** a
code-upload/storage mechanism, and an isolation layer (a WASM runtime, an
isolate library, a container per tenant). See `ISSUES.md` for the full
citation trail.

## Why the scope had to narrow, and why this port does not fake an upload path

Building an upload endpoint into this port would attribute infrastructure
*this author wrote* to a platform capability DBOS does not have — the
opposite of what a pair is for. So `src/workflow.ts`'s `runTenantStep` does
not attempt one: it composes a "tenant step" the only way DBOS composes any
step, `DBOS.runStep` over a plain function that is already part of this
file at compile time. `stepName` selects one of the **operator's own**
functions, not a tenant's — there is nothing on the DBOS side resolving
tenant-supplied code, because DBOS has nothing that would run it if there
were.

This also means the scope is narrower than `examples/integration-hub/hub.go`'s
full `SyncCustomer` (webhook ingestion, connector dispatch, rate limiting).
Matching the cleat-side sandbox pair's own precedent
(`scripts/run-integration-hub-tenant-sandbox-scenario.sh`, which also skips
the webhook pipeline), this port is scoped to exactly the one property that
script measures: whether a "tenant step" reaches the host filesystem.

## The two steps, mirroring the cleat side's positive/negative control pair

- `normalizeOrder` — the legitimate transform, matching
  `examples/integration-hub/tenant-steps/normalize-order/main.go` at the
  same scope. **This is the mandatory positive control**, carried over from
  the cleat-side sandbox pair's own rule
  (`scripts/run-integration-hub-tenant-sandbox-scenario.sh`'s header:
  *"a FAIL on the adversarial arm alone would be worthless"*). Without a
  step that demonstrably succeeds, a failure on `readHostFile` would be
  indistinguishable from a DBOS runtime that fails every step.
- `readHostFile` — matches the *intent* of
  `examples/integration-hub/tenant-steps/malicious-read-host-file/main.go`:
  the simplest thing a tenant-supplied step could do to reach the host
  filesystem. It reads `/etc/hosts` rather than `/etc/hostname` — the latter
  does not exist on macOS, and an `ENOENT` there is indistinguishable from a
  refusal without reading the message, which cost a local test run while
  writing this port. `/etc/hosts` exists on every POSIX host this runs on.

Under cleat, `read_host_file` traps — `engine/wasi_policy.go` lists WASI's
`path_open` as `wasiFatal`, and `engine/wasi_policy_wasmtime.go` turns that
into a hard trap before the guest's own error-handling code ever runs.
Under DBOS, `readHostFile` **succeeds**.

## Why a trivially-true result is evidence about DBOS, not about Node

`fs.readFileSync` succeeding inside a DBOS step is unsurprising by itself —
Node has full host access by design, and nobody familiar with it expects
otherwise. **That observation is not what this pair is evidence for.** The
claim is about DBOS's platform *surface*: DBOS *could* have interposed
something between a step and the process it runs in — a wrapped executor, a
required isolate, a capability check — the way cleat's engine interposes
`engine/wasi_policy_wasmtime.go` between a guest and every WASI import. It
does not. Nothing in `DBOS.runStep`, `DBOS.registerWorkflow`, or anywhere
else in the SDK's public API touches what a step function is allowed to do.

So `readHostFile`'s success is evidence that the **platform's** contribution
to isolation is zero — not evidence about what Node can do. DBOS could have
built a boundary here; it doesn't, so the entire burden of isolating a
per-tenant step falls on the application author, every time, from scratch.
Cleat's boundary is built once, in the engine, and applies to every tenant
step forever.

## Reading this pair's exit codes — not the usual shape

`src/wedge.test.ts` and `scripts/run-integration-hub-dbos-scenario.sh` both
use:

    0   both assertions held: the positive control ran, and readHostFile
        succeeded exactly as documented above.
    1   A FINDING, not "the test failed" in the ordinary sense. readHostFile
        was refused or threw. That would FALSIFY the claim this file
        documents — meaning today's DBOS does not behave the way this pair
        says it does — and needs investigating (did the SDK change? did
        something OUTSIDE DBOS, e.g. a container's read-only filesystem,
        intervene?), never silently accepted or loosened away.
    2   UNMEASURED: the positive control itself did not hold, or the harness
        crashed before reaching a verdict. Neither says anything about the
        boundary this pair exists to measure.

**This is stated explicitly because `1` conventionally reads as "the test
failed."** A future reader who sees exit 1 and assumes a broken run, rather
than a possible change in DBOS's own behaviour worth re-checking against
current docs, has drawn exactly the wrong conclusion from this pair's own
design.

## Version and date

`package.json` pins `@dbos-inc/dbos-sdk` to `5.1.10` — the latest stable
release as of **2026-09-28**, confirmed with `npm view @dbos-inc/dbos-sdk
version` on the same day this port was written, and the same version the
order-lifecycle pair already pins. No drift between the two pairs to record.

## Build and run

```bash
npm install
npm run build
```

Needs `DBOS_SYSTEM_DATABASE_URL` (a plain Postgres connection string; DBOS
creates its own system-database schema there on launch):

```bash
DBOS_SYSTEM_DATABASE_URL=postgres://postgres:PASSWORD@127.0.0.1:5432/integration_hub_dbos \
  npm test
```

`scripts/run-integration-hub-dbos-scenario.sh`, at the repo root, is what CI
runs — it installs, builds, and runs `npm test`, translating its exit code
into the three-way status documented above.

## Why the totals below are not the whole comparison

**These file lists are not role-symmetric, and that asymmetry IS the
finding — not a flaw in the count.** Re-derive with
`scripts/dbos-pair-loc.sh integration-hub`, `cloc.py` 2.10:

| role | file | code lines |
|---|---|---|
| cleat: tenant step (positive) | `tenant-steps/normalize-order/main.go` | 31 |
| cleat: tenant step (adversarial) | `tenant-steps/malicious-read-host-file/main.go` | 18 |
| cleat: sandbox policy | `engine/wasi_policy.go` | 92 |
| cleat: sandbox enforcement | `engine/wasi_policy_wasmtime.go` | 32 |
| **cleat total** | | **173** |
| DBOS: workflow + steps | `src/workflow.ts` | 31 |
| DBOS: test | `src/wedge.test.ts` | 73 |
| **DBOS total** | | **104** |

This is a snapshot dated 2026-09-28; re-derive with the command above rather
than re-quoting these rows — CLAUDE.md's own rule about numbers in prose
applies to this table as much as to anything else in the repo.

**A smaller DBOS total here is not an efficiency win, and reading it as one
is the mistake this section exists to head off.** It is the same shape as
the order-lifecycle pair's *"Tenancy: 0 lines, and it is not a compliment"*:
124 of cleat's 173 lines are the enforcement code that makes a refusal real
— `engine/wasi_policy.go` declaring `path_open` as `wasiFatal`,
`engine/wasi_policy_wasmtime.go` turning that into a trap. Written once,
applied to every tenant step this engine will ever run. **The DBOS side has
no equivalent line to count, because nothing on that side enforces
anything.** A smaller total is what "zero isolation code" looks like in a
line counter, not what "better isolation" looks like.

## The counter

`scripts/dbos-pair-loc.sh` runs `cloc` identically on this directory's
`src/` and on the cleat-side files above, so both pairs' counts come from
one pinned invocation rather than two people using `cloc` slightly
differently.
