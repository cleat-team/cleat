# Implementation Plan: Java/Kotlin and TypeScript Language Support

## Context

Extend cleat's WASM-based workflow model to Java/Kotlin (via TeaVM) and TypeScript
(via AssemblyScript). The Rust pipeline (`crates/durable-sdk/` + `crates/durable-macro/`
+ `durable build --target rust`) is the reference architecture. Full research in
`LANGUAGE_SUPPORT.md`.

## Java/Kotlin via TeaVM (4-8 weeks)

TeaVM is an AOT compiler that translates JVM bytecode to WASM. It targets **Wasm GC**
(not plain wasm32). wazero 1.2+ supports Wasm GC.

### Files to create

```
crates/durable-java/
  build.gradle.kts           # Gradle build with TeaVM plugin
  src/main/java/cleat/
    HostCalls.java           # 15 @Import declarations + safe wrappers
    Memory.java              # String read/write, bit-packing decoders
    DurableEntry.java        # @DurableEntry annotation
    DurableEntryProcessor.java # Annotation processor (javac plugin)
    DurableResult.java       # Result type for success/error
  src/test/java/cleat/
    HostCallsTest.java       # Unit tests
examples/java-workflow/
  build.gradle.kts           # Example workflow build
  src/main/java/
    PlaceOrder.java          # @DurableEntry example

cmd/durable/build_java.go    # CLI: gradle build + find .wasm
```

### Phase 1: HostCalls SDK (week 1-2)

**HostCalls.java** (~300 lines). A class with 15 `static native` methods annotated
with `@Import(module = "env", name = "durable_call")`. Methods take `int` for
pointers/lengths, `long` for i64 params, return `long` (packed i64). Public wrapper
methods with idiomatic Java signatures (String params, Result return types) delegate
to native imports, handling String↔memory translation.

```java
@Import(module = "env", name = "durable_call")
private static native long durableCallRaw(int svcPtr, int svcLen,
    int opPtr, int opLen, int reqPtr, int reqLen,
    int respPtr, int respMaxLen);

public static DurableResult<String> durableCall(
    String service, String operation, String requestJSON) {
    byte[] reqBytes = requestJSON.getBytes(StandardCharsets.UTF_8);
    // Write to scratch region, call raw import, decode response...
}
```

**Memory.java** (~150 lines). Static helpers:
- `readString(ptr: int, len: int): String` — reads UTF-8 from linear memory
- `writeString(ptr: int, maxLen: int, s: String): int` — writes to memory, returns bytes written
- Bit-packing decoders matching the Rust `memory.rs` patterns
- Constants: OUT_BUF_SIZE, SCRATCH_BASE, SUSPEND_SENTINEL

Uses `java.nio.ByteBuffer.allocateDirect()` for linear memory access under TeaVM.

### Phase 2: Annotation Processor (week 3-5)

**@DurableEntry** annotation and processor:

```java
@Retention(RetentionPolicy.SOURCE)
@Target(ElementType.METHOD)
public @interface DurableEntry {
    String name() default ""; // export name, defaults to method name
}
```

**DurableEntryProcessor.java** — standard `javax.annotation.processing.AbstractProcessor`.
At compile time, finds `@DurableEntry` methods and generates companion classes with:

1. A `@Export(name = "...")` method with signature `(int, int, int, int) -> long`
2. JSON deserialization (bundled library or code-generated parser)
3. HostCalls construction and inner method invocation
4. JSON serialization of return value
5. `encodeExportResult(errCode, actualLen)` return encoding

The processor runs during `javac` compilation (before TeaVM). TeaVM then compiles both
the user's code and the generated wrappers into WASM.

### Phase 3: Build Integration (week 5-6)

**cmd/durable/build_java.go:**

```go
func runBuildJava(pattern, outDir string) {
    // 1. Validate build.gradle.kts exists
    // 2. Check for gradle on PATH
    // 3. Run `gradle buildWasmGC`
    // 4. Locate output .wasm in build/generated/teavm/
    // 5. Copy to outDir
}
```

Add `--target java` to `cmd/durable/main.go`. The TeaVM `.wasm` requires companion
runtime imports (`teavmConsole`, `teavmDate`, `teavmMath`) — register stub modules
in the cleat host.

### Phase 4: Testing (week 7-8)

Port the Rust integration tests from `internal/host/rust_workflow_test.go`:
`TestJavaWorkflowExecute`, `TestJavaWorkflowReplay`, `TestJavaWorkflowCompensation`.

### Risks and Mitigations

| Risk | Mitigation |
|------|-----------|
| Wasm GC required | Require wazero 1.2+; add runtime check |
| TeaVM runtime imports | Register stub modules in cleat host |
| No built-in JSON | Bundle minimal JSON lib or code-gen in annotation processor |
| Binary size (200KB-2MB) | Acceptable; still deployable in Postgres |
| TeaVM classlib gaps | Document: no threads, limited reflection |

---

## TypeScript via AssemblyScript (2-3 weeks)

AssemblyScript compiles directly to plain wasm32. It is NOT full TypeScript (no
closures, no async/await, no `any`), but its memory model, binary size, and WASM
control are a perfect fit for the cleat ABI.

### Files to create

```
packages/cleat-as/
  package.json              # npm package: @cleat/sdk
  assembly/
    host-calls.ts           # 15 @external declarations + HostCalls class
    memory.ts               # String read/write, bit-packing helpers
    durable-entry.ts        # @durableEntry decorator
    index.ts                # Re-exports
  transform/
    index.js                # AssemblyScript transformer plugin
    package.json            # npm package: @cleat/transform
examples/as-workflow/
  package.json
  asconfig.json
  assembly/
    place-order.ts          # @durableEntry example

cmd/durable/build_as.go     # CLI: npx asc + find .wasm
```

### Phase 1: AssemblyScript SDK (week 1)

**host-calls.ts** (~200 lines). Declare all 15 host functions:

```typescript
@external("env", "durable_call")
export declare function durable_call_raw(
  svcPtr: usize, svcLen: i32,
  opPtr: usize, opLen: i32,
  reqPtr: usize, reqLen: i32,
  respPtr: usize, respMaxLen: i32
): i64;

@external("env", "durable_sleep")
export declare function durable_sleep(durationMs: i64): i64;
// ... 13 more
```

`HostCalls` class wraps the raw imports with idiomatic methods handling string
serialization, bit unpacking, and JSON marshalling.

**memory.ts** (~100 lines). Ports the Rust `memory.rs` bit-packing functions.
AssemblyScript uses `i32`/`i64` directly; unsigned right shifts need explicit handling.

### Phase 2: Transformer Plugin (week 2)

**transform/index.js** (~150 lines). An AssemblyScript AST transformer that:

1. Visits each function declaration, checking for `@durableEntry`
2. Generates a new `export function` with the ABI signature:
   `(argsPtr: usize, argsLen: i32, outPtr: usize, maxOutLen: i32): i64`
3. The generated function reads input JSON, deserializes, calls user function,
   serializes result, writes to output buffer, and returns packed i64

### Phase 3: Build Integration (week 2-3)

**cmd/durable/build_as.go:**

```go
func runBuildAssemblyScript(pattern, outDir string) {
    // 1. Validate package.json and asconfig.json exist
    // 2. Check for npx/node on PATH
    // 3. Run `npx asc assembly/index.ts -o dist/workflow.wasm
    //    --transform @cleat/transform --runtime stub
    //    --initialMemory 170 --optimize`
    // 4. Locate output .wasm and copy to outDir
}
```

Add `--target assemblyscript` to `main.go`.

### Phase 3b: Real TypeScript via Javy (follow-on, week 3+)

Optional follow-on for real TypeScript using Javy (QuickJS-in-WASM). Requires a
custom Javy Rust plugin bridging JS ↔ WASM memory. Binary sizes 1.5-5 MB.
AssemblyScript covers 80% of use cases at 5% of the binary size.

### Constraints and Mitigations

| Constraint | Mitigation |
|-----------|-----------|
| AssemblyScript ≠ TypeScript | Document; target AS-first, TS/Javy as follow-on |
| No closures in AS | Workflow code is naturally procedural |
| Heap/scratch region conflict | Set `--initialMemory 170` (10+ MiB); AS heap stays below 0xA00000 |
| No async/await | Not needed — cleat workflows are synchronous, suspending via host calls |

---

## Comparison

| Factor | Java/Kotlin (TeaVM) | TypeScript (AssemblyScript) |
|--------|---------------------|---------------------------|
| SDK effort | 2-3 weeks | 1-2 weeks |
| Transformer effort | 3-5 weeks (annotation processor) | 1-2 weeks (AST transformer) |
| Build integration | 1 week (gradle) | 0.5 weeks (npx asc) |
| Total | 4-8 weeks | 2-3 weeks |
| Binary size | 200 KB - 2 MB | 5-50 KB |
| Language fidelity | Full Java/Kotlin | TypeScript subset |
| WASM requirement | Wasm GC (wazero 1.2+) | Plain wasm32 |
| Enterprise appeal | High | Medium |
| Risk | TeaVM classlib gaps | AS ≠ TS limitation |
| Recommended order | Second | First |

## Recommended Order

1. **AssemblyScript** (weeks 1-3): Fastest path to a working second dynamic language.
   Validates the transformer-plugin pattern outside Rust. Binary sizes excellent.

2. **Java/Kotlin via TeaVM** (weeks 4-8): Higher effort, higher enterprise value.

## Verification

For both languages, port the 4 integration tests from
`internal/host/rust_workflow_test.go`:
1. Fresh execution with mock caller
2. Deterministic replay returns same result
3. Cancellation path
4. Compensation path after mid-workflow failure
