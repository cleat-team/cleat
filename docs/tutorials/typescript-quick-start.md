# TypeScript quick start

> **Status: tier 2.** (`tiers.yaml`.) This page describes what exists and was measured. The known differences from Node are
> listed in `packages/cleat-ts/DIVERGENCES.md`, and the limits below are not a complete list.

Cleat runs a TypeScript workflow as a WASM module: your code runs on QuickJS (through Javy, in static mode) inside the
same engine that runs Go, Rust, Python, Java and AssemblyScript workflows. The module is about 1.45 MB, almost all of it
the JavaScript engine (the example below built to 1,446,426 bytes on 2026-11-17; the Javy plugin it embeds is 1,296,266 of that).

## What has and has not been run

| | Measured | Where |
|---|---|---|
| Build chain, harness, worker tests | Linux and macOS arm64 | `.github/workflows/typescript-tier2.yml` |
| **PostgreSQL** | yes | the `TypeScript Tier 2 (Linux x86_64)` job |
| **MySQL** | yes, the database-backed tests only | the `TypeScript Tier 2 (Linux x86_64, MySQL)` job |
| **SQL Server** | yes, the database-backed tests only | the `TypeScript Tier 2 (Linux x86_64, SQL Server)` job |

"The database-backed tests only" is a real limit: the MySQL and SQL Server jobs run the tests that start a worker against a
database (`TestTypeScriptRunsAllThreeSuspensionsOnARealWorker`, `TestTypeScriptRunSurvivesAKillInTheMiddleOfExecution`,
`TestTypeScriptDeferSegmentRunsOnARealWorker`), not the whole package, because the rest never touch a database.
`TestTypeScriptEveryDatabaseBackedTestRunsInBothDialectJobs` fails if a database-backed test is added and one of those jobs does
not run it. These jobs are **tier 2: they must run and may fail, and they gate no merge**.

Cleat treats multi-database support as a portability discipline. What is measured here is what those three tests do on each database; nothing else about the guest was run on MySQL or SQL Server.

## 1. Prerequisites

Go, Node, and a Rust toolchain with the `wasm32-wasip1` target. Then, from a checkout, install the pinned Javy and esbuild:

```
packages/cleat-ts/bootstrap.sh
```

It verifies each download against `packages/cleat-ts/toolchain.lock` before running it, and exits 2 (`UNMEASURED`) on a
platform it has no pin for. The pins cover macOS arm64, Linux arm64 and Linux x86_64 only.

## 2. Write a workflow

`examples/ts-workflow/workflow.ts`:

```ts
import { workflow } from "@cleat/ts";

export const greet = workflow(async (ctx, input: { name: string }) => {
  const composed = await ctx.call<{ text: string }>("greeter", "compose", { name: input.name });
  const audited = await ctx.call<{ id: string }>("audit", "record", { text: composed.text });
  return { greeting: composed.text, auditId: audited.id };
});
```

Everything durable goes through `ctx`: `ctx.call`, `ctx.sleep`, `ctx.waitForSignal`, `ctx.child`, `ctx.now()`, `ctx.random()`.
The engine records each one, so a replay returns the recorded answer instead of repeating the effect.

The build refuses what would make a replay take a different path. A `setTimeout` is a build error naming the file, line and
global, and nothing is written (`TestTypeScriptCleatBuildTargetTSProducesTheModuleTheScriptDoes`). Use the `ctx` calls instead.

## 3. Build it

```
cleat build --target ts -o /tmp/out examples/ts-workflow/workflow.ts:greet
```

The argument is `file.ts:entry`; the entry is the exported name. The command prints that the target is tier 2 and not
supported. Outside a checkout, point `CLEAT_TS_DIR` at `packages/cleat-ts`.

## 4. Test it

`packages/cleat-ts/testing.ts` runs the **compiled module** (not your source) and returns what happened:

```ts
const run = runWorkflow(module, "greet", { name: "Ada" }, {
  answers: { "greeter/compose": JSON.stringify({ text: "Hello, Ada" }), "audit/record": JSON.stringify({ id: "a-1" }) },
});
```

You script the answers to your durable calls. **A call your script does not name is refused**, with an error that quotes the
missing key, rather than answered with `{}`; a workflow that passes on an empty object it never chose is the failure this
exists to prevent. `examples/ts-workflow/workflow.test.ts` runs both directions, and runs them against the module that the
build command above produced.

**What the harness scripts.** Durable calls (`answers`), promises (`promises`, `promiseRejects`), signals (`signals`) and
child workflows (`children`). A name your script does not cover is not invented: an unscripted promise stays pending, an
unqueued signal is not found, and an unscripted child stays running, so a workflow waiting on any of them **suspends** instead of
failing or quietly getting `{}`. Signals are delivered **before** the run starts; there is no mid-run delivery (`DIVERGENCES.md` H1).

**Updates, locks, fetch.** Updates are scripted with `scripts: { updates: [{ name, payload? }] }` and are all pending before the run starts, so a test covers a handler's answer, not an update arriving while the workflow is parked. A lock call without a `locks` script answers `false` (there is no store at all), and a `fetch` without a `fetch` script is **refused**, which is also a worker's default: configure the script you mean to test. A plugin call is answered from a scripted registry (`scripts: { plugins: { "plugin/function": { result | error | chunks } } }`), and `extras.pluginCalls` says what reached it. See the table in `packages/cleat-ts/testing.ts` for each store's fixture and what it reports.

## 5. Known limits

Each is recorded, with the test that holds it, in [`packages/cleat-ts/DIVERGENCES.md`](../../packages/cleat-ts/DIVERGENCES.md).

- **Every host call is bound or deliberately unbound; none is pending** (cleat#3827; checked 2026-11-18). Four are deliberately unbound, each with a `permanent:` reason in `packages/cleat-ts/host-calls.json`: `cleat_poll_work` and `cleat_complete` (the worker's own handshake), `cleat_register_query_handler` (not bindable from a guest) and `cleat_interrupt_requested` (polled by the plugin itself, never by a workflow). `TestEveryHostCallIsBoundOrListed` reads that file and `host-calls.d/*.json` and fails when a call is in neither list, so a host call added later without a binding turns it red. Re-derive it, reading both files: `python3 -c "import json,glob;u={n:str(r) for f in ['packages/cleat-ts/host-calls.json']+glob.glob('packages/cleat-ts/host-calls.d/*.json') for n,r in json.load(open(f)).get('unbound',{}).items()};print(len(u),'unbound; pending:',[n for n,r in u.items() if r.startswith('pending:')])"` should print `4 unbound; pending: []`.
- **`continueAsNew` with `options.version` starts the next run on that version, or fails the run** if it is not deployed (cleat#4041). It never continues on the current version instead.
- **A synchronous `ctx.fetch` throws a plain `Error`, not a `CleatError`**, which differs from the other bindings (a synchronous call cannot get the class).
- **A killed workflow's `ctx.defer` cleanup runs, except for a guest whose work never returns to the bytecode loop** (D1, cleat#3922, fixed in cleat#4053). The host asks the guest to stop through an interrupt poll before its hard limit, so the drain runs. A single native operation that never yields can still reach the hard trap, and its defers are lost.
- **A cancelled one runs its cleanup too, on a different path.** Cancelling a *suspended* workflow that registered a defer makes the worker run the cleanup, asserted
  through the worker's own outbound calls (`TestTypeScriptDeferSegmentRunsOnARealWorker`). The two paths differ, and which one
  your workflow takes depends on how it stops.
- A defer body that itself suspends stops the later bodies' calls (D2); no fixture covers it yet.
- **No timers, no `crypto`** (rows 4-5): `ReferenceError`, loud. **`TextEncoder`/`TextDecoder` are UTF-8 only** (rows 3, 7, 8): a `TextDecoder` for another encoding throws `RangeError`, and `TextDecoderStream`/`TextEncoderStream` are undefined.
- **Local time is UTC** (row 6).
- **Silent divergences from Node are the dangerous kind, and the list is not closed.** Row 1 (`Date` setters given a non-finite
  argument) is fixed and held by a 631-case comparison against Node. Row 2 (`URL`) is fixed **only for what the shim can answer
  exactly**: an IPv6 host, an internationalised host or a `file:` URL makes it throw, and a catch-all that swallows the throw, as
  zod's `z.url()` does, still reads a valid URL as invalid (cleat#3898). The list covers what was looked at: four npm packages,
  27 cases. There may be more.

## 6. Deploy it and run it for real

A worker runs the built `.wasm` like any other workflow module. Put the module where the worker will find it:

```bash
cleatctl -driver mysql -db "$CLEAT_DATABASE_URL" deploy workflow <name> <module.wasm>   # or postgres, or mssql
```

That works on PostgreSQL, MySQL and SQL Server. `-driver` and `-db` are global flags and go before the subcommand; `-driver` is
inferred from the shape of the database URL when you leave it out, and passing it is clearer. `deploy-workflow --driver
postgres|mysql|mssql --db <dsn> <name> <module.wasm>` is the standalone equivalent. Only `cleatctl deploy plugin` is still
PostgreSQL-only (cleat#4005). The full how-to, including the version and plugin checks, is
[deploy-workflows](../how-to/deploy-workflows.md).

`TestTypeScriptRunsAllThreeSuspensionsOnARealWorker`, `TestTypeScriptRunSurvivesAKillInTheMiddleOfExecution` and
`TestTypeScriptDeferSegmentRunsOnARealWorker` in `tests/typescript/` do this on a real worker, and the tier-2 CI jobs run them
against PostgreSQL, MySQL and SQL Server. On MySQL and SQL Server they deploy through `deploy-workflow`, so that command is exercised by CI
on those databases. `cleatctl deploy workflow` on them was measured by cleat#4004's own tests, not by these jobs. `CLEAT_TS_DIALECT` (cleat#3992) selects the database when you run them yourself.
