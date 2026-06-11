# Cleat Error Code Catalog

This document catalogs all error codes produced by the Cleat project's
static analysis tool (`cleat vet`). Each error is emitted when the analyzer
detects a pattern that would break workflow determinism during replay.

## Error Code Overview

| Code | Description | Severity |
|------|-------------|----------|
| [E001](#e001--goroutines) | Goroutines | Error |
| [E002](#e002--channel-operations) | Channel send/receive | Error |
| [E003](#e003--timenow) | `time.Now()` | Error |
| [E004](#e004--timesleep) | `time.Sleep()` | Error |
| [E005](#e005--nethttp-calls) | `net/http` calls | Error |
| [E006](#e006--databasesql) | `database/sql` | Error |
| [E007](#e007--mathrand) | `math/rand` | Error |
| [E008](#e008--interface-dispatch) | Interface dispatch | Warning |
| [E009](#e009--function-value-calls) | Function-value calls | Warning |
| [E010](#e010--os-package) | `os` package usage | Error |
| [E011](#e011--reflect-package) | `reflect` package | Error |
| [E012](#e012--close-on-channels) | `close()` on channels | Error |
| [E013](#e013--sync-operations) | `sync` package | Error |
| [E014](#e014--timeafter-and-timers) | `time.After` / timers | Error |
| [E015](#e015--fmtlog-output) | `fmt`/`log` output | Warning |
| [E016](#e016--osenv-and-osexit) | `os.Getenv` / `os.Exit` | Error |
| [E017](#e017--cryptorand) | `crypto/rand` | Error |
| [E018](#e018--mathrandv2) | `math/rand/v2` | Error |
| [E020](#e020--durable-calls-in-init) | Durable calls in `init()` | Error |
| [E021](#e021--non-deterministic-map-iteration) | Map iteration | Error |

> **Note:** E019 is not currently assigned. E014 covers `time.After`,
> `time.NewTicker`, and `time.NewTimer`.

---

## E001 -- Goroutines

**Source:** `internal/closure` (static analyzer)

**Cause:** The workflow function uses the `go` keyword to spawn a goroutine.
Goroutines introduce non-deterministic scheduling across replays -- the order
and timing of goroutine execution is not guaranteed to be the same on replay
as it was on the original execution.

**Fix:** Replace goroutines with child workflows (`h.ChildWorkflow`) for
parallelism. Cleat's child workflow mechanism is deterministic and replayed
correctly.

**Example:**
```go
// BAD -- non-deterministic goroutine:
go func() {
    result := h.DurableCall("service", "Op", input)
    // ...
}()

// GOOD -- use child workflow for parallelism:
runID, err := h.ChildWorkflow("my_worker", input)
// ...
result, err := h.AwaitChild(runID)
```

---

## E002 -- Channel Operations

**Source:** `internal/closure` (static analyzer)

**Cause:** The workflow function uses channel send (`ch <-`) or receive
(`<-ch`) operations. Channel operations are non-deterministic because
goroutine state is not replayed. The order in which channel operations
complete depends on goroutine scheduling, which varies between runs.

**Fix:** Replace channels with Cleat signals (`h.AwaitSignals`,
`h.PollSignal`, `h.SendSignal`). Signals are replayed deterministically.

**Example:**
```go
// BAD -- channel receive is non-deterministic:
result := <-resultCh

// GOOD -- use signals:
result := h.AwaitSignals([]string{"order_ready"}, timeoutMs)
```

---

## E003 -- `time.Now()`

**Source:** `internal/closure` (static analyzer)

**Cause:** The workflow function calls `time.Now()`, which returns the
wall-clock time at the moment of the call. On replay, the wall-clock time
will be different, producing a different value than the original execution.

**Fix:** Use `h.Now()` for deterministic time. The host returns the
recorded time from the original execution during replay.

**Example:**
```go
// BAD -- non-deterministic wall-clock time:
now := time.Now()

// GOOD -- deterministic workflow time:
now := h.Now()
```

---

## E004 -- `time.Sleep()`

**Source:** `internal/closure` (static analyzer)

**Cause:** The workflow function calls `time.Sleep()`, which pauses
execution using real wall-clock time. On replay, `time.Sleep()` would
re-execute and wait for the full duration again, breaking both
determinism and performance.

**Fix:** Use `h.DurableSleep()` (or `h.CleatSleep()`). The host records
the sleep and returns immediately during replay.

**Example:**
```go
// BAD -- real wall-clock sleep:
time.Sleep(5 * time.Second)

// GOOD -- replay-safe durable sleep:
h.DurableSleep(5000) // 5 seconds in milliseconds
```

---

## E005 -- `net/http` Calls

**Source:** `internal/closure` (static analyzer)

**Cause:** The workflow function makes direct HTTP calls via `net/http`.
Network calls are non-deterministic -- network failures, DNS resolution,
and response timing differ across replays.

**Fix:** Use `h.DurableCall()` with a registered service name, or
`h.CleatFetch()` for durable HTTP requests. These are recorded in the
event history and return cached results during replay.

**Example:**
```go
// BAD -- direct network call:
resp, err := http.Get("https://api.example.com/orders/123")

// GOOD -- durable call through host:
result, err := h.DurableCall("orders", "GetOrder", `{"id":"123"}`)

// ALSO GOOD -- durable HTTP fetch:
result, err := h.CleatFetch("https://api.example.com/orders/123", "GET", nil, nil)
```

---

## E006 -- `database/sql`

**Source:** `internal/closure` (static analyzer)

**Cause:** The workflow function uses `database/sql` directly. Database
queries produce non-replayable side effects because the database state
may have changed between the original execution and replay.

**Fix:** Replace direct database calls with `h.DurableCall()` to a
registered backend service that performs the database operation.

**Example:**
```go
// BAD -- direct database access:
row := db.QueryRow("SELECT status FROM orders WHERE id = $1", orderID)

// GOOD -- durable call to a service:
result, err := h.DurableCall("db-service", "query", `{"sql":"SELECT status FROM orders WHERE id = 123"}`)
```

---

## E007 -- `math/rand`

**Source:** `internal/closure` (static analyzer)

**Cause:** The workflow function uses `math/rand`. The default seed depends
on wall-clock time, producing different random sequences on replay.

**Fix:** Use `h.Random()` for deterministic randomness. The host records
the random value during the original execution and returns the same value
during replay.

**Example:**
```go
// BAD -- non-deterministic random:
val := rand.Intn(100)

// GOOD -- deterministic workflow random:
val := h.Random()
```

---

## E008 -- Interface Dispatch

**Source:** `internal/closure` (static analyzer)

**Cause:** The workflow function calls a method through an interface type.
The analyzer cannot statically resolve which concrete implementation will
execute, so it cannot verify the callee is safe for workflow use.

**Fix:** Use concrete types instead of interfaces, or refactor to avoid
interface dispatch in cleat workflow functions.

**Example:**
```go
// BAD -- call through interface (unverifiable):
var svc Service = &impl{}
result := svc.DoWork(input)

// GOOD -- call concrete type directly:
var svc ConcreteService
result := svc.DoWork(input)
```

---

## E009 -- Function-Value Calls

**Source:** `internal/closure` (static analyzer)

**Cause:** The workflow function calls a function value stored in a
variable. The analyzer cannot statically resolve the call target, so it
cannot verify the callee is safe for workflow use.

**Fix:** Replace function-value calls with direct function calls or inline
the logic.

**Example:**
```go
// BAD -- function value call (unverifiable):
fn := someFunc
result := fn(input)

// GOOD -- direct function call:
result := someFunc(input)
```

---

## E010 -- `os` Package

**Source:** `internal/closure` (static analyzer)

**Cause:** The workflow function references the `os` package. Filesystem
operations, environment variable access, and process management differ
across replays and are not allowed in workflow code.

**Fix:** Pass configuration via workflow input instead of reading from
the environment. Use `h.DurableCall()` for filesystem operations.

**Example:**
```go
// BAD -- os operations:
dbURL := os.Getenv("DATABASE_URL")
data, err := os.ReadFile("/path/to/config.json")

// GOOD -- configuration via workflow input:
// Pass DATABASE_URL as part of the workflow input payload
```

---

## E011 -- `reflect` Package

**Source:** `internal/closure` (static analyzer)

**Cause:** The workflow function references the `reflect` package.
Reflection results can differ across Go versions or build targets,
breaking determinism.

**Fix:** Avoid runtime type introspection. Use compile-time generics
where possible, or use concrete types.

**Example:**
```go
// BAD -- reflection breaks determinism:
t := reflect.TypeOf(val)

// GOOD -- use concrete types or generics:
func process[T any](val T) string {
    // type-safe at compile time
}
```

---

## E012 -- `close()` on Channels

**Source:** `internal/closure` (static analyzer)

**Cause:** The workflow function calls the `close()` built-in on a
channel. Closing a channel signals goroutine state that does not exist
during replay. Channel operations are inherently non-deterministic.

**Fix:** Replace channel patterns with signals (`h.AwaitSignals`,
`h.PollSignal`).

**Example:**
```go
// BAD -- channel close:
close(doneCh)

// GOOD -- use signals instead:
h.SendSignal(workflowID, "done", `{}`)
```

---

## E013 -- `sync` Operations

**Source:** `internal/closure` (static analyzer)

**Cause:** The workflow function uses `sync` package types
(`sync.Mutex`, `sync.RWMutex`, `sync.WaitGroup`, `sync.Once`,
`sync.Cond`, `sync.Pool`, `sync.Map`) or `sync/atomic` operations.
These synchronization primitives assume multi-threaded execution and
are non-deterministic across replays.

**Fix:** Workflow code is single-threaded by design. Remove the
synchronization primitive -- it is not needed. Use local variables
instead of atomic operations.

**Example:**
```go
// BAD -- mutex in workflow code (not needed):
var mu sync.Mutex
mu.Lock()
counter++
mu.Unlock()

// GOOD -- no synchronization needed (single-threaded):
counter++
```

---

## E014 -- `time.After` and Timers

**Source:** `internal/closure` (static analyzer)

**Cause:** The workflow function uses `time.After`, `time.NewTicker`, or
`time.NewTimer`. These create goroutines internally, which are
non-deterministic across replays. The channel receives from these
timers depend on goroutine scheduling.

**Fix:** Use `h.DurableSleep()` for deterministic delays.

**Example:**
```go
// BAD -- creates goroutine:
<-time.After(5 * time.Second)

// GOOD -- deterministic sleep:
h.DurableSleep(5000)
```

---

## E015 -- `fmt`/`log` Output

**Source:** `internal/closure` (static analyzer)

**Cause:** The workflow function uses `fmt.Print`, `fmt.Printf`,
`fmt.Println`, `fmt.Fprint`, `fmt.Fprintf`, `fmt.Fprintln` or `log.Print`,
`log.Printf`, `log.Println`, `log.Fatal`, `log.Fatalf`, `log.Fatalln`,
`log.Panic`, `log.Panicf`, `log.Panicln`. Output to stdout/stderr is
not captured reliably during replay.

**Fix:** Use `h.DurableLog()` for deterministic logging that survives
replay.

**Example:**
```go
// BAD -- stdout output not captured during replay:
fmt.Println("Processing order", orderID)

// GOOD -- recorded in event history:
h.DurableLog("Processing order " + orderID)
```

---

## E016 -- `os.Getenv` and `os.Exit`

**Source:** `internal/closure` (static analyzer)

**Cause:** The workflow function uses `os.Getenv`, `os.Environ`, or
`os.Exit`. Environment variable values may differ across replays.
`os.Exit` terminates the WASM runtime, preventing proper cleanup.

**Fix:** Pass configuration via workflow input instead of environment
variables. Return an error instead of calling `os.Exit`.

**Example:**
```go
// BAD -- environment-dependent:
dbURL := os.Getenv("DATABASE_URL")
os.Exit(1)

// GOOD -- input-driven configuration:
// Pass DATABASE_URL as a workflow input field
// Return error instead of exiting:
return "", fmt.Errorf("processing failed")
```

---

## E017 -- `crypto/rand`

**Source:** `internal/closure` (static analyzer)

**Cause:** The workflow function uses `crypto/rand`. It reads from OS
entropy sources, producing different values across replays.

**Fix:** Use `h.Random()` for deterministic randomness needed for
workflow logic.

**Example:**
```go
// BAD -- OS entropy, non-deterministic:
key := make([]byte, 32)
rand.Read(key)

// GOOD -- deterministic workflow random:
val := h.Random()
```

---

## E018 -- `math/rand/v2`

**Source:** `internal/closure` (static analyzer)

**Cause:** The workflow function uses `math/rand/v2`. Like `math/rand`,
the default seeding is time-based, producing different sequences on
replay.

**Fix:** Use `h.Random()` for deterministic randomness.

**Example:**
```go
// BAD -- time-seeded randomness:
val := rand.IntN(100)

// GOOD -- deterministic workflow random:
val := h.Random()
```

---

## E020 -- Durable Calls in `init()`

**Source:** `internal/closure` (static analyzer)

**Cause:** An `init()` function calls a durable function
(`h.DurableCall`, `h.CleatSleep`, `h.CleatLog`, etc.). Durable calls
must happen inside workflow entry points, not during package
initialization. The execution order and replay behavior of `init()`
functions is undefined for durable calls.

**Fix:** Move durable calls from `init()` into the workflow entry
point function.

**Example:**
```go
// BAD -- durable call in init():
func init() {
    h.DurableCall("service", "Op", `{}`)
}

// GOOD -- durable call in the workflow function:
func MyWorkflow(h cleat.HostCalls, input Input) (Output, error) {
    result, err := h.DurableCall("service", "Op", `{}`)
    // ...
}
```

---

## E021 -- Non-Deterministic Map Iteration

**Source:** `internal/closure` (static analyzer)

**Cause:** The workflow function iterates over a Go map using `range`.
Go map iteration order is intentionally randomized and differs across
runs, breaking workflow determinism.

**Fix:** Collect keys into a sorted slice using
`slices.Sorted(maps.Keys(m))`, then iterate over the sorted keys.

**Example:**
```go
// BAD -- non-deterministic iteration order:
for k, v := range myMap {
    process(k, v)
}

// GOOD -- deterministic sorted iteration:
for _, k := range slices.Sorted(maps.Keys(myMap)) {
    v := myMap[k]
    process(k, v)
}
```

---

## Suppressing Errors

In exceptional cases where the SDK itself or a test helper legitimately
uses a forbidden pattern, the error can be suppressed with a comment
directive:

```go
// cleat:allow E002 -- SDK test helper, not user workflow
case response := <-replyCh:
```

This is reserved for the Cleat SDK itself. User workflows should never
suppress analyzer errors -- doing so will produce non-deterministic
workflows that behave incorrectly during replay.
