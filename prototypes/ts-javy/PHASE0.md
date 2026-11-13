# Phase 0: toolchain and bridge (prototype, branch `proto/ts-javy`, 2026-11-13)

**This is a prototype. It supports nothing, is not in `tiers.yaml`, and claims nothing.** Plan:
`cleat-internal/typescript-javy-prototype-plan-2026-11-08.md`. Raw output behind every measured line below is in
`evidence/phase0/verify-2026-11-13.txt`, produced by `phase0-verify.sh` against a fresh database.

## Result

**Exit criterion met: a `return {"hello":"ts"}` TypeScript workflow, deployed with `cleatctl`, completes on a stock
`cleat-worker` against PostgreSQL, with zero changes outside `prototypes/`** (`git diff origin/develop --stat --
. ':!prototypes'` is empty, section 7 of the evidence; base `e44bd9ae`). **Bridge (a) worked; bridge (c) was not needed.**

One condition, and it is a Q1 finding rather than a footnote: the engine routes on the module's own
`cleat.metadata.language` and accepts only the labels in `engine.WasmtimeLanguages` (`go`, `assemblyscript`, `java`,
`rust`, `python`). A module labelled `typescript` is refused by name (`engine/executor.go:83`, "no WASM backend
registered for guest language"). This module is labelled **`rust`**, which is the closest truth (its host-call layer
is `cleat-sdk`'s pattern, planned for Phase 1) and the only way to run with no engine change. A truthful label needs a
one-line engine change (`WasmtimeLanguages`); that is for the owner to decide at the end, not for the prototype to do.
`deferSegmentLanguages` (`engine/engine.go:906`) also lists `rust`, so the label makes the engine treat the guest as one
that decodes the suspend sentinel in a defer segment. *Inference from reading the code, not tested; Phase 1 must either
decode it or this label is wrong.*

## Versions (pinned in `toolchain.lock`)

| | |
|---|---|
| Javy | **v9.1.0**, released 2026-07-30; repository pushed 2026-11-12, not archived. Releases: v7.0.1 2025-10-06, v8.0.0 2025-11-12, v8.1.0 2026-03-22, v8.1.1 2026-04-06, v9.0.0 2026-06-12, v9.1.0 2026-07-30 |
| QuickJS | rquickjs-core 0.12.2, through the `javy` 8.1.0 crate |
| `javy-plugin-api` | 7.1.0 (the crate in Javy's own v9.1.0 tree) |
| esbuild | 0.28.2 |
| walrus | 0.26.4 (what Javy v9.1.0's own codegen uses) |
| rustc / wasm-tools | 1.96.0 / 1.261.0 |

## The plan's description of Javy, checked against today's docs and binary

The plan said these came from memory. Checked 2026-11-13 against the README, `docs/` at tag v9.1.0, the source at that
tag, and the v9.1.0 binary:

- **Exports:** a Javy module exports JS functions as `name: () -> ()`, named by a WIT world; "exported functions with
  arguments ... are not supported. Return values will also be dropped" (`docs-using-exports.md`). Matches the plan.
  Multi-word names must be kebab-case in the WIT and are exported kebab-case, so cleat's snake_case entry names need a
  rename step (Phase 1 or 5).
- **Output modes:** `static` (default, engine and bytecode in one module) and `-C dynamic`. Matches the plan.
- **Plugins:** `javy build -C plugin=<initialized plugin>`; a plugin is a Rust cdylib on `wasm32-wasip1` (or a `wasip2`
  component) using `javy-plugin-api`, then `javy init-plugin` (Wizer snapshot). **The docs are stale against the code:**
  `docs-using-extending.md` says `javy-plugin-api = "5.0.0"` and the dynamic example says `javy-default-plugin-v5`, while
  the v9.1.0 tree has plugin-api 7.1.0 and the default plugin's namespace is `javy-default-plugin-v4`. The source was the
  authority here.
- **Static mode keeps a custom plugin's other exports** (measured: `cleat_set_io` and `cleat_take_result` are exported by
  the Javy-built module), and removes only `invoke` and `compile-src` (`crates/codegen/src/lib.rs`). That is what makes
  bridge (a) possible without touching Javy.
- **Sizes:** stock hello-world static 1,359,454 bytes (README: "at least 869 KB"); dynamic 583 bytes plus a 1,341,746-byte
  plugin. The cleat-js static module is 1,029,129 bytes (`opt-level = "s"`, LTO).
- `javy build -C deterministic` ("fixed clocks and constant zero-filled RNG during pre-initialization") exists. It
  concerns the build-time snapshot, not run time; relevant to Phase 2.

## The bridge (a), as built

```
workflow.ts -esbuild-> bundle.js -javy build -C plugin=cleat-plugin.wasm (static)-> .javy.wasm
            -cleat-js-link-> .linked.wasm -tsmeta-> workflow.wasm (cleat.metadata + cleat_entry_points)
```

- `plugin/` (Rust, `javy-plugin-api`): installs `__cleat_args()`, `__cleat_result(json)`, `__cleat_fail(message)` in
  QuickJS and exports `cleat_set_io(args_ptr, args_len, out_ptr, max_out_len)` and `cleat_take_result() -> i64`.
- `link/` (Rust, walrus): for each entry point, takes Javy's `() -> ()` export and exports under the same name a function
  of the ABI's type, `(i32,i32,i32,i32) -> i64`, that calls `cleat_set_io`, then Javy's export, then
  `cleat_take_result`. It removes every other export except `memory`. **This is the new code the plan said bridge (a)
  needs**, about 60 lines.
- `tools/tsmeta` (Go, uses the repo's `wasm` package): writes `cleat.metadata` and `cleat_entry_points` byte-for-byte as
  `cleat build` would.

**The host places the input and output buffers itself**, above the guest's memory (`scratchBaseFor`, `engine/memory.go`):
the guest allocates nothing for them, so the plugin reads and writes raw pointers. *Read from the code:* that region
starts at least 10 MiB up and adds 2 MiB, so the 32 MiB sandbox ceiling is not all the QuickJS heap's.

## WASI census against `engine/wasi_policy.go`

Static imports of the deployed module (evidence section 1): `environ_get`, `environ_sizes_get`, `fd_close`,
`fd_fdstat_get`, `fd_read`, `fd_write`, `proc_exit` are allowed; `clock_time_get` is intercepted; **`fd_seek` is in the
fatal bucket**. No import comes from any module other than `wasi_snapshot_preview1`. The policy was not widened. A fatal
import traps on call, not on load, and the hello run completed, so `fd_seek` was not reached on this path. **That is the
caveat the policy's own `poll_oneoff` comment warns about:** a census of imports plus one successful run cannot see a
path taken only when something goes wrong (an exception, an out-of-memory). Phase 1 should run the error paths and, if
`fd_seek` is ever reached, stub it in the plugin.

## Controls

| | result (evidence section) |
|---|---|
| green: change the source's result, rebuild | `{"hello":"ts-two"}` comes back (4) |
| **red: skip `cleat-js-link`, deploy Javy's raw module** | `failed: host: export "hello": too many arguments provided` (5). The trampoline is what makes the module satisfy the ABI |
| dynamic mode on the stock engine | `failed: instantiate: unknown import: javy-default-plugin-v4::cabi_realloc has not been defined` (6) |

**Dynamic mode does not work on the stock engine, and the reason is structural:** a dynamically linked module imports
the plugin's `cabi_realloc`, `invoke` and `memory` from the host, and the engine has no way to supply a second module
(it also requires the guest to *export* `memory`). So Q5's "dynamic ≤ 100 KB per workflow, shared engine ≤ 3 MB" cannot
be met under Q1's "no engine changes": each deployed row is a static module of about 1 MB. Merging the plugin into the
small module afterwards gives the static module back. Phase 4 should measure static and report dynamic as blocked
unless the owner wants the engine change measured.

## Observations, not measurements

One cold and one warm execution of `hello` on this machine: 1,504,511,917 ns and 77,424,083 ns (`duration` in the worker
log). That is the cold-start compile of a 1 MB module against a cached one, a single sample each. Phase 4 measures
properly; nothing here is a threshold result.

## Host limits hit

None blocked this phase. Javy ran natively on macOS arm64 (the macOS kill that stopped `componentize-py` did not recur),
and Apple clang 21.0.0 built QuickJS for `wasm32-wasip1` through `rquickjs-sys`. The linux Javy binary is downloaded for
container use if a later phase needs it.

## What Phase 1 inherits

- Bind the section 4.3 host calls from the plugin as `env` imports. Javy's Wizer step runs the plugin inside Javy's own
  runtime, which must tolerate unresolved `env` imports: **not yet checked.**
- The `rust` label, the entry-name kebab/snake rename, and the `fd_seek` caveat above.
- Exceptions: an uncaught JS exception currently reaches Javy's generated export, which aborts; the SDK layer must catch
  it and call `__cleat_fail`.
