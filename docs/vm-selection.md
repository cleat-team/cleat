# VM Selection: Why WASM

May 2026. Analysis of execution-VM options for cleat's language-agnostic
workflow runtime, and why WASM is the correct choice despite its toolchain
immaturity.

---

## 1. Hard Requirements

Cleat's architecture imposes four non-negotiable constraints on the VM:

1. **Deterministic replay.** Given the same initial state and event history,
   execution must produce bit-for-bit identical results. This is the hardest
   constraint and eliminates most candidates.

2. **Language agnosticism.** Must compile Go, Rust, Java/Kotlin, TypeScript/
   AssemblyScript, and potentially Python to a common target.

3. **Sandboxing.** Workflow code must not access the network, filesystem, or
   non-deterministic system calls. Sandboxing must be enforced by the runtime,
   not by developer discipline or runtime detection.

4. **Small deployable artifacts.** Compiled blobs are stored in PostgreSQL and
   deployed via INSERT. Multi-megabyte artifacts per workflow version would
   bloat the database and slow deployments.

5. **Clean host ABI.** FFI for calling host functions (DurableCall, PluginCall,
   sleep, await signals) with minimal serialization overhead.

---

## 2. Candidates

### WASM (current choice)

**What it gets right:**

- **Deterministic by design.** No inherent sources of non-determinism — no
  `time.Now()`, no random, no GC interference visible to the program. The
  entire execution model is a pure function of (state, imports). This is the
  killer feature for replay.
- **Sandboxed from the ground up.** No filesystem, no network, no OS access
  unless explicitly granted via imports. This isn't bolted on — it's the
  architecture.
- **Module isolation.** Each compiled workflow is a self-contained module with
  its own linear memory. Multiple versions coexist trivially.
- **Small binaries.** Go workflow WASM binaries are typically 100KB-2MB.
  JVM equivalents would be 10-50x larger.
- **Growing compiler support.** Go (1.26+ native), Rust (native), Java/Kotlin
  (TeaVM), C/C++ (clang), Python (componentize-py), Zig, Swift.

**Current weaknesses:**

- **Immature non-Go toolchains.** The 38 fork/port issues are direct evidence —
  AS SDK doesn't compile on current AS 0.27.32, TeaVM has 6 blockers, Python
  WASM binaries are ~20MB.
- **Go version lock-in.** Requires Go 1.26+ for `//go:wasmimport` and
  `//go:wasmexport`. This is bleeding-edge (May 2026). TinyGo fallback exists
  but uses a language subset.
- **Debugging is primitive.** No step-through debugger for WASM workflows.
  Debugging is via event history inspection.
- **GC within WASM is per-module and not shared with the host.**

---

### JVM

**Why it seems attractive:**

- 30 years of maturity, massive language ecosystem (Java, Kotlin, Scala,
  Clojure).
- Excellent tooling (debuggers, profilers, monitoring, JFR).
- Class versioning is built-in — different classloader instances for different
  versions of the same class.
- GraalVM native image for AOT compilation and reduced startup time.

**Why it fails cleat's requirements:**

- **Non-determinism is pervasive.** `System.currentTimeMillis()`,
  `System.nanoTime()`, `java.util.Random`, `HashMap` ordering,
  `Object.hashCode()` (default impl uses memory address), `IdentityHashMap`,
  `WeakHashMap`, thread scheduling, GC timing, `System.identityHashCode()`.
  Temporal's Java SDK spends enormous effort detecting and preventing
  non-determinism at runtime with a static analyzer that must reject valid
  code. Cleat catches it at build time via Go's WASM restrictions — the JVM
  would require runtime detection, which is strictly worse.

- **Sandboxing is removed.** The JVM SecurityManager was deprecated in Java 17
  (JEP 411) and removed in Java 21. There is no supported, production-ready
  way to prevent workflow code from calling `System.exit()`, opening sockets,
  or reading files. Custom classloaders and bytecode rewriting can approximate
  sandboxing but are fragile, slow, and not security-hardened.

- **GC is non-deterministic.** Stop-the-world pauses happen at unpredictable
  times. If a workflow's event history records a GC pause, replay must
  reproduce the same pause, which is impossible. WASM runtimes have GC
  available but cleat's Go compiler doesn't use it.

- **Artifact sizes are large.** A minimal JVM workflow JAR is 5-50MB including
  dependencies. WASM workflows are 100KB-2MB. For versioned blobs stored in
  PostgreSQL, this matters.

- **Startup cost.** JVM cold start is 100ms-2s. WASM instantiation is
  microseconds. For a worker processing thousands of short workflows, this
  accumulates.

---

### eBPF

Deterministic and sandboxed by construction, but the instruction set is too
limited for general-purpose business logic. No high-level language compilers
target eBPF with support for JSON parsing, string manipulation, complex control
flow, or standard libraries. Non-starter for a general-purpose workflow engine.

---

### Lua (sandboxed via lua_sandbox or similar)

Lightweight, fast, embeddable, and can be made deterministic by removing
`os.time()`, `math.random()`, and similar functions. Used by Redis for
server-side scripting. But single-language only. Cleat's value proposition
is multi-language via WASM — dropping to a single scripting language
eliminates the architectural differentiator that competes with Temporal
and DBOS.

---

### V8 Isolate / QuickJS

JavaScript is widely known with good sandboxing (Cloudflare Workers, Deno
Deploy). But single-language, GC timing is non-deterministic (V8's GC is
stop-the-world and scheduling varies), and supporting typed workflows with
strong type guarantees is harder. The platforms that use V8 isolates don't
need deterministic replay — they execute functions once and return results.

---

### Process checkpointing (CRIU)

An entirely different approach: instead of a VM, snapshot the process state.
Works with any language, no compilation needed. Used by some academic durable
execution systems. But checkpoints are large (10-100MB), fragile across kernel
versions and library versions, OS-specific (Linux only), and impossible to
replay deterministically — you can restore a checkpoint but not re-execute
from event history with bit-identical results. This fundamentally fails the
replay requirement.

---

## 3. Summary

| | Deterministic | Sandboxed | Multi-language | Small artifacts |
|---|---|---|---|---|
| **WASM** | Yes (by design) | Yes (by design) | Yes (growing) | Yes (100KB-2MB) |
| JVM | No (pervasive sources) | No (SecurityManager removed) | Yes (best in class) | No (5-50MB) |
| eBPF | Yes | Yes | No | Yes |
| Lua | Yes (with work) | Yes (with work) | No | Yes |
| V8/JS | No (GC timing) | Yes | No | Acceptable |
| CRIU | No (not replayable) | N/A (checkpoints) | Yes | No (10-100MB) |

---

## 4. Verdict

**Stick with WASM.** The alternatives all fail at least one of cleat's hard
requirements. The JVM's non-determinism and sandboxing problems are
architectural — they can't be fixed without rebuilding the JVM. WASM's
problems are toolchain maturity problems — they're fixable one by one.

The 38 fork/port issues aren't a WASM problem — they're a toolchain maturity
problem. The AS SDK compilation error, the TeaVM blockers, the Python WASM
binary size — these are all fixable with engineering effort. The JVM's
non-determinism is not fixable without forking the JVM.

**The one thing to watch:** the WASM Component Model. When it stabilizes, it
could provide a standardized ABI that makes multi-language support cleaner
(typed imports/exports instead of raw linear memory). It's not ready today,
and cleat doesn't need it to ship, but it could simplify the multi-language
story when it matures.

## 5. When to Revisit

Re-evaluate if:
- A new WASM alternative emerges with deterministic replay and multi-language
  support (unlikely in the next 2-3 years)
- The WASM Component Model stabilizes and all target languages support it
- The non-Go toolchains (AS, Java, Python) prove too expensive to maintain
  and a single-language (Go-only) strategy becomes preferable — at which
  point Lua or a Go interpreter could be considered for scripting use cases
