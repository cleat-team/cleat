# Where the TypeScript guest differs from Node

TypeScript is a **tier-2** guest language: in development, **not supported** (cleat#3825, `tiers.yaml`). This is the list of
known places where code that runs under Node gives a different answer in the guest, which runs on QuickJS inside a WASM
module with no clock, no event loop and no randomness of its own. It records what was measured and what was not, and each row
names the test that holds it.

A divergence is **silent** when the guest returns a plausible wrong value with no error, and **loud** when it throws. Silent
ones are the dangerous kind and must be fixed or removed before any support claim; loud ones are documented with a workaround.

Only what was looked at is listed. The first survey ran 27 cases over four npm packages (zod 4.6.7, date-fns 4.4.0,
lodash-es 4.18.1, uuid 14.0.3) unmodified; there may be more.

| # | Divergence | Kind | Status | Held by |
|---|---|---|---|---|
| 1 | `Date` setters given a non-finite argument returned `NaN` but left the date unchanged (spec: an Invalid Date). `date-fns` `isValid(parseISO("nope"))` was `true`. | **silent** | **fixed** (the `datesetters` shim in `crates/cleat-js-plugin/src/shims.js`) | `TestTypeScriptDateSettersAgreeWithNode`: 631 cases against Node |
| 2 | No `URL` or `URLSearchParams`. zod's `z.url()` calls `new URL(...)` inside a `try`/`catch`, so the `ReferenceError` was swallowed and a valid URL reported invalid. | **silent** | **open** | none yet |
| 3 | No `TextEncoder` / `TextDecoder`. | loud (`ReferenceError`) | open | none yet |
| 4 | No timers: `setTimeout`, `setInterval`, `setImmediate` are not defined, by design (a timer is not a durable operation). lodash-es `debounce` fails with `setTimeout is not defined`. The static check refuses a source that names one. | loud | by design | `TestTypeScriptStaticCheckRefusesATimerAndNamesTheLine` |
| 5 | No `crypto`. uuid `v4()`/`v7()` with no arguments fails with `crypto is not defined`. With the `random` option supplied they equal Node: pass the SDK's replay-safe random in. | loud | by design | none yet |
| 6 | Local time is UTC: the guest has no time zone. A workflow must not depend on the machine's zone anyway, since a replay may run elsewhere. | — | by design | the conformance tests run Node with `TZ=UTC` |

## What fixing one costs

Row 1 is a patch over the setters in the plugin's initialisation script. Measured on macOS arm64, `hello.ts`: the plugin
grows by 377 bytes (1,082,221 to 1,082,598) and a deployed module by 4,340 bytes (1,125,603 to 1,129,943, about 0.4%), because
the initialised heap carries the patched functions. The size cost of `URL`/`URLSearchParams` (row 2) is **not yet measured**.

## How a row is held

A conformance test runs one source, plain JavaScript in a `.ts` file under `tests/typescript/testdata/conformance/`, in both
places and compares what each prints: the guest through the module `build.sh` builds, Node through the same source bundled by
esbuild. The oracle is Node itself, not a belief about Node. Every fix is also run **with its shim left out of the build**
(`CLEAT_SHIMS_OFF=<name> REBUILD_PLUGIN=1`), and the test must fail: row 1 does (235 of 631 cases differ without it).
