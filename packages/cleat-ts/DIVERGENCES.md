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
| 2 | No `URL` or `URLSearchParams`. zod's `z.url()` calls `new URL(...)` inside a `try`/`catch`, so the `ReferenceError` was swallowed and a valid URL reported invalid. | **silent** | **fixed** for what can be answered exactly (the `url` shim); for the rest it throws, which a catch-all such as zod's still reads as "invalid" (see below) | `TestTypeScriptURLAgreesWithNode` (867 cases), `TestTypeScriptURLSearchParamsAgreeWithNode` (135), `TestTypeScriptURLRefusesWhatItCannotAnswer` |
| 3 | No `TextEncoder` / `TextDecoder`. | loud (`ReferenceError`) | open | none yet |
| 4 | No timers: `setTimeout`, `setInterval`, `setImmediate` are not defined, by design (a timer is not a durable operation). lodash-es `debounce` fails with `setTimeout is not defined`. The static check refuses a source that names one. | loud | by design | `TestTypeScriptStaticCheckRefusesATimerAndNamesTheLine` |
| 5 | No `crypto`. uuid `v4()`/`v7()` with no arguments fails with `crypto is not defined`. With the `random` option supplied they equal Node: pass the SDK's replay-safe random in. | loud | by design | none yet |
| 6 | Local time is UTC: the guest has no time zone. A workflow must not depend on the machine's zone anyway, since a replay may run elsewhere. | — | by design | the conformance tests run Node with `TZ=UTC` |

## What the URL shim does and does not do

It is the WHATWG URL algorithm for the part that can be done exactly: the special schemes (`http`, `https`, `ws`, `wss`, `ftp`)
and generic ones, relative resolution against a base, percent-encoding, IPv4 hosts (including the hex, octal and short forms),
`URLSearchParams` in full, `URL.canParse` and `URL.parse`, and the `search` and `hash` setters (which update `searchParams`).

It **throws** `cleat: URL: <what> is not supported in the guest`, and never returns a guess, for: a `file:` URL, an IPv6 host, a
host that is not ASCII or that contains an `xn--` label (both need IDNA processing), and setting `href`, `protocol`,
`username`, `password`, `host`, `hostname`, `port` or `pathname`. `TestTypeScriptURLRefusesWhatItCannotAnswer` holds each one.

**That is loud to code that lets the error out, and not to code that catches everything.** zod's `z.url()` wraps `new URL` in a
`try`/`catch`, which is how the `ReferenceError` was swallowed in the first place: for an input in the list above `z.url()` still
reports "invalid" where Node says valid. So row 2 is fixed for every input the shim answers and **remains silent through a
catch-all for the ones it refuses** (an IPv6 host, an internationalised host, a `file:` URL). It is not a support claim for those.
The remaining inputs are cleat#3898.

**Where Node itself departs from the specification** (measured on Node 26.4.0, not in the guest): a relative input that carries a
fragment is accepted against a base with an opaque path (`new URL("x#y", "foo:bar")` is `foo:bar/x#y`, while `new URL("x",
"foo:bar")` throws). The specification makes every one of them a failure unless the input starts with `#`, and the guest follows
the specification. Those pairs are excluded from the comparison in `urls.ts`.

## What fixing one costs

Row 1 is a patch over the setters in the plugin's initialisation script. Measured on macOS arm64, `hello.ts`: the plugin
grows by 377 bytes (1,082,221 to 1,082,598) and a deployed module by 4,340 bytes (1,125,603 to 1,129,943, about 0.4%), because
the initialised heap carries the patched functions.

Row 2, measured 2026-11-14 on the same machine with the same toolchain: the plugin grows by 1,405 bytes (1,274,892 to
1,276,297) and a deployed `hello.ts` module by **74,202 bytes (1,330,301 to 1,404,503, about 5.6%)**, for the compiled classes
that the initialised heap carries. That is the price of removing a silent wrong answer from every module, used or not.

## How a row is held

A conformance test runs one source, plain JavaScript in a `.ts` file under `tests/typescript/testdata/conformance/`, in both
places and compares what each prints: the guest through the module `build.sh` builds, Node through the same source bundled by
esbuild. The oracle is Node itself, not a belief about Node. Every fix is also run **with its shim left out of the build**
(`CLEAT_SHIMS_OFF=<name> REBUILD_PLUGIN=1`), and the test must fail: row 1 does (235 of 631 cases differ without it), and row 2 does (all three tests fail with `URL is not defined`).

The oracle is **Node 24 or newer** for the URL rows, and the tests fail, not skip, on an older one. Node 20 (which the tier-2 job used until
this row) leaves `^` unencoded in a path and drops the root slash of a non-special URL whose path `..` climbs above the root (`foo:`
where the current specification gives `foo:/`): against it the corpus reports 9 differences that are the oracle's age and not the
shim's. Measured 2026-11-14 on Node 20.20.2, 24.21.0 and 26.4.0; the last two agree with the shim on every case. Two answers that vary
by Node version, `new URLSearchParams(null)` and the key order of an iterator's result object, are left out of the corpus.

A control build leaves the shim-less plugin in `packages/cleat-ts/.work/`, and the next ordinary build reuses it (cleat#3897): run the next build with `REBUILD_PLUGIN=1` to put the shipped plugin back.

## Where the guest differs from the other cleat SDKs

The rows above are guest-versus-Node. These are guest-versus-Go-and-Rust, and they matter for a different reason: a cleat
workflow's behaviour should not depend on which language it was written in. A tier-2 guest is allowed to differ where the
difference is **recorded here**; what it may not do is differ silently.

| # | Difference | What it means for you | Held by |
|---|---|---|---|
| D1 | **A killed workflow's defers do not run.** The other core-module SDKs run their deferred work when the host stops a workflow (the execution fence, the instruction limit, an unrecoverable runtime failure) — Go and Rust, and Java and AssemblyScript, which emit `__cleat_run_deferred` too (`wasm/exports.go`, `tiers.yaml`). This guest does not: the host looks for an export named `__cleat_run_deferred`, and a Javy-linked module exports only `memory` and its entry points, so there is nothing for it to call. | If the host kills a run — for any reason other than the workflow finishing — its deferred cleanup does **not** happen, and whatever that cleanup would have done is lost. The workflow's other durable effects are unaffected. | `TestTypeScriptDeferredRunsAfterCompletionAndNotOnSuspension` covers the two paths that DO work; there is no test for this row because the behaviour is the absence of one. Parity is cleat#3922. |

Defers otherwise behave as in the other SDKs: they run after the workflow returns **or throws**, in reverse registration
order, each body awaited, and one failing body does not stop the rest. They do **not** run when the workflow suspends — a
suspended run has not finished, and running cleanup at the first sleep would release work the resumed run still needs.
