# Language Support Analysis for Cleat

> **Status, corrected 2026-08-09.** This document originally analyzed
> extending cleat to languages beyond Go, and estimated costs for Java
> ("~4-8 weeks"), AssemblyScript/TypeScript ("weeks 6-10"), and Python
> ("~4-5 weeks to MVP") as future work. All three have since shipped and are
> tier-ranked in `tiers.yaml`, the source of truth for support status:
> **Python is tier 1** (D2, DECIDED 2026-08-06 — same record-bearing bar as
> Go); **Java and AssemblyScript are tier 2** (D4, DECIDED 2026-08-06 —
> proven working with named open items, not yet at the tier-1 bar). Treat
> the language-by-language sections below as historical design rationale for
> *why* each approach was chosen, not as a statement of what remains to be
> built — the "Cost" and "remaining weeks" framings throughout are stale.
> Current gaps for each are tracked as `open_items` in `tiers.yaml`, not
> here.
>
> Baseline SDK sizes, re-derived 2026-08-09 (previous figures: Rust 537
> lines, Python 4,508 lines — both far out of date):
>
> ```
> wc -l crates/cleat-sdk/src/*.rs crates/cleat-macro/src/*.rs   # Rust: 4,324 lines
> find python-sdk/cleat_sdk -name '*.py' | xargs wc -l          # Python: 13,948 lines
> ```

## The WASM Boundary

Any language must:
1. **Compile to `wasm32-wasip1` (or `wasm32-unknown-unknown`)** — produce a `.wasm` shared library
2. **Export functions** with the cleat ABI: `(args_ptr, args_len, out_ptr, max_out_len) -> i64`
3. **Import host functions** from the `"env"` module with `(ptr, len)` string protocol — 59
   as of 2026-08-09 (`ABI.md` §2; re-derived via `engine/imports.go`), though a given SDK
   may only need a subset (Python's WIT world declares 52 — the two Go-`wasip1`-specific
   dispatch functions, `cleat_poll_work` and `cleat_complete`, don't apply to a
   component-model SDK)
4. **Read/write linear memory** at a 10 MiB scratch offset for string I/O
5. **Return the suspend sentinel** `(1 << 62)` for sleep/await-signals

The host runtime doesn't know or care what language produced the WASM bytes.

---

## Language-by-Language Assessment

### C — Cost: ~1 week, Value: Medium

**How:** `clang --target=wasm32-wasip1` produces standalone WASM. No runtime needed.

**SDK:** A `cleat.h` header declaring the 59 `extern` imports (see ABI.md §2) plus inline memory
helpers (`read_string`, `write_string`, `encode_export_result`). ~200 lines.

**Transformer:** None needed. The user `#include`s the header and writes `extern "C"`
export functions. The "build" is `clang --target=wasm32-wasip1 -o workflow.wasm workflow.c`.

**DX:** Manual FFI, no macros. The user writes the export wrapper by hand, similar to
the Rust proof-of-concept before `#[cleat_entry]`. A `CLEAT_EXPORT(name, input_type)`
C macro could reduce boilerplate to ~5 lines per export.

**Showstoppers:** None. C has been compiling to WASM since the MVP of wasmtime in 2019.

**Binary size:** ~10-50 KB (no runtime, no GC).

**Verdict:** Lowest cost. Do this first. The `cleat.h` header proves the ABI works
for all compiled languages.

### Zig — Cost: ~1 week, Value: Medium

**How:** `zig build-exe -target wasm32-wasi` (Zig ships its own WASM linker). Even
simpler toolchain setup than C — no clang/WASI sysroot needed.

**SDK:** A `cleat.zig` module with comptime-generated import wrappers and memory
helpers. Zig's `comptime` could auto-generate the 59 import declarations (see ABI.md §2). ~150 lines.

**Transformer:** None needed, but Zig's comptime reflection could generate export
wrappers at compile time without needing a separate proc-macro. Zero build-step
overhead beyond `zig build`.

**DX:** Potentially better than Go — comptime type-safe import generation without an
external transformer pipeline. The user writes a regular Zig function, and a
comptime-generated export wrapper handles the ABI.

**Showstoppers:** None. Zig's WASM support is production-quality (used by Bun, TigerBeetle).

**Binary size:** ~5-30 KB (Zig's compiler is aggressive about dead code elimination).

**Verdict:** Same low cost as C, but better DX. Zig is the dark-horse winner for
WASM-based durable execution — comptime reflection eliminates the need for a separate
transformer entirely.

### Java / Kotlin — Shipped, tier 2

> Corrected 2026-08-09: this section described Java as a "~4-8 weeks" future
> build. It shipped and was promoted to tier 2 on 2026-08-06 (D4 in
> `tiers.yaml`). Original design rationale kept below; see `tiers.yaml`'s
> `sdk-java` entry for current status and open items instead of this
> section's "Cost"/"Showstoppers" framing.

**How:** [TeaVM](https://github.com/konsoletyper/teavm) compiles JVM bytecode to
WASM (no embedded JVM — it's an AOT compiler). Compiles `.class` files to `.wasm`.
`crates/cleat-java` is the shipped SDK.

**SDK:** `crates/cleat-java` provides:
- A `HostCalls` class declaring the native imports via TeaVM's `@Import` annotations
- A `Memory` helper class for string I/O
- A `@CleatEntry` annotation for marking workflow methods

**Transformer:** An annotation processor reads `@CleatEntry` at compile time and
generates export wrapper methods that handle JSON (de)serialization, `HostCalls`
construction, and result packing.

**DX:** Familiar to Temporal Java users. `@CleatEntry` on a method, `HostCalls`
injected. `cleat build --target java` (via TeaVM's `gradle` integration).

**Measured 2026-08-06** (per `tiers.yaml`'s `sdk-java` entry): TeaVM compiles
`examples/saga-java-port` to a 324 KB module; wasmtime executes it, running
`accounts.Withdraw -> accounts.Deposit` with the `release_funds` compensation
correctly registered. `crates/cleat-java`'s own suite is 280 tests, 0 failures,
0 skips. `e2e-cross-language.yml` runs `TestJava*` with Java 17 + Gradle
provisioned.

**Known gaps (tier 2, not tier 1 — see `tiers.yaml` for the full list):**
- No cross-language replay coverage: `tests/cross-language` is Go↔Rust only,
  so nothing checks that a Java guest replays a history the Go host recorded.
- The workflow result comes back double-encoded (a JSON string containing
  JSON) where Go and Rust return an object — worth comparing all three before
  treating it as a bug in one.
- `examples/saga-java-port/ISSUES.md` lists 13 findings, 4 still open: no
  saga abstraction, `JsonHelper.parse` only supports `String.class`, no
  `getQueryState`, `durableDefer` takes a description rather than a closure.
- TeaVM's class library remains a subset of the JDK (no `java.lang.reflect`,
  limited `java.util.concurrent`) — a real constraint, not a blocker for most
  workflow code.

**Binary size:** ~324 KB measured (TeaVM includes a minimal class library + GC).

### AssemblyScript — Shipped, tier 2 (real TypeScript remains unbuilt)

> Corrected 2026-08-09: this section described AssemblyScript/TypeScript
> support as a "~6-12 weeks" future build and treated AssemblyScript purely
> as a stepping-stone option. AssemblyScript shipped and was promoted to
> tier 2 on 2026-08-06 (D4 in `tiers.yaml`). Real TypeScript (via Javy or
> Static Hermes, options B/C below) remains unbuilt and is not tracked in
> `tiers.yaml` at any tier — the analysis for those two is left as-is
> below since it is still forward-looking, not a status claim.

**A) AssemblyScript** (a TypeScript-like language that compiles directly to WASM):
- Mature compiler, produces small WASM binaries
- Subset of TypeScript (no closures, no `any`, manual memory management)
- SDK: `packages/cleat-as`, with a `HostCalls` class and `@cleatEntry` decorator
- Transformer: AssemblyScript transformer plugin that generates exports
- Still true and worth keeping in mind: AssemblyScript is NOT TypeScript. It's
  a different language that looks like TypeScript. Existing TS code will not
  compile as-is.
- Workflows must import the SDK as `@cleat/sdk`, not by relative path. Both
  resolve to the same files, but `asc` treats them as two distinct modules with
  two distinct `HostCalls` types — and, since §3.73, two distinct defer
  registries, so a defer registered through one would be drained from the
  other and silently never run. The generated wrapper calls `runDeferred(h)`,
  which makes that case a compile error rather than a silent one.

**Measured 2026-08-06** (per `tiers.yaml`'s `sdk-assemblyscript` entry):
`TestAssemblyScriptWorkflowExecute` passes on wasmtime —
`inventory.Reserve -> payments.Charge -> shipping.CreateShipment ->
notifications.SendEmail`, four calls in order — and the SDK's own as-pect
suite is 113/113 across four spec files (re-derive with
`cd packages/cleat-as && npm test`; measured 2026-09-02, was 106/106 across
three before §3.73 added `defer.spec.ts`). Making the test suite able to fail
(rather than degrading every failure to `t.Skipf`, as it did before #350)
found two real defects the same day: no data flowed between the saga's
steps (a field-naming mismatch, `reservationID` vs `reservation_id` etc.,
that the test never asserted on), and the example could not be installed
from a clean checkout (`package-lock.json` recorded its `file:`
devDependencies in a form `npm ci` refuses). Both are fixed.

**Resolved 2026-08-09:** `isReplaying()` was hardcoded `return false` with no
host call behind it, so every "only on first execution" branch fired on
every replay. Removed rather than implemented — `sideEffect()` already
covers the one safe use case. See `tiers.yaml`'s `sdk-assemblyscript`
open_items and `docs/determinism.md`.

**B) Javy / QuickJS-in-WASM** (Shopify's approach):
- Embeds the QuickJS JavaScript engine compiled to WASM
- Runs actual JavaScript/TypeScript code inside the WASM module
- SDK: `cleat-js` npm package with `HostCalls` class and `cleatEntry()` decorator
- Transformer: Babel plugin or `tsc` plugin that wraps entry functions with ABI glue
- **Binary size:** 1-5 MB (embedded JS engine)
- **Debugging:** Very difficult — JS running inside QuickJS inside WASM inside wazero.
  Three layers of abstraction.
- **Showstoppers:**
  - QuickJS-in-WASM adds significant overhead and debugging complexity
  - TypeScript type information is erased at compile time — can't generate typed
    adapters from TS interfaces the way cleat does for Go
  - The WASM binary is impractically large for database storage (1-5 MB per version)
  - No mature `tsc --target wasm32-wasip1` compiler exists

**C) Static Hermes** (Meta's experimental TypeScript→native compiler):
- Compiles TypeScript to native code via Hermes engine
- WASM target is experimental (not production-ready as of 2026)
- Could produce smaller binaries than QuickJS-in-WASM
- **Showstopper:** Not production-ready. Would be betting on an experimental compiler.

**Verdict:** Real TypeScript remains the highest-value unbuilt language (largest
developer community), and the technical path for it is still the roughest of
the three options above — neither has been attempted. AssemblyScript shipped
as the practical "TypeScript-flavored" option instead (tier 2, see above);
whether a real-TypeScript path is worth pursuing on top of it is an open
product question, not an engineering one at this point.

### Python — Shipped, tier 1

> Corrected 2026-08-09: this section described the Python SDK as having
> "WASM FFI incomplete" with all host imports stubbed to raise
> `NotImplementedError`, and estimated "~4-5 weeks to MVP". That is no
> longer true in any respect: Python was promoted to tier 1 on 2026-08-06
> (D2 in `tiers.yaml`) — the same record-bearing bar as Go, the only other
> tier-1 language. The SDK is `python-sdk/cleat_sdk/`, 13,948 lines
> (`find python-sdk/cleat_sdk -name '*.py' | xargs wc -l`, 2026-08-09), with
> a real WIT world (`python-sdk/wit/cleat.wit`, 52 function imports across
> 19 interfaces — including `durable-cron`, added for #450) wired through
> `componentize-py`, not stubs.

**How:** [componentize-py](https://github.com/bytecodealliance/componentize-py)
compiles CPython + the workflow module into a WASM component via the WIT
interface types in `python-sdk/wit/cleat.wit`. `cleat build --target python`
drives the pipeline.

**Status, per `tiers.yaml`'s D2 decision:**
- **Execution is proven, not just built.** `e2e-cross-language.yml` declares
  `python` in `CLEAT_REQUIRE_TOOLCHAINS`, so `TestPythonWasmEndToEnd` and
  `TestPythonComponentExecutionFence` `Fatal` rather than skip there, under
  `pipefail` — that job is blocking. All three real Python tests pass,
  building an 18.3 MB component and executing it on wasmtime (measured
  2026-08-06).
- **The SDK's own suite** is 442 tests passing across Python 3.10/3.11/3.12
  (down to 1 skip — `componentize-py` itself, which that job installs); no
  longer carries `continue-on-error` (removed in #348).
- Six example workflows exist: hello, saga, child fan-out, durable call,
  update handler, and a spin/timeout workflow.
- LangChain (`cleat_sdk/langchain/`) and LangGraph (`cleat_sdk/langgraph/`)
  integrations are built, not just planned.
- Cron scheduling shipped 2026-08-09 (#450) — Python workflows can register
  cron triggers via the `durable-cron` WIT interface.

**Known residual gaps** (see `tiers.yaml`'s `sdk-python` note, carried from
its tier-2 history):
- The component *decomposition* path (as opposed to the native component
  path Python actually uses) has never once succeeded — this is a separate,
  parked code path (`tiers.yaml` tier 3, `component-decomposition-path`),
  not a Python-specific gap.
- `wasm/component_test.go`'s `TestComponentPythonBinary` reads a hardcoded
  `/tmp/test_python.wasm` and has never run anywhere but the machine it was
  written on — a test-design bug, tracked separately, not a tier-1 blocker.
- Binary size (CPython WASM is large — the measured component is 18.3 MB)
  remains a real operational consideration for `workflow_defs.wasm_bytes`
  storage at scale, independent of correctness.

**Verdict:** The Python SDK is no longer a build-estimate item. It carries the
same record-bearing bar as Go (tier 1), ships with LangChain/LangGraph
integration, and is proven end-to-end in CI, not just built on disk.

### Go — Already Done

Full automated transformer pipeline: analyzer → callgraph → closure → transform →
WASM compile via the standard Go toolchain (`--target go`, default).
~3,000 lines of transformer code across 5 packages.

### Rust — Shipped, tier 2

`crates/cleat-sdk` + `crates/cleat-macro` + `cleat build --target rust`.
4,324 lines total as of 2026-08-09 (`wc -l crates/cleat-sdk/src/*.rs
crates/cleat-macro/src/*.rs`) — grown substantially from the original
537-line estimate as the SDK hardened (see `DX_COMPARISON.md`). Tier 2 in
`tiers.yaml`: the only non-Go language with cross-language replay tests
(`tests/cross-language`, Go↔Rust).

---

## Summary Table

> Corrected 2026-08-09: "SDK effort"/"Transformer effort" below are the
> original pre-build estimates, kept for languages that are still unbuilt
> (C, Zig, real TypeScript, C#). For the four that have since shipped
> (Go, Rust, Python, Java, AssemblyScript), those columns are replaced with
> actual status per `tiers.yaml`.

| Language | WASM compiler | Status | Binary size (measured where shipped) | Tier |
|----------|--------------|--------|-------------------|------|
| **Go** | go | Shipped | ~4-10 MB | 1 |
| **Python** | componentize-py | Shipped, proven end-to-end in CI | 18.3 MB (measured 2026-08-06) | 1 |
| **Rust** | cargo build (built-in) | Shipped | ~50-200 KB | 2 |
| **Java/Kotlin** | TeaVM (mature) | Shipped | 324 KB (measured 2026-08-06) | 2 |
| **AssemblyScript** | asc (mature) | Shipped | 10-50 KB (design estimate) | 2 |
| **C** | clang (mature) | Not built | 10-50 KB (design estimate) | — |
| **Zig** | zig build-exe (mature) | Not built | 5-30 KB (design estimate) | — |
| **TypeScript** (real) | Javy/QuickJS (mature) | Not built | 1-5 MB (design estimate) | — |
| **C#/.NET** | NativeAOT-LLVM (exp.) | Not built | 1-5 MB (design estimate) | — |

---

## The Binary Size Constraint

Cleat stores WASM blobs in `workflow_defs.wasm_bytes`. Each deploy creates a
new row. With 10 versions of a workflow, you're storing 10× the binary size.
This drives the ranking:

- **Good** (< 500 KB): C, Zig, Rust, AssemblyScript, Java/TeaVM
- **OK** (500 KB - 2 MB): Go (standard), TypeScript/Javy (worst-case, unbuilt)
- **Problematic** (> 2 MB): Python/CPython — measured at 18.3 MB per
  component (2026-08-06), which makes the "deploy is an INSERT" model
  expensive at scale. This is now a known, accepted cost of a shipped tier-1
  language, not a hypothetical.

If binary size at scale becomes a real constraint, consider:
- Store WASM in object storage (S3) instead of the database, with a
  `wasm_url` column
- Or use a WASM deduplication layer (the embedded runtime is identical across
  workflow versions — only the user code differs)

---

## Recommended Order

> Stale as of 2026-08-09: Java and AssemblyScript have already shipped (see
> above), out of order relative to this original plan, which assumed C/Zig
> would come first. Kept for the reasoning behind C/Zig/TypeScript, which are
> still unbuilt.

1. **C + Zig**: Prove the ABI works for compiled languages. The `cleat.h`
   header is < 200 lines and validates that no host changes are needed. Still
   not started as of 2026-08-09.
2. ~~Java~~ — shipped, tier 2. See the Java section above.
3. ~~AssemblyScript~~ — shipped, tier 2. See the AssemblyScript section
   above. Real TypeScript remains a separate, unbuilt effort; it could follow
   once Javy or Static Hermes stabilizes.
4. ~~Python WASM FFI~~ — shipped, tier 1, proven end-to-end in CI. See the
   Python section above.
5. **TypeScript via Javy**: The only entry on this list still meaningfully
   "next" — genuinely unbuilt, and would follow AssemblyScript's adoption
   data if pursued. Focus areas unchanged: reducing binary size
   (tree-shaking the JS runtime) and improving debugging.

No shipped language hit a hard showstopper — Python via CPython-wasm works
in production CI today, with binary size the main operational cost to
manage (object storage for large blobs, if it becomes a problem at scale).
The WASM boundary design has held up across five shipped languages
(Go, Rust, Python, Java, AssemblyScript).
