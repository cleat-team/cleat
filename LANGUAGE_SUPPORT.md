# Language Support Analysis for Cleat

> **Status, corrected 2026-08-09.** This document originally analyzed
> extending cleat to languages beyond Go, and estimated costs for Java
> ("~4-8 weeks"), AssemblyScript/TypeScript ("weeks 6-10"), and Python
> ("~4-5 weeks to MVP") as future work. Java and Python have since shipped,
> TypeScript shipped via Javy, and AssemblyScript shipped and was later
> removed (cleat#4023). The supported ones are tier-ranked in `tiers.yaml`,
> the source of truth for support status:
> **Python is tier 1** (D2, DECIDED 2026-08-06 — same record-bearing bar as
> Go); **Java is tier 2** (D4, DECIDED 2026-08-06 —
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
3. **Import host functions** from the `"env"` module with `(ptr, len)` string protocol —
   `ABI.md` §2 enumerates them and `scripts/check-doc-consistency.sh` holds that list to
   `engine/imports.go`. A given SDK may need only a subset: Python's WIT world omits the
   two Go-`wasip1`-specific dispatch functions, `cleat_poll_work` and `cleat_complete`,
   which don't apply to a component-model SDK. No count is quoted here because it moves —
   this line read 59 from 2026-08-09 and was five short by 09-13
4. **Read/write linear memory** at a 10 MiB scratch offset for string I/O
5. **Return the suspend sentinel** `(1 << 62)` for sleep/await-signals

The host runtime doesn't know or care what language produced the WASM bytes.

---

## Language-by-Language Assessment

### C — Cost: ~1 week, Value: Medium

**How:** `clang --target=wasm32-wasip1` produces standalone WASM. No runtime needed.

**SDK:** A `cleat.h` header declaring one `extern` per host call (see ABI.md §2) plus inline memory
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
helpers. Zig's `comptime` could auto-generate the import declarations (see ABI.md §2). ~150 lines.

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

### TypeScript options — AssemblyScript removed, TypeScript shipped via Javy (tier 2)

> Corrected 2026-11-18: this section analysed three ways to run TypeScript.
> **A) AssemblyScript** shipped at tier 2 and was removed (cleat#4023); its
> analysis is not kept. **B) Javy / QuickJS-in-WASM** is what shipped as
> TypeScript, at tier 2 (`tiers.yaml`, `sdk-typescript`). **C) Static Hermes**
> has not been attempted. B and C are left as the original analysis below, which
> is design rationale, not a status claim.

**B) Javy / QuickJS-in-WASM** (Shopify's approach):
- Embeds the QuickJS JavaScript engine compiled to WASM
- Runs actual JavaScript/TypeScript code inside the WASM module
- SDK: `cleat-js` npm package with `HostCalls` class and `cleatEntry()` decorator
- Transformer: Babel plugin or `tsc` plugin that wraps entry functions with ABI glue
- **Binary size:** 1-5 MB (embedded JS engine)
- **Debugging:** Very difficult — JS running inside QuickJS inside WASM inside
  wasmtime. Three layers of abstraction.
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

**Verdict:** Real TypeScript has the largest developer community of the three
options, and the technical path was the roughest. Option B (Javy) is the one
that shipped, at tier 2, with its known differences from Node tracked in
`packages/cleat-ts/DIVERGENCES.md`; option C (Static Hermes) has not been
attempted.

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
- Binary size (CPython WASM is large — the measured component was 18.3 MB on 2026-08-06, and `cleat build` now adds about 11.6 MB of zero-filled gaps so instances share their memory image, cleat#4382: about 30 MB)
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
> (Go, Rust, Python, Java), those columns are replaced with
> actual status per `tiers.yaml`.

| Language | WASM compiler | Status | Binary size (measured where shipped) | Tier |
|----------|--------------|--------|-------------------|------|
| **Go** | go | Shipped | ~4-10 MB | 1 |
| **Python** | componentize-py | Shipped, proven end-to-end in CI | 18.3 MB (measured 2026-08-06); about 30 MB since cleat#4382 (measured 2026-11-22) | 1 |
| **Rust** | cargo build (built-in) | Shipped | ~50-200 KB | 2 |
| **Java/Kotlin** | TeaVM (mature) | Shipped | 324 KB (measured 2026-08-06) | 2 |
| **C** | clang (mature) | Not built | 10-50 KB (design estimate) | — |
| **Zig** | zig build-exe (mature) | Not built | 5-30 KB (design estimate) | — |
| **TypeScript** (real) | Javy/QuickJS | Shipped (tier 2; known differences from Node in `packages/cleat-ts/DIVERGENCES.md`) | ~1.45 MB (measured 2026-11-17, the example module; see below) | 2 |
| **C#/.NET** | componentize-dotnet (NativeAOT-LLVM, Component Model; **prerelease toolchain**) | Shipped (tier 2; prerelease toolchain and other limits in the section below) | 0.7-1.05 MB (measured in the cleat#4077 spike: a hello and a 9-mode test workflow); 1.35 MB for the SDK fixture (2026-11-18, `packages/cleat-csharp/README.md`) | 2 |

### TypeScript (real): tier 2

`tiers.yaml` lists TypeScript under tier 2 as a supported language. It carried **IN DEVELOPMENT -- NOT SUPPORTED** from its first
commit until cleat#3898's fix removed the last silent divergence from Node, which was the owner's condition for the claim. The
language can be built, tested and run, and [the getting-started page](docs/tutorials/typescript-quick-start.md) is where to
start. What it cannot do or has not been shown to do, each with its issue:

- **Every host call is bound or deliberately unbound; none is pending** (cleat#3827; checked 2026-11-18). Four are deliberately unbound, each with a `permanent:` reason in `packages/cleat-ts/host-calls.json`: `cleat_poll_work` and `cleat_complete` (the worker's own handshake), `cleat_register_query_handler` (not bindable from a guest) and `cleat_interrupt_requested` (polled by the plugin itself, never by a workflow). `TestEveryHostCallIsBoundOrListed` reads that file and `host-calls.d/*.json` and fails when a call is in neither list, so a host call added later without a binding turns it red. Re-derive it, reading both files: `python3 -c "import json,glob;u={n:str(r) for f in ['packages/cleat-ts/host-calls.json']+glob.glob('packages/cleat-ts/host-calls.d/*.json') for n,r in json.load(open(f)).get('unbound',{}).items()};print(len(u),'unbound; pending:',[n for n,r in u.items() if r.startswith('pending:')])"` should print `4 unbound; pending: []`.
- **A killed workflow's `ctx.defer` cleanup runs, except for a guest whose work never returns to the bytecode loop** (cleat#3922, fixed in cleat#4053). The host asks the guest to stop through an interrupt poll before its hard limit, so the drain runs; a single native operation that never yields (a `"x".repeat(1_000_000)` loop reached no poll at all) still ends on the hard trap and its defers are lost. Measured and stated in `packages/cleat-ts/DIVERGENCES.md` D1. A *cancelled* workflow's cleanup runs on its own path.
- **`CheckJoiners` is not implemented** (cleat#4020): a ZWNJ or ZWJ in a host label is refused whatever its context, so a
  Devanagari, Malayalam or Arabic host that Node accepts is refused here. Loud, not silent.
- **No timers and no `crypto`; `TextEncoder`/`TextDecoder` are UTF-8 only** (cleat#3829, `DIVERGENCES.md` rows 3, 7 and 8): a `TextDecoder` for another encoding throws `RangeError`, and the stream variants are undefined. Local time is UTC.
- **Dialects:** the database-backed worker tests run on PostgreSQL, MySQL and SQL Server in tier-2 CI jobs (they must run, they
  gate no merge), and only those tests run on MySQL and SQL Server. **Deploying a *plugin* is PostgreSQL-only** (cleat#4005);
  `cleatctl deploy workflow` works on all three (cleat#4004).

The divergences from Node are listed in `packages/cleat-ts/DIVERGENCES.md`, and the URL claim is measured rather than asserted:
every code point U+0000-U+10FFFF was swept in a raw and an already-Punycode-encoded host label, by accept/refuse and by the
resulting host value, against the Node version CI pins -- zero silently accepted code points and zero silent value differences.
Where Node's own versions disagree the shim follows the specification and the answer is pinned as a literal, and where the
mapping needs the UTS46 table the shim refuses rather than mis-encode: those are the loud cases, and each is named.

The binary-size cell is one measurement: the example module in `examples/ts-workflow` is 1,446,426 bytes and the Javy plugin it
embeds is 1,296,266 bytes (2026-11-17, macOS arm64; `cleat build --target ts -o <dir> examples/ts-workflow/workflow.ts:greet`).
The evidence behind the claim, and the open items, are in `tiers.yaml` under `sdk-typescript`.


### C#/.NET: tier 2

`tiers.yaml` lists C# under tier 2 as a supported language (owner decision 2026-11-19). It carried **IN DEVELOPMENT -- NOT SUPPORTED** from its
first commit until the last of the seven PRs tracked in cleat#4080. The language can be built, tested and run, and
[the getting-started page](docs/tutorials/csharp-quick-start.md) is where to start. What a reader has to know, stated plainly:

- **The toolchain is a prerelease.** `Microsoft.DotNet.ILCompiler.LLVM` is a release candidate (`10.0.0-rc.1.*`) from the `dotnet-experimental`
  feed, not from nuget.org, and `componentize-dotnet` is `0.8.0-preview`. Both are pinned by digest and hash (`packages/cleat-csharp/toolchain.env`,
  `toolchain.packages.tsv`) and a build refuses anything else, but a pin is not a support commitment from upstream. **Builds are not byte-reproducible**
  (same size, different bytes).
- **There is no NuGet package.** The SDK is a source package a project imports by path from a cleat checkout, so a C# workflow is built inside a
  checkout, in a pinned linux container (`cleat build --target csharp`; the compiler does not run on macOS), amd64 or arm64.
- **No C# workflow has been run on a real worker against a database.** The tests drive the engine in process; `tests/crash` has no C# client, and
  MySQL and SQL Server have not been measured for C# at all.
- **A suspension is an exception and it can be caught.** A `catch (Exception)` that swallows `CleatSuspendException` lets the workflow keep running, though since cleat#4285 the host refuses the host calls it then makes (none reaches
  the service); the code between them still runs. The determinism vet warns on it (`CV101`); do not swallow it.
- **`async`/`await` works over host calls only because every awaited task is already complete.** `Task.Delay`, `Task.Run` and a blocking wait on
  an incomplete task throw `PlatformNotSupportedException` (`Task.Yield` returns); to wait, call `HostCalls.Sleep`.
- **Typed (record) entries** go through a source-generated `JsonSerializerContext` the author writes, and the first one costs about 1.6 MB and 140 ms
  of first execution (measured 2026-11-18, amd64 emulation).
- **`cleat-gen-plugin` has no C# target.** One host call, `register-query-handler`, is deliberately not bound in any SDK.

CI runs the C# tests in the required Tier 2 Gate and in a non-required workflow (`.github/workflows/csharp-tier2.yml`) that also runs the SDK's
managed unit tests and a native arm64 build. The evidence behind the claim, and the open items, are in `tiers.yaml` under `sdk-csharp`.

---

## Determinism enforcement is not the same in every language

"Enforced determinism, not promised determinism" is true of every shipped language in the sense
that a check exists and runs. It is **not** true that the checks are comparable. They differ by
about as much as a compiler differs from `grep`, and a table of five green fixtures would hide
that completely — which is why this section states the mechanism rather than a pass/fail.

| Language | Mechanism | What it can see | What it cannot |
|---|---|---|---|
| **Go** | whole-program analysis (`analyze()` in `cmd/cleat`) | the durable closure, computed from the type-checked call graph | — |
| **Python** | AST call-graph and closure analysis (`python-sdk/cleat_sdk/vet.py`) | what the entry point reaches, across functions | a forbidden call spelled so the `(module, function)` table does not match it |
| **Java** | import and fully-qualified-name resolution over source with comments, strings, text blocks and character literals blanked first (`cmd/cleat/vet_java.go`) | a path expression **resolving** to a listed class, member or package -- so static imports, wildcard imports (against a known class list) and fully-qualified calls are all seen | java.nio, which is on no list at all; reflection with a computed argument, which no resolver can evaluate |
| **Rust** | `use`-declaration resolution over source with comments, strings and `#[cfg(test)]` items blanked first (`cmd/cleat/vet_rust.go`); plus, since cleat#1864, per-function syntactic binding tracking for one usage pattern (HashMap/HashSet iteration order, R008) | a path expression **resolving** to a listed module — so grouped imports, nested groups and `as` aliases are all seen; **and** a HashMap/HashSet whose type is declared on a function parameter or a `let` binding, when it is later enumerated (`for x in m`, `m.iter()`, `.keys()`, `.values()`, `.into_iter()`, `.drain()`) |a method call whose receiver is not a bare tracked identifier -- a struct field (`self.counts.iter()`), a value returned from a call, or a map produced inline by `.collect::<HashMap<_, _>>()` and iterated without ever being bound to a name; any module not on the path list; a glob import, which names nothing locally |

Re-derive the shape of each, rather than trusting the row:

    grep -c '^\s*{"' cmd/cleat/vet_rust.go cmd/cleat/vet_java.go   # resolved-path table entries
    grep -oE '"PY[0-9]{3}"' python-sdk/cleat_sdk/vet.py | sort -u | wc -l
    grep -ln 'runVet' cmd/cleat/build_*.go                          # which builds gate

### A longer list is not a stronger check, and neither is a resolver over the wrong list

Java's checker resolves imports and fully-qualified names (cleat#1812), which fixed how a listed
API is spelled, not which APIs are listed. `java.nio` is on no list at all — the API Java has
recommended over `java.io` since 1.7 — so it covers the legacy package and not its replacement:

    import java.io.File;                 // reported
    Files.readString(Path.of("x"));      // not reported

Rust's resolves the same forms Java's does now (cleat#1811) — a grouped import is caught, not
missed. What it still cannot see, in general, is a **method** call, which names no module at all:

    SystemTime::now();                   // reported -- a path expression
    t.elapsed();                         // not reported -- names no module, only a receiver's type

**One method-call shape is an exception, since cleat#1864, and it is narrow on purpose.** A HashMap
or HashSet's *enumeration order* is not itself a module reference, so R008 tracks the receiver's
type through a syntactic binding — a function parameter or a `let` — rather than through a path:

    fn total(m: &HashMap<String, u64>) { for (k, v) in m { ... } }   // reported -- m is a tracked binding
    self.counts.iter()                                                // not reported -- a struct field, not a binding
    pairs.into_iter().collect::<HashMap<_, _>>().into_iter()          // not reported -- never bound to a name

The middle and bottom rows are `known_limit_map_via_struct_field` and `known_limit_collect_into_map`.
Neither generalises to method calls at large: `t.elapsed()` above is exactly as unseen as before.

Both are measured, and each has a fixture pinning it: see the `known_limit_*` directories under
`testdata/vet-checks/`, and the `*_build_refuses_nondeterminism_test.go` files, which assert **both**
that a violation is refused and that the known limit is not. The second arm is the one that keeps
this table honest — without it, "every language refuses a bad fixture" reads as parity that does not
exist.

### The scope question, which all four got wrong

A determinism check has to decide what counts as workflow code, and three of the four answered by
walking the call graph the wrong way (cleat#1789, #1799, #1813):

- **determinism** is a property of what a workflow *executes*, so the scope is forward reachability
  from the entry points. Walking backwards instead means a helper the workflow **calls** is never
  checked;
- **"must carry a HostCalls"** is a property of what *reaches the host*, so it is the intersection
  of that forward scope with the callers closure. Either closure alone is wrong: the callers closure
  flags the test harness that *supplies* `h`, and the forward closure flags pure helpers that need
  none.

Fixed for Python in #1816. Rust and Java have no closure
analysis at all, so they do not have this specific problem — their scope is "every source file in
the tree, including tests" (cleat#1789).

Rust reads only executable code since #1815 and Java since #1824, so the two rows are now the same
shape. Java's scanner is the simpler of the pair: block comments do not nest, and `'` always opens a
character literal rather than sometimes a lifetime. Java 15 text blocks are the one construct Rust
has no equivalent of.

---

## The Binary Size Constraint

Cleat stores WASM blobs in `workflow_defs.wasm_bytes`. Each deploy creates a
new row. With 10 versions of a workflow, you're storing 10× the binary size.
This drives the ranking:

- **Good** (< 500 KB): C, Zig, Rust, Java/TeaVM
- **OK** (500 KB - 2 MB): Go (standard), TypeScript/Javy (worst-case, unbuilt)
- **Problematic** (> 2 MB): Python/CPython — measured at 18.3 MB per
  component (2026-08-06; about 30 MB since cleat#4382, 2026-11-22), which makes the "deploy is an INSERT" model
  expensive at scale. This is now a known, accepted cost of a shipped tier-1
  language, not a hypothetical.

If binary size at scale becomes a real constraint, consider:
- Store WASM in object storage (S3) instead of the database, with a
  `wasm_url` column
- Or use a WASM deduplication layer (the embedded runtime is identical across
  workflow versions — only the user code differs)

---

## Recommended Order

> Stale as of 2026-08-09: Java has already shipped (see
> above), out of order relative to this original plan, which assumed C/Zig
> would come first. Kept for the reasoning behind C/Zig/TypeScript, which are
> still unbuilt.

1. **C + Zig**: Prove the ABI works for compiled languages. The `cleat.h`
   header is < 200 lines and validates that no host changes are needed. Still
   not started as of 2026-08-09.
2. ~~Java~~ — shipped, tier 2. See the Java section above.
3. ~~AssemblyScript~~ — shipped at tier 2, then removed (cleat#4023). TypeScript
   shipped separately, via Javy; see the TypeScript sections.
4. ~~Python WASM FFI~~ — shipped, tier 1, proven end-to-end in CI. See the
   Python section above.
5. ~~TypeScript via Javy~~ — shipped, tier 2. Focus areas unchanged: reducing
   binary size (tree-shaking the JS runtime) and improving debugging.

No shipped language hit a hard showstopper — Python via CPython-wasm works
in production CI today, with binary size the main operational cost to
manage (object storage for large blobs, if it becomes a problem at scale).
The WASM boundary design has held up across five shipped languages
(Go, Rust, Python, Java, TypeScript).
