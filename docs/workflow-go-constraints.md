# Go Workflow Constraints

Workflow code compiled to WebAssembly and executed by cleat operates under
significant constraints compared to standard Go programs. This document
catalogs the allowed and forbidden Go standard library packages, explains why
each restriction exists, and provides migration guidance.

---

## How Constraints Are Enforced

The `cleat build` pipeline includes a static analysis pass (`cleat vet`) that
scans the transitive closure of workflow functions and rejects unsupported
Go constructs. This is not a runtime check — violations are caught at build
time with specific error codes.

**Build-time enforcement** means you discover constraint violations during
development, not during replay in production.

---

## 1. Forbidden Standard Library Packages

The following packages are **blocked** in workflow code. Using any of them
produces a build-time error with the listed code.

### `os` (E010, E016)

**Error codes**: E010 (package reference), E016 (function call)

**Why blocked**: The `os` package provides access to environment variables,
filesystem operations, and process management — all of which differ across
replays. A workflow that reads a file during original execution may find the
file missing or changed during replay.

**What to use instead**:
- **Files**: Pass file contents as workflow input, or use `h.DurableCall()` to
  a service that reads the file
- **Environment variables**: Pass configuration as workflow input parameters
- **Process management**: Use child workflows (`h.ChildWorkflow()`) instead of
  spawning OS processes

### `reflect` (E011)

**Error codes**: E011

**Why blocked**: Reflection results can differ across Go versions and build targets.
Type identities, method sets, and struct field layouts are not guaranteed to be
identical between Go and TinyGo, or between different Go versions. This breaks
deterministic replay because a workflow that uses `reflect` during original
execution may get different reflection results during replay.

**What to use instead**:
- Type switches (`switch v.(type)`)
- Compile-time generics (Go 1.18+)
- Code generation for enum-like patterns

### `net/http` and `net/http/*` (E005)

**Error codes**: E005

**Why blocked**: Direct HTTP calls introduce non-determinism from network
failures, DNS resolution differences, timeout behavior, and TLS certificate
variation across replays.

**What to use instead**: Use `h.DurableCall()` or `h.DurableFetch()` with a
service name. The host handles the actual HTTP call, records the response in
event history, and replays it deterministically.

### `database/sql` (E006)

**Error codes**: E006

**Why blocked**: Direct database calls produce non-replayable side effects. A
workflow that inserts a row during original execution will attempt the same
insert during replay, potentially causing duplicate key errors or data corruption.

**What to use instead**: All external side effects must go through `h.DurableCall()`.
Create a service or microservice that performs the database operation, and call
it via `h.DurableCall()`.

### `math/rand` and `math/rand/v2` (E007, E018)

**Error codes**: E007 (math/rand), E018 (math/rand/v2)

**Why blocked**: Both versions seed from wall-clock time by default, producing
different random sequences on every replay. Even with an explicit seed, the
internal algorithm may differ between Go versions.

**What to use instead**: `h.Random()` provides deterministic randomness.
During original execution it returns cryptographically random bytes; during
replay it returns the bytes recorded in event history, guaranteeing identical
results.

### `crypto/rand` (E017)

**Error codes**: E017

**Why blocked**: Reads from OS entropy sources (`/dev/urandom`, `getrandom(2)`),
which return different values on every call, making replay diverge.

**What to use instead**: `h.Random()` for deterministic randomness that is safe
for replay.

### `sync`, `sync/atomic` (E013)

**Error codes**: E013

**Why blocked**: Mutexes, wait groups, and atomic operations are designed for
concurrent access. Workflow code is single-threaded by design — there is no
goroutine creation, so synchronization primitives have no purpose and their
internal state is not preserved across replays.

**What to use instead**: Nothing. Workflow code runs single-threaded. Use local
variables instead of shared state.

### `time.Now` and `time.Sleep` (E003, E004)

**Error codes**: E003 (time.Now), E004 (time.Sleep)

**Why blocked**: Wall-clock time differs across replays. `time.Sleep` blocks
the WASM runtime which prevents the worker from handling concurrent workflows.

**What to use instead**:
- `h.Now()` — returns deterministic time (recorded at original execution, replayed identically)
- `h.DurableSleep()` — suspends the workflow and records the sleep in event history

### `time.After`, `time.NewTicker`, `time.NewTimer` (E014)

**Error codes**: E014

**Why blocked**: These create internal goroutines, which are forbidden in
workflow code. The goroutines' scheduling is non-deterministic and their
state is not preserved across replays.

**What to use instead**: `h.DurableSleep()` or check elapsed time with `h.Now()`.

### `fmt.Print`, `fmt.Printf`, `fmt.Println`, etc. (E015)

**Error codes**: E015

**Why blocked**: Output to stdout/stderr is not captured reliably during replay.
A `fmt.Println()` call during original execution writes to the console, but during
replay there is no event history entry for it.

**What to use instead**: `h.DurableLog()` records log output in event history
and replays it deterministically. Alternatively, use `h.DurableLog().LogKV()`
for structured key-value logging.

### `log` (E015)

**Error codes**: E015

**Why blocked**: Same as `fmt.Print*` — the `log` package writes to stderr and
is not captured during replay.

**What to use instead**: `h.DurableLog()`.

---

## 2. Allowed Standard Library Packages

The following packages are **allowed** and safe to use in workflow code. This
list is not exhaustive — any package not explicitly forbidden is available, but
be aware that many packages work via determinism-sensitive mechanisms
(e.g., `sort` uses reflection in some paths, producing W001/W002 warnings).

### Always Safe

| Package | Notes |
|---------|-------|
| `sort` | Mostly safe; uses reflection but produces deterministic output |
| `strings` | Pure string manipulation, fully deterministic |
| `bytes` | Pure byte slice manipulation, fully deterministic |
| `strconv` | String/number conversions, fully deterministic |
| `unicode` | Character classification, fully deterministic |
| `unicode/utf8` | UTF-8 encoding/decoding, fully deterministic |
| `math` | Pure math (except `math/rand`), deterministic |
| `errors` | Error creation and wrapping, deterministic |
| `fmt.Sprintf` | String formatting (not `fmt.Print*`) is safe |
| `encoding/base64` | Encoding/decoding without external state |
| `encoding/hex` | Hex encoding/decoding |
| `container/heap` | Heap data structure, deterministic |
| `container/list` | Linked list, deterministic |
| `container/ring` | Circular list, deterministic |
| `crypto/sha256`, `crypto/md5`, etc. (pure hash functions) | No external state, deterministic |
| `encoding/binary` | Binary encoding/decoding |

### Safe with Caution

| Package | Warning | Notes |
|---------|---------|-------|
| `encoding/json` | W001 (map iteration) | Deterministic if not used with maps. Can add 1-2 MB to WASM binary. |
| `maps` | W001 | Iteration order over maps is non-deterministic; use sorted keys |
| `slices` | — | Safe; deterministic |
| `sync` (only `sync.Once`) | — | `sync.Once` is safe for one-time initialization patterns |
| `crypto/aes`, `crypto/cipher` | — | Pure computation, deterministic |
| `time` (only `time.Duration`, `time.Time` values, `time.Parse`) | — | Creating values and parsing is safe. `time.Now()` and `time.Sleep()` are forbidden |

### Not Recommended

| Package | Problem |
|---------|---------|
| `context` | Contexts carry deadlines and cancellation signals that are not replayable. Pass explicit parameters instead. |
| `unsafe` | Pointer arithmetic violates memory safety guarantees. Hard to debug and easily broken by Go version changes. |
| `embed` | Embedded files are not available in the WASM module. Use `h.DurableCall()` instead. |

---

## 3. Forbidden Language Constructs

Beyond package restrictions, certain Go language features are forbidden or
warned about:

### Goroutines (E001)

```go
go doSomething() // ERROR: E001
```

**Why**: Goroutine scheduling is non-deterministic. The interleaving of goroutine
execution differs across replays.

**Alternative**: Use `h.ChildWorkflow()` for parallelism.

### Channels (E002)

```go
ch := make(chan int)
ch <- 42      // ERROR: E002 (send)
<-ch          // ERROR: E002 (receive)
close(ch)     // ERROR: E012
```

**Why**: Channel operations are non-deterministic and goroutine state is not
replayed.

**Alternative**: Use `h.AwaitSignals()` for signal-based coordination.

### Interface Dispatch (E008)

```go
var iface MyInterface = implementation
iface.DoSomething() // ERROR: E008
```

**Why**: The analyzer cannot statically determine which concrete method is
called through an interface, so it cannot verify the call chain.

**Alternative**: Use concrete types or make the function a cleat entry point.

### Function Value Calls (E009)

```go
fn := someFunction
fn() // ERROR: E009
```

**Why**: The analyzer cannot trace function-value calls through the call graph.

**Alternative**: Call functions directly by name.

### Floating Point in Control Flow (W002)

```go
if x > 0.5 { // WARNING: W002
```

**Why**: Floating-point comparison results can differ between Go and TinyGo,
or between CPU architectures, causing replay divergence.

**Alternative**: Use integer arithmetic or `math.Float64bits()` for exact
bitwise comparison.

### Map Iteration (W001)

```go
for k, v := range myMap { // WARNING: W001
```

**Why**: Map iteration order is intentionally random in Go, producing different
orderings across replays.

**Alternative**: Iterate over sorted keys:
```go
keys := make([]string, 0, len(myMap))
for k := range myMap {
    keys = append(keys, k)
}
sort.Strings(keys)
for _, k := range keys {
    v := myMap[k]
    // ...
}
```

---

## 4. Complete Error Code Reference

| Code | Severity | Rule | Applies To |
|------|----------|------|-----------|
| E001 | Error | Goroutines | Language construct |
| E002 | Error | Channel send/receive | Language construct |
| E003 | Error | `time.Now()` | Function call |
| E004 | Error | `time.Sleep()` | Function call |
| E005 | Error | `net/http` calls | Package import |
| E006 | Error | `database/sql` calls | Package import |
| E007 | Error | `math/rand` calls | Package import |
| E008 | Error | Interface dispatch | Language construct |
| E009 | Error | Function value calls | Language construct |
| E010 | Error | `os` package reference | Package reference |
| E011 | Error | `reflect` package reference | Package reference |
| E012 | Error | `close()` on channels | Language construct |
| E013 | Error | `sync.Mutex`, `sync/atomic`, etc. | Package import |
| E014 | Error | `time.After`, `time.NewTicker`, `time.NewTimer` | Function call |
| E015 | Error | `fmt.Print*` / `log.Print*` output | Function call |
| E016 | Error | `os.Getenv` / `os.Exit` | Function call |
| E017 | Error | `crypto/rand` | Package import |
| E018 | Error | `math/rand/v2` | Package import |
| E020 | Error | Durable calls in `init()` | Package structure |
| W001 | Warning | Map iteration | Language construct |
| W002 | Warning | Float in control flow | Language construct |

---

## 5. TinyGo Limitations

When using `cleat build --target tinygo`, additional constraints apply because
TinyGo does not fully implement the Go standard library. Using
`cleat build --target go` (the default) lifts all TinyGo-specific limitations:
the full Go standard library is available, there are no JSON bugs, no Go version
constraints beyond the project's `go.mod`, and no `.deps/` shim is required.
See [When to Use Standard Go vs TinyGo](#when-to-use-standard-go-vs-tinygo)
for guidance on choosing between the two targets.

### Missing Standard Library Packages

The following packages are **not available** under TinyGo:

| Package | Status | Workaround |
|---------|--------|------------|
| `net` | Unimplemented | Use `h.DurableFetch()` for HTTP |
| `net/http` | Unimplemented | Use `h.DurableFetch()` for HTTP |
| `crypto/tls` | Partial | Avoid TLS; use plain HTTP or `h.DurableFetch()` |
| `crypto/x509` | Unimplemented | Not needed without TLS |
| `encoding/asn1` | Unimplemented | Avoid |
| `encoding/gob` | Unimplemented | Use `encoding/json` or manual serialization |
| `encoding/xml` | Unimplemented | Use `encoding/json` |
| `html/template` | Unimplemented | Not needed in workflow code |
| `net/url` | Partial | Use string manipulation |
| `os/exec` | Unimplemented | Use `h.ChildWorkflow()` |
| `os/signal` | Unimplemented | Not needed in WASM |
| `path/filepath` | Partial | Use `path` or string manipulation |
| `plugin` | Unimplemented | Not applicable (WASM) |
| `reflect` | Partial | Avoid (also forbidden by cleat) |
| `regexp` | Partial | Use string operations as workaround |
| `runtime/debug` | Unimplemented | Avoid |
| `runtime/pprof` | Unimplemented | Use host-level profiling |
| `sync` | Partial (no RWMutex, Cond) | Not needed (single-threaded) |
| `syscall` | Unimplemented | Not needed in WASM |
| `testing` | Unimplemented | Use `cleattest.TestEnv` |
| `text/template` | Partial | Avoid in workflow code |
| `unsafe` | Partial | Avoid (also not recommended) |

### Unsupported Go Features in TinyGo

| Feature | Status | Notes |
|---------|--------|-------|
| Goroutines | Supported (with limitations) | TinyGo uses a cooperative scheduler; blocking operations must yield |
| Channels | Supported (with limitations) | Only works with goroutines |
| Generics (Go 1.18+) | Partial | Basic generics work; complex constraints may fail |
| `recover()` | Supported | Works as expected |
| `defer` | Supported | Works as expected |
| CGo | Not supported | WASM target disables CGo |
| `//go:wasmimport` | Supported | Used by cleat adapter code |
| Reflection via `interface{}` | Partial | Type assertions work; `reflect` package is limited |
| `finalizer` | Not supported | No GC finalizers in TinyGo |
| Plugin loading | Not supported | WASM cannot load Go plugins |
| `go test` | Supported | But `testing` package is limited |
| Race detector | Not supported | No runtime race detection in TinyGo |

### Binary Size Comparison

| Scenario | Standard Go | TinyGo | Savings |
|----------|-------------|--------|---------|
| Hello World (no imports) | 4 MB | 50 KB | ~98.8% |
| Basic workflow (calls, sleep) | 5-6 MB | 80-120 KB | ~98% |
| Workflow with JSON | 6-8 MB | 100-150 KB | ~98% |
| Complex workflow (retries, signals) | 7-10 MB | 120-200 KB | ~98% |

### Performance Characteristics

| Metric | Standard Go WASM | TinyGo WASM |
|--------|-----------------|-------------|
| Cold start (load from DB) | 50-100 ms | 0.5-2 ms |
| Execution speed (pure compute) | ~100% of native Go | ~50-80% of native Go |
| Garbage collection | Full GC (stop-the-world) | Simple mark-sweep; lower latency |
| Memory usage | 10-20 MB baseline | 100-500 KB baseline |

### When to Use Standard Go vs TinyGo

**Use Standard Go when:**
- Your workflow needs full standard library compatibility
- You depend on packages that are not implemented in TinyGo
- You use complex generic types with advanced constraint expressions
- Compute performance is critical (pure number crunching)

**Use TinyGo when:**
- You are starting a new workflow (it is the recommended default)
- WASM binary size and cold start time matter
- You deploy many workflow versions and want to minimize storage
- Your worker environment is memory-constrained
- You want faster build times

---

## 6. Common Migration Patterns

### Replacing `time.Now()`

```go
// Before (forbidden):
now := time.Now()
nextDeadline := now.Add(5 * time.Minute)

// After (safe):
now := h.Now()
nextDeadline := now.Add(5 * time.Minute)
```

### Replacing `fmt.Sprintf`

```go
// Before (warning: adds binary bloat):
msg := fmt.Sprintf("Order %d processed", orderID)

// After (safe, no imports needed):
msg := "Order " + strconv.Itoa(orderID) + " processed"
```

### Replacing `math/rand`

```go
// Before (forbidden):
r := rand.Intn(100)

// After (safe):
r := h.Random().Intn(100)
```

### Replacing HTTP Calls

```go
// Before (forbidden):
resp, err := http.Get("https://api.example.com/orders")

// After (safe):
var result OrderResponse
err := h.DurableCall("payment-service", "processOrder", input, &result)
```

### Replacing `encoding/json` Heavily

```go
// Before (adds 1-2 MB to binary, reflection-heavy):
var order Order
json.Unmarshal(data, &order)

// After (uses code generation, no reflection):
var order Order
order.UnmarshalJSON(data) // generated by cleat-gen
```

---

## References

- [WASM Size Guide](./wasm-size-guide.md) — Binary size impact of imports
- [TinyGo Language Support Reference](https://tinygo.org/docs/reference/lang-support/)
- [TinyGo Standard Library Coverage](https://tinygo.org/docs/reference/stdlib/)
- [Cleat Transformer Source (`internal/closure/closure.go`)](../internal/closure/closure.go)
