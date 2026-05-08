# Transformer Analysis & Error-Checking Improvement Plan

## Summary

The Go transformer pipeline (analyzer → callgraph → closure → transform → wasm gen) is the
most mature, with 12 error codes, call-chain tracing, and structural validation. The other
four language SDKs (AssemblyScript, Rust, Java, Python) have **zero static analysis** beyond
entry-point detection and wrapper generation — no call-graph, no closure analysis, no
forbidden-API detection. This plan closes the gap systematically.

## Current State by Language

### Go (`internal/{analyzer,callgraph,closure,transform}`) — Good foundation, fixable gaps

**What works:**
- Package loading + type resolution via `go/packages` (`analyzer/loader.go`)
- Call graph construction with reverse edges (`callgraph/callgraph.go`)
- Durable closure computation (transitive reachability to HostCalls leaves) (`closure/closure.go`)
- Forbidden construct detection: goroutines (E001), channels (E002), time.Now (E003),
  time.Sleep (E004), net/http (E005), database/sql (E006), math/rand (E007),
  interface dispatch (E008), function-value calls (E009), os package (E010),
  reflect (E011), close() builtin (E012), map iteration (W001), floats in conditions (W002)
- HostCalls threading verification with call-chain tracing (`closure/threading.go`)
- Auto-threading: adds `h *cleat.HostCalls` to functions referencing global `var h` (`transform/transform.go`)
- WASM import/export + host adapter generation (`internal/wasm/`)

**Specific gaps:**

| # | Issue | File:Line | Severity |
|---|-------|-----------|----------|
| G1 | `checkForbiddenCall` misses `math/rand/v2` (only checks `math/rand`) | `closure/closure.go:292` | High |
| G2 | No detection of `sync.Mutex.Lock`, `sync.RWMutex`, `sync/atomic` — mutex ordering is non-deterministic across replays | `closure/closure.go:141` (missing case) | High |
| G3 | No detection of `time.After`, `time.NewTicker`, `time.NewTimer` — these create goroutines under the hood | `closure/closure.go:271` (only checks Now/Sleep) | High |
| G4 | No detection of `fmt.Println`, `fmt.Printf`, `log.Print*` — writes to stdout are non-deterministic | `closure/closure.go:141` (missing case) | Medium |
| G5 | No detection of `os.Getenv`, `os.Environ` — environment may differ across replays | `closure/closure.go:402` (os check only catches selector refs, not calls) | Medium |
| G6 | No detection of `os.Exit` — terminates WASM runtime, no replay possible | `closure/closure.go:141` (missing case) | Medium |
| G7 | No detection of `crypto/rand.Read` — cryptographically random, non-deterministic | `closure/closure.go:141` (missing case) | Medium |
| G8 | `encoding/json` with `interface{}` target — type-unstable unmarshaling can produce different value shapes across Go versions | `closure/closure.go:141` (missing case) | Low |
| G9 | Package-level `init()` functions that call durable functions are not analyzed — init runs once, durable calls inside it may not be replayable | `analyzer/loader.go:98` (skips init) | Medium |
| G10 | Error messages don't embed the error code in a machine-parseable prefix | `closure/closure.go:159-165` (code and message are separate fields) | Low |
| G11 | `checkForbiddenPackageRef` only triggers on `SelectorExpr` like `os.Open` — `import _ "os"` with side effects is silent | `closure/closure.go:389` | Low |
| G12 | `resolveImportPath` fallback uses `LastComponent` name matching, which can match wrong package (e.g., `rand` could be `crypto/rand` or `math/rand`) | `closure/closure.go:474-497` | Low |

### AssemblyScript (`packages/cleat-as/transform/index.js` + `cmd/cleat/build_as.go`) — Entry-point only, no analysis

**What works:**
- AST-based `afterParse` hook finds `@durableEntry` functions
- Renames original → `__durable_inner_X`, strips decorator
- Generates WASM export wrapper with JSON deserialization + HostCalls creation
- Multi-parameter support with type-aware JSON parsing (string, i32, i64, f64, bool)
- Fallback file output if AST injection fails

**What's missing:**

| # | Issue | File:Line | Severity |
|---|-------|-----------|----------|
| A1 | **No call graph** — never traces which functions call HostCalls methods; only the decorated entry point is known | `transform/index.js:40-53` | Critical |
| A2 | **No forbidden API detection** — AS `Math.random()`, `Date.now()`, `console.log()`, `process.*` are all silently accepted in durable functions | missing entirely | Critical |
| A3 | **No threading verification** — nothing checks that helper functions calling `h.DurableCall()` have access to `h` | missing entirely | Critical |
| A4 | **No closure analysis** — nothing determines which functions transitively reach HostCalls calls | missing entirely | Critical |
| A5 | **Error messages are `console.error()`** — not surfaced through the cleat CLI; `npm install` and `npx asc` errors are raw compiler output | `transform/index.js:71-76`, `cmd/cleat/build_as.go:69-72` | High |
| A6 | **No validation of entry function type signatures** beyond "has at least one param" — doesn't check that first param IS HostCalls | `transform/index.js:70-77` | High |
| A7 | **Transform code has zero tests** — no unit tests for `_findDurableEntries`, `_extractEntryInfo`, `_generateWrappers`, etc. | `packages/cleat-as/` (no transform tests) | High |
| A8 | **No error for entry points without a `@durableEntry` decorator** — silent no-op | `transform/index.js:46` (returns early if no entries) | Medium |
| A9 | **Type deserialization throws at transformer time** for unsupported types (good) but doesn't include source location | `transform/index.js:434-439` | Medium |
| A10 | **`try/catch` in `_injectWrappers` silently swallows all errors** — falls back to file output without explaining why | `transform/index.js:213-216` | Medium |
| A11 | **SUSPEND_SENTINEL bit overlap bug** (documented in DX_COMPARISON, line 140-142) — bit 62 overlaps with signal name length field | `transform/index.js` wrapper generation | High |
| A12 | **AS runtime `--runtime stub` has no try/catch, no closures** — the SDK cannot use these features, but the transformer doesn't check for them in user code | missing entirely | Medium |

### Rust (`crates/cleat-macro/src/entry.rs`) — Proc-macro only, single-function scope

**What works:**
- `#[cleat_entry]` proc-macro attribute
- Spanned compile errors for missing/multiple parameters
- Type-safe Serde JSON deserialization
- `std::panic::catch_unwind` with `SuspendSentinel` detection
- Clean ABI wrapper: `(args_ptr, args_len, out_ptr, max_out_len) -> i64`

**What's missing:**

| # | Issue | File:Line | Severity |
|---|-------|-----------|----------|
| R1 | **No call graph or closure analysis** — proc macro only sees the single annotated function, not its callees | `entry.rs:1-139` (entire file) | Critical |
| R2 | **No forbidden API detection** — `std::fs::read_to_string`, `std::net::TcpStream`, `std::process::Command`, `rand::random`, `std::time::SystemTime::now` are all silently accepted inside workflows | missing entirely | Critical |
| R3 | **No threading verification** — helper functions that call `h.cleat_call()` but lack `&HostCalls` param won't be caught until compilation fails with missing variable | missing entirely | Critical |
| R4 | **Artificial single-parameter limit** — `#[cleat_entry]` only accepts exactly one input parameter beyond `&HostCalls`; Go supports multiple, AS supports multiple via JSON extraction | `entry.rs:29-39` | Medium |
| R5 | **No validation that input type implements `Deserialize`** at macro level — only caught later by serde compilation error | `entry.rs:73-74` (blindly calls `serde_json::from_str`) | Low |
| R6 | **No validation that return type implements `Serialize`** — caught later by `cleat_sdk::format_cleat_result` | `entry.rs:116-126` | Low |
| R7 | **No test coverage for the macro error paths** — `tests/basic.rs` exists but likely only tests happy path | `crates/cleat-macro/tests/basic.rs` | Medium |
| R8 | **`catch_unwind` may be expensive on WASM targets** — no profiling data on this | `entry.rs:110` | Low |

### Java (`crates/cleat-java/.../CleatEntryProcessor.java`) — Annotation processor only

**What works:**
- `@CleatEntry` annotation + `CleatEntryProcessor` (javax.annotation.processing)
- Validates `@CleatEntry` is on methods only (not classes, fields)
- Parameter type validation (`isSupportedParameterType`)
- Generates WASM export wrapper class + `CleatEntryIndex` aggregator
- `WorkflowEntry` generation for TeaVM tree-shaking prevention

**What's missing:**

| # | Issue | File:Line | Severity |
|---|-------|-----------|----------|
| J1 | **No call graph or closure analysis** — annotation processor only sees the annotated method, not its callees | `CleatEntryProcessor.java:1-451` | Critical |
| J2 | **No forbidden API detection** — `System.currentTimeMillis()`, `java.io.*`, `java.net.*`, `java.sql.*`, `Math.random()`, `Thread.sleep()` are all silently accepted | missing entirely | Critical |
| J3 | **No threading verification** — Java has no `HostCalls` parameter threading concept since `HostCalls` is created locally; but helper methods silently fail if they call host functions without proper setup | missing entirely | Critical |
| J4 | **`JsonHelper.parse()` only supports `String.class`** — all complex inputs must be pre-serialized (documented in DX_COMPARISON, line 111) | `CleatEntryProcessor.java:235` | High |
| J5 | **No validation that the method is `public static`** — only checks for ElementKind.METHOD | `CleatEntryProcessor.java:71-78` | High |
| J6 | **Processor is `@SupportedSourceVersion(RELEASE_11)`** — may not work with Java 17+ features | `CleatEntryProcessor.java:56` | Medium |
| J7 | **I/O exceptions during file generation are logged but don't fail the build** — annotation processors should use `processingEnv.getMessager().printMessage(ERROR)` | `CleatEntryProcessor.java:170-175` | Medium |
| J8 | **No test coverage for the annotation processor** — requires javac compilation tests | missing entirely | High |
| J9 | **TeaVM tree-shaking can still remove exports** if `WorkflowEntry` isn't configured as `mainClass` — no warning when this happens | `CleatEntryProcessor.java:419-451` | Medium |

### Python (`cmd/cleat/build_python.go` + `python-sdk/scripts/build_wasm.py`) — No static analysis at all

**What works:**
- Auto-detects `@cleat_entry` decorated functions via string scanning
- `componentize-py` pipeline compiles Python → WIT → WASM component
- Comprehensive SDK with 34 WIT imports, 80+ tests, LangChain integration

**What's missing:**

| # | Issue | File:Line | Severity |
|---|-------|-----------|----------|
| P1 | **No static analysis whatsoever** — any Python is accepted; `open()`, `requests.get()`, `time.sleep()`, `random.random()` all pass through silently | `cmd/cleat/build_python.go:1-164` (entire file) | Critical |
| P2 | **`detectEntryFunction` is a fragile string scan** — confused by `@cleat_entry` inside comments or multi-line strings; no AST-level understanding | `cmd/cleat/build_python.go:132-163` | High |
| P3 | **No validation of the function signature** — doesn't check that first param is `HostCalls` or that it has correct arity | `cmd/cleat/build_python.go:132-163` | High |
| P4 | **`componentize-py` errors are raw tool output** — no filtering, no suggestion, no error code | `cmd/cleat/build_python.go:99-103` | High |
| P5 | **`os.Exit(1)` on every failure** — no partial results, no JSON error output for tooling | every error path in `build_python.go` | Medium |
| P6 | **No validation that `--entry` function actually exists** — will fail at componentize-py time with confusing error | `cmd/cleat/build_python.go:99` | Medium |
| P7 | **No WIT stub generation** — the SDK has 34 WIT imports but the build pipeline doesn't generate the WIT file; the user must provide it | `cmd/cleat/build_python.go:99` (calls `wasm.BuildPythonWasm`) | High |
| P8 | **Python's dynamic features** (eval, exec, getattr, setattr, monkey-patching) — none are detected or rejected; they could break replay determinism | missing entirely | Medium |

## Cross-Cutting Issues

| # | Issue | Severity |
|---|-------|----------|
| X1 | **"cleat vet" doesn't exist** — the transformer plan (Phase 9) specifies a read-only analysis command for CI; it's only implemented for Go | Critical |
| X2 | **No shared validation rule definitions** — forbidden APIs for Go are hardcoded in `closure.go`; other languages duplicate the concept ad-hoc or not at all | High |
| X3 | **Error output format is inconsistent** — Go has structured `ValidationError` with code/line/suggestion; AS has `console.error()`; Rust has `compile_error!`; Java has `printMessage(ERROR)`. No JSON output mode exists for any language. | High |
| X4 | **Entry point detection mechanisms differ per language** — Go: HostCalls param; AS: `@durableEntry`; Rust: `#[cleat_entry]`; Java: `@CleatEntry`; Python: string scan for `@cleat_entry`. This inconsistency is a DX papercut. | Medium |
| X5 | **No WASM export validation** — after compilation, no tool checks that the WASM binary exports the expected functions with correct signatures | Medium |
| X6 | **No end-to-end transform tests for non-Go languages** — Go has `transform_test.go` with 10+ test cases; AS/Java/Python/Rust transforms have zero tests | Critical |
| X7 | **Build commands (`build_as.go`, `build_rust.go`, `build_python.go`, `build_java.go`) have no error recovery** — every failure path is `fmt.Fprintf + os.Exit(1)`. No structured error format for editor/CI integration. | Medium |
| X8 | **WASM binary validation is ad-hoc** — Go build pipeline does `filepath.Glob` for .wasm files; no check for valid WASM magic bytes, no check for imports/exports, no size sanity check | Medium |

## Implementation Plan

### Phase 1: Go Gap Closure (Week 1)

Close the 12 gaps in the Go transformer — the foundation that other languages will follow.

1. **Add missing forbidden API detectors** (G1-G7):
   - `math/rand/v2` (G1)
   - `sync.Mutex`, `sync.RWMutex`, `sync/atomic` (G2)
   - `time.After`, `time.NewTicker`, `time.NewTimer` (G3)
   - `fmt.Print*`, `log.Print*` (G4)
   - `os.Getenv`, `os.Environ`, `os.Exit` (G5, G6)
   - `crypto/rand.Read` (G7)
   - Each gets an error code in the E013-E019 range

2. **Fix `resolveImportPath` ambiguity** (G12):
   - When multiple imports match a package name, use the type-checked `Info.Uses` path
   - Fall back to partial-name matching only when type info is unavailable

3. **Add `init()` analysis** (G9):
   - Walk `init` functions in the target package
   - If they call any durable leaf or durable closure function, emit E020: "init() functions cannot make durable calls"

4. **Add machine-parseable error output**:
   - `--json` flag on `cleat build` outputs diagnostics as JSON array
   - Each entry: `{code, file, line, col, message, suggestion, chain}`
   - Also emit to stderr in GitHub Actions format: `::error file=order.go,line=42::E001 goroutine in durable function`

### Phase 2: AssemblyScript Static Analysis (Week 2-3)

Add a TypeScript-based static analysis layer that runs before (or as part of) the AS transform.

1. **Implement call graph construction in the AS transformer**:
   - Walk all statements in `parser.program.sources` after parsing
   - Build caller→callee edges for function declarations and call expressions
   - Identify HostCalls method calls as durable leaves
   - Compute transitive closure

2. **Implement forbidden API detection**:
   - `Math.random()` → error: "use h.Random() instead"
   - `Date.now()` → error: "use h.Now() instead"
   - `console.log()` → error: "use h.DurableLog() instead"
   - `process.*` → error: "process access is not allowed in workflow code"
   - `Math.seedRandom()` → error (same as Math.random())

3. **Implement HostCalls threading verification**:
   - Check that functions in the durable closure have `h` available (via param or global)
   - Emit errors with call chains when `h` is missing

4. **Improve error reporting**:
   - Add `--json` output mode at the `cleat build` level for AS
   - Surface AS transform errors through the CLI, not just `console.error()`
   - Include source locations (file, line, column) in all errors

5. **Add transform tests**:
   - Unit tests for `_findDurableEntries`, `_extractEntryInfo`, `_generateWrappers`
   - Integration tests: given AS source → run transform → verify generated code
   - Error fixture tests: verify each forbidden pattern produces the right error

6. **Fix SUSPEND_SENTINEL bug** (A11):
   - Audit the bit layout; ensure SUSPEND_SENTINEL doesn't overlap with any field

### Phase 3: Rust & Java Static Analysis (Week 4-5)

For both Rust and Java, the approach is the same: add a standalone analysis tool (not just a macro/annotation processor) that can validate the full source tree.

3a. **Rust** — create a subcommand `cleat vet --lang rust` that:
   - Parses Rust source files (using `syn` or `rust-analyzer` as a library)
   - Builds call graph across the crate
   - Identifies functions in the durable closure
   - Checks for forbidden APIs (see R2 above)
   - Verifies HostCalls threading
   - Outputs structured errors in the same JSON format as Go

3b. **Java** — create a subcommand `cleat vet --lang java` that:
   - Uses `com.github.javaparser` or the javac `CompilationTask` API
   - Visits all method bodies and builds call graph
   - Identifies durable closure (methods that reach HostCalls calls)
   - Checks for forbidden APIs (see J2 above)
   - Outputs structured errors

### Phase 4: Python Static Analysis (Week 5-6)

Python needs the most work since it currently has zero analysis.

1. **Implement `cleat vet --lang python`** using the `ast` module:
   - Parse Python source files
   - Build call graph via AST visitor
   - Identify durable closure: functions that reach `cleat_call("...", "...", ...)` or other SDK methods
   - Detect forbidden Python APIs:
     - `open()` → error
     - `requests.*`, `urllib.*` → error (direct HTTP)
     - `time.sleep()`, `time.time()` → error
     - `random.*` → error
     - `os.*`, `subprocess.*` → error
     - `threading.*`, `asyncio.*` → error
     - `socket.*` → error
     - Decorated entry function without proper HostCalls param → error
   - Verify HostCalls threading through the call chain

2. **Replace `detectEntryFunction` string scan with proper AST parsing**:
   - Parse the file with `ast.parse()`
   - Walk for `FunctionDef` nodes with `@cleat_entry` decorator
   - Extract function name, parameter names, types

3. **Add WIT generation**:
   - Scrape the Python SDK's WIT imports
   - Generate a `.wit` file for `componentize-py`

### Phase 5: Unified Tooling (Week 6-7)

1. **Implement `cleat vet` command** that dispatches to the right language analyzer:
   ```
   cleat vet ./workflows/          # auto-detect language
   cleat vet --lang go ./wf/       # explicit
   cleat vet --lang as ./as-wf/
   cleat vet --lang rust ./rust-wf/
   cleat vet --lang java ./java-wf/
   cleat vet --lang python ./py-wf/
   ```

2. **Common JSON error format** across all languages:
   ```json
   {
     "errors": [
       {
         "code": "E001",
         "file": "order.go",
         "line": 42,
         "column": 2,
         "message": "goroutines are not allowed in durable functions",
         "suggestion": "Use child workflows (h.ChildWorkflow) for parallelism.",
         "chain": ["PlaceOrder", "processAsync"]
       }
     ],
     "warnings": [...],
     "summary": {"functions": 12, "durable_leaves": 3, "durable_closure": 5, "pure": 4}
   }
   ```

3. **Editor integration**:
   - Output in GitHub Actions annotation format for CI
   - Consider LSP diagnostics (future)

### Phase 6: End-to-End Tests (Week 7-8)

1. **Create shared test fixtures** for cross-language validation:
   - Each forbidden pattern → test case in all 5 languages
   - Verify the same error code is produced (or language-appropriate equivalent)
   - `testdata/vet-checks/` directory with per-language subdirectories

2. **Add transform integration tests** for AS, Rust macro, Java processor:
   - Given source → run the transform/processor → verify output compiles
   - Verify error cases produce correct messages at the right locations

3. **Add WASM export validation tests**:
   - Compile a workflow from each language
   - Use `wazero` or `wasm-tools` to verify exported functions
   - Verify all imports exist

## Priority Triage

**Week 1-2 (immediate):** Phase 1 — Go gaps (G1-G7, G9, G12) + `cleat vet` for Go
**Week 3-4:** Phase 2 — AssemblyScript static analysis (A1-A4, A6, A8)
**Week 5-6:** Phase 3 — Rust + Java simple vet (R2-R3, J2-J3)
**Week 7-8:** Phase 4 — Python vet (P1-P3)
**Week 9-10:** Phase 5 — Unified tooling + Phase 6 — Tests

## Success Metrics

- `cleat vet` catches 100% of the forbidden API patterns in the test fixtures
- Error messages always include: error code, file:line, what's wrong, and how to fix it
- Call chain is shown for every threading error
- All 5 languages produce the same JSON error format
- `cleat build` fails with a clear error before compilation when analysis fails
- No silent passes — every invalid workflow is caught statically
