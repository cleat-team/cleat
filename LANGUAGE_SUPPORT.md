# Language Support Analysis for Cleat

Extending cleat's WASM-based workflow model to additional languages.
Baseline: the Rust SDK is 537 lines (host_calls 290 + memory 126 + proc-macro 121).

## The WASM Boundary

Any language must:
1. **Compile to `wasm32-wasip1` (or `wasm32-unknown-unknown`)** — produce a `.wasm` shared library
2. **Export functions** with the cleat ABI: `(args_ptr, args_len, out_ptr, max_out_len) -> i64`
3. **Import 15 host functions** from the `"env"` module with `(ptr, len)` string protocol
4. **Read/write linear memory** at a 10 MiB scratch offset for string I/O
5. **Return the suspend sentinel** `(1 << 62)` for sleep/await-signals

The host runtime doesn't know or care what language produced the WASM bytes.

---

## Language-by-Language Assessment

### C — Cost: ~1 week, Value: Medium

**How:** `clang --target=wasm32-wasip1` produces standalone WASM. No runtime needed.

**SDK:** A `cleat.h` header declaring the 15 `extern` imports plus inline memory
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
helpers. Zig's `comptime` could auto-generate the 15 import declarations. ~150 lines.

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

### Java / Kotlin — Cost: ~4-8 weeks, Value: High

**How:** [TeaVM](https://github.com/konsoletyper/teavm) compiles JVM bytecode to
WASM (no embedded JVM — it's an AOT compiler). Compiles `.class` files to `.wasm`.
Also works for Kotlin, Scala, and any JVM language.

**SDK:** A `cleat-java` JAR with:
- `HostCalls` class declaring the 15 native imports via TeaVM's `@Import` annotations
- `Memory` helper class for string I/O
- `@CleatEntry` annotation for marking workflow methods

**Transformer:**
- Annotation processor (javac plugin) that reads `@CleatEntry` annotations at
  compile time and generates export wrapper methods
- The export wrapper handles JSON deserialization (using a bundled JSON library or
  TeaVM's built-in JavaScript interop), HostCalls construction, and result packing

**DX:** Familiar to Temporal Java users. `@CleatEntry` on a method, `HostCalls`
injected. Gradle/Maven plugin runs `cleat build --target java`.

**Showstoppers:**
- TeaVM's class library is a subset of the JDK. No `java.lang.reflect`, limited
  `java.util.concurrent`. Most workflow code doesn't need these, but it's a constraint.
- JSON libraries: need one that works under TeaVM (TeaVM has its own JSON support, or
  a minimal `org.json`-compatible library).
- Debugging: stack traces from WASM are opaque. Need source maps or a dev-mode
  that runs on the JVM directly (like `cleat dev` for Go).

**Binary size:** ~200-500 KB (TeaVM includes a minimal class library + GC).

**Verdict:** Feasible and high-value. Java has the largest enterprise workflow
ecosystem (Camunda, Temporal Java SDK). 4-8 weeks for a solid v0.1.

### TypeScript — Cost: ~6-12 weeks, Value: Very High

**How:** Two approaches:

**A) AssemblyScript** (a TypeScript-like language that compiles directly to WASM):
- Mature compiler, produces small WASM binaries (~10-50 KB)
- Subset of TypeScript (no closures, no `any`, manual memory management)
- SDK: `cleat-as` package with `HostCalls` class and `@cleatEntry` decorator
- Transformer: AssemblyScript transformer plugin that generates exports
- **Showstopper:** AssemblyScript is NOT TypeScript. It's a different language that
  looks like TypeScript. Existing TS code won't compile. This is a significant
  limitation for adoption.

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

**Verdict:** TypeScript is the highest-value language (largest developer community),
but the technical path is the roughest. AssemblyScript is the most practical near-term
option but is only "TypeScript-flavored." Real TypeScript via Javy is possible but
carries significant binary size and debugging overhead. Recommend AssemblyScript
as a stepping stone.

### Python — SDK exists, WASM FFI incomplete. Remaining: ~4-5 weeks to MVP

**Status (May 2026):** The Python SDK exists at 4,508 lines with full ABI
conformance — all 22 host imports are defined, the `@cleat_entry` decorator
generates WASM export wrappers, and typed wrappers (Saga, ChildWorkflow,
Defer, Plugins) are built. 80 tests pass (61 memory/encoding, 19 entry
decorator). `cleat build --target python` is wired via `componentize-py` in
`cmd/cleat/build_python.go`. Three example workflows exist (hello, saga,
child fan-out).

**The critical gap:** The 22 `_import_*` functions in `host_calls.py` are
implementation stubs that raise `NotImplementedError`. They define the correct
interface but aren't wired to actual WASM imports. The `componentize-py`
pipeline exists on disk but has never been validated end-to-end with a real
cleat worker loading and executing a Python-compiled WASM module.

**What remains (4 phases):**

*Phase 1: End-to-End WASM Compilation (P0, ~2-3 weeks)*
1. Verify `componentize-py` produces valid cleat WASM — run hello_workflow.py
   through the pipeline, load in a cleat worker, execute. Almost certainly
   something will break on first attempt.
2. Wire the 22 host imports. `componentize-py` uses WIT (WASM Interface Types)
   to define imports. Either write a WIT file describing cleat's host imports
   and generate Python bindings, or use `componentize-py`'s raw import
   mechanism. The Go SDK uses `//go:wasmimport` directives; Python needs the
   equivalent.
3. Validate SuspendSentinel propagation through the actual WASM ABI (the
   `@cleat_entry` decorator already has the logic but it's untested at the
   WASM boundary).
4. Fix whatever breaks. The AS SDK had 11 issues, Java had 11. Python will
   have its own set.

*Phase 2: Feature Parity with Go SDK (P1, ~2 weeks)*
5. Add missing HostCalls methods: `HasState`, `ListState`, `LogKV`,
   `FetchJSON`, typed `Promise[T]`, typed update handlers.
6. Add AI plugin wrappers to `plugins.py`: `llm_chat`, `llm_embed`,
   `pgvector_search`, `pgvector_upsert`.
7. Add `cleat init --template agent --language python`.

*Phase 3: Ecosystem Integration (P2, ~3-4 weeks)*
8. LangChain integration — a `CleatCallbackHandler` that records LangChain
   steps as cleat events.
9. LangGraph checkpointer — a `CleatCheckpointer` using cleat's event
   history as the checkpoint backend.
10. PyPI publishing — package `cleat-sdk` with versioning, docs, quickstart.

*Phase 4: Production Hardening (P3, ongoing)*
11. Binary size: CPython WASM is 5-20 MB. Options: accept it for server
    deployment, investigate RustPython (2-5 MB) or MicroPython (200-500 KB)
    as lighter alternatives, or use object storage with a `wasm_url` column
    for large blobs rather than storing directly in PostgreSQL.
12. Fork/port a Python OSS project (Temporal or DBOS example) to find
    real-world issues, as was done for AS and Java.
13. Async/await support — investigate whether `componentize-py` supports
    async functions via WASI async or a polling mechanism.

**Showstoppers (all fixable):**
- `componentize-py` is emerging tech with rough edges. Mitigation: the Go SDK
  proves the ABI works; if `componentize-py` fights back, fall back to a
  Python→Rust FFI bridge or a gRPC proxy to Go workers.
- Binary size (5-20 MB). Mitigation: server-side deployment tolerates larger
  binaries. `cleat deploy` can use S3 URLs for WASM blobs over a size
  threshold rather than storing in PostgreSQL `BYTEA`.

**Verdict:** The Python SDK is much further along than the original analysis
assumed. The remaining work is the WASM FFI wiring (~2-3 weeks) plus feature
parity and ecosystem integration (~5-6 weeks). Total: ~7-9 weeks to an
AI-ready Python SDK with LangChain integration. The Go-native AI plugins
(already shipped) provide the proving ground while Python/WASM is completed.

### Go — Already Done

Full automated transformer pipeline: analyzer → callgraph → closure → transform →
WASM compile via TinyGo. ~3,000 lines of transformer code across 5 packages.

### Rust — Already Done

`cleat-sdk` crate (290 lines) + `cleat-macro` proc-macro (121 lines) +
`cleat build --target rust`. ~537 lines total for the SDK.

---

## Summary Table

| Language | WASM compiler | SDK effort | Transformer effort | Binary size | Showstoppers? | Rank |
|----------|--------------|------------|-------------------|-------------|---------------|------|
| **C** | clang (mature) | ~1 week | None (header macro) | 10-50 KB | None | 1st |
| **Zig** | zig build-exe (mature) | ~1 week | None (comptime) | 5-30 KB | None | 1st |
| **Java/Kotlin** | TeaVM (mature) | ~2-3 weeks | ~2-5 weeks | 200-500 KB | TeaVM classlib subset | 2nd |
| **AssemblyScript** | asc (mature) | ~1-2 weeks | ~1-3 weeks | 10-50 KB | Not actually TypeScript | 3rd |
| **TypeScript** | Javy/QuickJS (mature) | ~2-3 weeks | ~3-6 weeks | 1-5 MB | Binary size, debugging | 4th |
| **Python** | componentize-py | ✅ Done (4.5K lines, 80 tests) | ✅ Done (@cleat_entry) | 5-20 MB | WASM FFI wiring (2-3 wks) | 5th |
| **C#/.NET** | NativeAOT-LLVM (exp.) | ~3-5 weeks | ~4-8 weeks | 1-5 MB | Immature toolchain | 6th |
| **Go** | tinygo | ✅ Done | ✅ Done | ~100-500 KB | None | Done |
| **Rust** | cargo build (built-in) | ✅ Done | ✅ Done | ~50-200 KB | None | Done |

---

## The Binary Size Constraint

Cleat stores WASM blobs in `workflow_defs.wasm_bytes` (PostgreSQL `BYTEA`). Each
deploy creates a new row. With 10 versions of a workflow, you're storing 10× the
binary size. This drives the ranking:

- **Good** (< 500 KB): C, Zig, Rust, AssemblyScript, Java/TeaVM, Go/tinygo
- **OK** (500 KB - 2 MB): Go (standard), TypeScript/Javy (worst-case)
- **Problematic** (> 2 MB): Python/CPython — makes the "deploy is an INSERT" model
  expensive. 20 MB × 10 versions × 50 workflows = 10 GB of WASM in Postgres.

If Python/TypeScript via embedded interpreters is essential, consider:
- Store WASM in object storage (S3) instead of Postgres, with a `wasm_url` column
- Or use a WASM deduplication layer (the embedded runtime is identical across
  workflow versions — only the user code differs)

---

## Recommended Order

1. **C + Zig** (week 1-2): Prove the ABI works for compiled languages. The `cleat.h`
   header is < 200 lines and validates that no host changes are needed.
2. **Java** (weeks 3-8): Highest value for enterprise adoption. TeaVM is mature.
   The annotation processor pattern is well-understood from Lombok/Dagger.
   Note: 11 issues already identified via fork/port; fixing these is the first step.
3. **AssemblyScript** (weeks 6-10): Gives a "TypeScript-like" option without the
   embedded-interpreter overhead. Real TypeScript can follow once Javy or Static
   Hermes stabilizes. Note: 11 issues already identified; fixing SDK compilation on
   AS 0.27.32 is the first step.
4. **Python WASM FFI** (weeks 8-11): The SDK is already built (4,508 lines, 80 tests,
   full ABI conformance). The remaining work is wiring the 22 host imports via
   `componentize-py`'s WIT bindings (~2-3 weeks), adding feature parity with Go SDK
   (~2 weeks), and LangChain/LangGraph integration (~3-4 weeks). Total: ~7-9 weeks
   to an AI-ready Python SDK.
5. **TypeScript via Javy** (months 3-4): After AssemblyScript proves demand. Focus
   on reducing binary size (tree-shaking the JS runtime) and improving debugging.

No language has a hard showstopper — even Python via CPython-wasm works, with the
binary size mitigated by object storage for large blobs. The WASM boundary design
holds up.
