# Determinism in Cleat Workflows

Cleat workflows must be deterministic to ensure correct replay. This document
describes the determinism guarantees, common pitfalls, and how to avoid them.

## Why Determinism Matters

Cleat records the event history of each workflow execution. When a workflow is
replayed (e.g., after a crash, for recovery, or during replay testing), the
workflow code is re-executed and must produce the same sequence of events.
Any non-determinism in the workflow code can cause replay divergence, leading
to incorrect results or workflow failure.

## Determinism Guarantees

### WASM Floating-Point (IEEE 754)

WASM `f32` and `f64` operations follow **IEEE 754-2019**, which guarantees
bit-identical results for the same arithmetic operations on the same inputs
across all compliant hardware. wazero's interpreter (used by cleat) implements
strict IEEE 754 semantics without "fast math" optimizations.

**Gotchas:**

1. **NaN payloads**: IEEE 754 allows multiple bit patterns for NaN. WASM
   operations that produce NaN may return different NaN payloads across CPU
   architectures or wazero versions. Use `math.Float64bits()` /
   `math.Float32bits()` for exact comparison, or avoid NaN entirely.
2. **Denormal numbers**: Some CPUs flush denormals to zero; wazero's
   interpreter preserves them. This is only visible if comparing floats
   for equality near the denormal range.
3. **FMA (Fused Multiply-Add)**: The host Go compiler may apply FMA,
   changing the exact result of `a*b + c` vs `fma(a, b, c)`. WASM code
   inside wazero is not affected, but be aware if mixing host and WASM
   computations.

### Cleat API Determinism

| API | Deterministic? | Notes |
|-----|---------------|-------|
| `h.Now()` | Yes | Virtual time from event history |
| `h.Random()` | Yes | Seeded replay-safe PRNG |
| `h.DurableCall()` | Yes | Recorded in event history |
| `h.Sleep()` | Yes | Virtual time |
| `h.AwaitSignal()` | Yes | Recorded signal replay |
| `time.Now()` | No | **Blocked** at vet/build time |
| `crypto/rand` | No | **Blocked** at vet/build time |

## Common Non-Determinism Patterns

### 1. Map Iteration (E021)

Go map iteration order is intentionally non-deterministic and varies between
runs. This is detected by `cleat vet` as error **E021**.

**Bad:**
```go
for name, score := range scores {
    h.DurableCall("svc", "report", ...)
}
```

**Fix:**
```go
keys := slices.Collect(maps.Keys(scores))
slices.Sort(keys)
for _, name := range keys {
    score := scores[name]
    h.DurableCall("svc", "report", ...)
}
```

### 2. Floating-Point in Control Flow (W002)

Floating-point comparisons in conditions (`if`, `for`, `switch`) can produce
different results across hardware or during replay. This is detected by
`cleat vet` as warning **W002**.

**Bad:**
```go
if val > 0.5 {
    h.DurableCall("svc", "op", `{"branch":"high"}`)
}
```

**Fix:**
```go
threshold := math.Float64bits(0.5)
vbits := math.Float64bits(val)
if vbits > threshold {
    h.DurableCall("svc", "op", `{"branch":"high"}`)
}
```

Or use integer arithmetic instead of floats.

### 3. Non-Deterministic APIs

The following patterns are blocked at build time:
- `time.Now()`, `time.Since()` -- use `h.Now()` instead
- `crypto/rand`, `math/rand` -- use `h.Random()` instead
- `sync.Mutex`, `sync.RWMutex` -- use cleat's built-in concurrency
- `os` package access -- blocked for WASM target
- `net/http` -- use `h.DurableCall()` for service calls
- `reflect` -- blocked for WASM target
- `fmt.Print*` -- blocked (captures stdout non-deterministically)
- Goroutines, channels -- use cleat's async patterns

## Verifying Determinism

Run `cleat vet` before deploying any workflow:

```bash
cleat vet ./workflows/my-workflow/
```

For CI/CD pipelines, use `--json` and `--ci` flags for machine-readable output
and strict enforcement.

## Event History Integrity

Cleat computes a **SHA-256 checksum** for each event in the event history.
The checksum is stored alongside the event data and verified during replay.
This provides runtime detection of data corruption, tampering, or
non-deterministic behavior.

If the checksum verification fails during replay, the engine:
1. Increments the `cleat_replay_checksum_failures_total` Prometheus counter
2. Logs the failure with the workflow ID and step number
3. If `failOnChecksumMismatch` is true (default), aborts the replay
4. If `failOnChecksumMismatch` is false, logs a warning and continues

This provides a safety net for non-determinism that was not caught at vet time.
