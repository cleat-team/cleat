# Project Review & Improvement Plan

## Summary

This is a durable execution framework that compiles Go workflows to WASM. The
core idea — write near-normal Go, auto-thread a HostCalls context object through
the call graph, compile to WASM, replay deterministically — is sound and
well-motivated. Phases 1-8 of the 13-phase transformer plan are implemented.
The fooddash example is credible, tests pass, and `durable vet` correctly
catches threading errors and forbidden constructs.

Below is a skeptical review organized by severity.

---

## 1. Repository Hygiene

### 1.1 Binary artifacts in the tree
Three compiled binaries (~11.5 MB total) are committed:
- `ast_demo` (3.1 MB)
- `ast_demo_v2` (3.2 MB)
- `durable-gen` (5.3 MB)

These bloat the repo and .git/index. They are already gitignored by `*.exe` patterns but these have no `.exe` extension. Add to `.gitignore` and remove.

### 1.2 go.mod module path is fictional
The module declares `github.com/rcownie/durable` but the directory is
`/localssd/rcownie/cleat`. The replace directives in generated build go.mod
files and in wasm-demo/go.mod point to `../` which works locally but the name
mismatch between the directory and module path is confusing.

### 1.3 stale references to `/home/rcownie/dev/goof/`
`durable_context.md` references paths like `/home/rcownie/dev/goof/` that don't
match the current working directory. The gopath in toolchain instructions
(`/tmp/go1.26.2/go/bin/go`) is 1.26 but `durable_context.md` says 1.23.4.

### 1.4 PROGRESS.md is outdated
Claims "Phases 1-8 complete" but doesn't mention the durabletest framework,
Selector, DurableCallTyped, DurableCallWithOptions, CallError types, Saga,
PollUntil, or the durable-gen code generator — all of which exist in the tree.
The file appears frozen at the Phase 8 implementation checkpoint.

**Fix:** Remove binaries, fix .gitignore, update stale paths in docs, update or
delete PROGRESS.md.

---

## 2. Bugs

### 2.1 WASM import result packing is undocumented and potentially buggy

`internal/wasm/adapter.go` packs 3 values into an int64 return for
`durable_call` using bit operations:

```go
responseLen := uint32(result >> 40)
callErrorCode := durable.CallErrorCode((result >> 8) & 0xFFFFFFFF)
errCode := uint32(result & 0xFF)
```

The layout uses 8 bits for errCode (0-7), 32 bits for callErrorCode (8-39), and
24 bits for responseLen (40-63). This fits in 64 bits but:
- There is NO test that round-trips the packing across the boundary.
- Different imports use different layouts (compare `durable_await_signals` which
  uses 16-bit fields for timedOut and payloadLen).
- The `callErrorCode >> 8 & 0xFFFFFFFF` effectively reads 40 bits (bits 8-47),
  overlapping with responseLen at bits 40-47 on machines where result is larger
  than 64 bits (not possible, but confusingly written).
- The actual masking for callErrorCode should be `(result >> 8) & 0xFFFFFFFF`.
  Since result is int64 and we shift right by 8, the value is in bits 8-39.

**The real bug**: `result >> 40` gives an int64, then `uint32(...)` truncates.
If the high 24 bits have data, this silently truncates. But on int64, result >>
40 produces a 24-bit int64, so the uint32 cast is safe. However, the layout is
fragile - a single off-by-one in the bit shifts corrupts all fields silently.

### 2.2 `durable_call` import used for both low-level and high-level methods

`internal/wasm/usage.go` maps `DurableCall`, `DurableCallJSON`,
`DurableCallTyped`, `DurableCallWithOptions`, `DurableCallJSONWithOptions`, and
`DurableCallWithHeartbeat` ALL to the same `durable_call` import. The import
generation deduplicates by import name (correctly), but the adapter generates
separate closures for each FieldName. This means if any HostCalls method is
used, ALL six closures are generated — including `DurableCallWithHeartbeat`
which has a `func(string)` parameter that CANNOT be passed across WASM imports.
The adapter closure for DurableCallWithHeartbeat would have `onProgress
func(string)` in its Go signature, which is fine for the closure but the
underlying import only handles the service/op/request/response parameters. The
heartbeat import doesn't know about the callback.

The fallback path in `hostCallsImpl.DurableCallWithHeartbeat` delegates to
`DurableCall` when the host doesn't provide a heartbeat-enabled implementation,
so the generated adapter would need a custom host-side path. As generated, the
adapter closure for `DurableCallWithHeartbeat` would call `durableCallImport`
and ignore the heartbeat/onProgress parameters, which is equivalent to the
fallback. This is correct but undocumented.

### 2.3 CONFIRMED BUG: Signed arithmetic right-shift corrupts packed int64 results

The WASM imports return `int64`. The adapter extracts packed fields with
`uint32(result >> N)`. In Go, `>>` on a signed `int64` is an **arithmetic**
right shift — it fills vacated bits with copies of the sign bit. When bit 63
of the packed int64 is 1 (which happens whenever the high-extraction field has
its MSB set, e.g., `responseLen >= 0x800000`), all extractions via `>> N`
produce wrong values.

**Verified with Go test**: For `responseLen=0xABCDEF`, the extraction
`uint32(result >> 40)` returns `0xFFABCDEF` instead of `0x00ABCDEF` — the
upper 8 bits are all 1s from sign extension.

Affected extractions (all files in `internal/wasm/adapter.go`):
- `durable_call`: `responseLen := uint32(result >> 40)` — broken
- `durable_await_signals`: `signalNameLen := uint32(result >> 48)` — broken
- `durable_defer`: `deferIDLen := uint32(result >> 32)` — broken
- `durable_poll_cancellation`: `reasonLen := uint32(result >> 32)` — broken
- `durable_poll_signal`: `payloadLen := uint32(result >> 32)` — broken
- `durable_child_workflow`: `runIDLen := uint32(result >> 32)` — broken
- `durable_await_child`: `resultLen := uint32(result >> 32)` — broken

Extractions that use `& mask` after the shift are NOT affected (the mask clears
sign-extended bits). Extractions from the low bits (no shift) are NOT affected.

**Fix**: Cast to uint64 before shifting: `uint32(uint64(result) >> N)`. This
forces a logical right shift.

### 2.4 `toSnakeCase` is duplicated

Both `internal/wasm/exports.go` and `cmd/durable/main.go` define identical
`toSnakeCase` functions. The export generation in wasm/exports.go uses one; the
WASM output naming in cmd/durable/main.go uses the other. They should use a
single shared function.

### 2.4 `generateExport` hardcodes `\t` in raw strings

In `internal/wasm/exports.go`, the function writes Go source containing literal
`\t` escape sequences in regular (not raw) Go strings. These correctly produce
tab characters. However, some lines don't end with `\n`:

```go
buf.WriteString("\t\treturn writeErrorOut(outPtr, maxOutLen, err)\n")
```

While correct today, this string-concatenation approach to code generation is
error-prone. Any missing `\n` produces an unparseable Go file with no compiler
error beyond "syntax error."

---

## 3. Security

### 3.1 JSON injection in testdata fixtures

`testdata/basic/order.go` and `testdata/autothread/order.go` construct JSON
with `fmt.Sprintf`:

```go
request := fmt.Sprintf(`{"sku":"%s"}`, sku)
```

A SKU containing `"` or `}` produces malformed JSON. The fooddash example
already uses `DurableCallTyped` with proper `encoding/json.Marshal` — the
testdata fixtures still use the old pattern as regression tests for the
`DurableCall` path, which is fine for test data, but it models a bad pattern
that users might copy.

### 3.2 os.Command injection via user-controlled Go version

In `cmd/durable/main.go`, the `go build` command is constructed safely, but
`cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")` inherits the
calling process's entire environment. If a user sets `GOPATH` or `GOROOT` to
point at malicious toolchains, the `go build` command picks them up. This is a
standard Go concern, not unique to this project.

---

## 4. Missing Tests

### 4.1 No test for the WASM code generation path

`internal/wasm/` has zero test files. The import stub generation, host adapter
generation, export generation, and memory utility generation are entirely
untested. A bug in the bit-packing (see 2.1) would only be caught at WASM
runtime, not at build time.

### 4.2 No test for the transform package

`internal/transform/` has zero tests. The auto-threading transform is complex
AST manipulation with no verification that the output compiles or preserves
semantics.

### 4.3 No test for the callgraph package

`internal/callgraph/` has zero tests. The call graph builder is tested
indirectly through the closure tests, but edge cases (generic functions,
interface dispatch, method calls on embedded types) are not exercised.

### 4.4 RetryPolicy exponential backoff is untested

`hostCallsImpl.DurableCallWithOptions` has a retry loop with exponential
backoff. No test verifies the backoff calculation or the non-retryable error
filtering.

### 4.5 Selector child workflow path is untested

`Selector.AddChildWorkflow` notes that child workflow support requires host-side
infrastructure and will "cause Select to return an error immediately." But
`Select()` has no child workflow polling — it only checks signals and timers.
If a child future is added, it's silently ignored.

---

## 5. Architecture & Design Issues

### 5.1 Retry at the SDK level vs host level

`DurableCallWithOptions` implements retry inside the WASM module. Each retry
attempt that fails past the first one creates a durable sleep + another durable
call. This means:
- A 3-attempt retry that succeeds on attempt 3 produces 5 history events (call
  fail + sleep + call fail + sleep + call success) instead of 1.
- The retry state (attempt count, backoff) is not persisted independently — it
  falls out deterministically from replay.
- This works but is less efficient than host-side retry, which would record only
  the final outcome.

### 5.2 Selector is a polling loop, not an event-driven primitive

`Selector.Select()` uses `AwaitSignals` with a timeout, then loops. On timeout,
it re-polls signals and checks the timer. This is functionally correct but
wasteful for long timeouts — the WASM module wakes up periodically when it could
just sleep until the timer fires. The comment acknowledges this but ships the
simple version.

### 5.3 No host runtime exists outside the demo

The WASM compilation pipeline produces `.wasm` files, but there is no production
host runtime. The `wasm-demo/host/main.go` is a standalone demo with simulated
DB/APIs. The `wasm-demo/worker/` files are separate conceptual demos. No
production-quality wazero host exists that can:
- Register WASM imports for all 14 host functions
- Manage WASM linear memory (allocator, string passing)
- Handle checkpoint/replay with a real PostgreSQL event store
- Implement the fencing protocol

This is the largest gap — the transformer produces artifacts that have no
production runtime to consume them.

### 5.4 Deployment pipeline is concept-only

`transformer-plan.md` Phase 11 describes `durable deploy` (INSERT into
`workflow_defs`) but it's not implemented. There's no database schema, no
migration, no `deploy` subcommand.

---

## 6. Code Quality

### 6.1 Inconsistent nil-guard patterns

Some HostCalls methods return errors when fields are nil (`DurableCall`,
`DurableSleepMs`, `DurableAwaitSignals`), some panic (`DurableSleepMs` again —
actually `DurableSleepMs` panics but `DurableCall` returns an error), some
silently no-op (`DurableLog`, `SetQueryState`, `PollCancellation`), and some
return defaults (`Version`, `MinVersion` return 1). There's no documented
contract for which pattern applies when. A user who forgets to wire up
`DurableSleep` gets a panic; a user who forgets `SetQueryState` gets silent data
loss.

### 6.2 HostCalls interface is large and growing

The interface has 23 methods. New additions (DurableCallTyped,
DurableCallWithOptions, DurableCallJSONWithOptions, DurableCallWithHeartbeat, 
AwaitSignals, SignalResult, PollUntil, LogKV) were all added to the single
interface. The `HostCallsOptions` struct matching it has 19 function fields.
This is a "fat interface" problem — any new capability requires touching the
interface, the implementation, the WASM adapter generation, and the durabletest
mock.

### 6.3 `placeLargeOrderInput` is defined in a function body

In `examples/fooddash/order.go:236`, a struct type is declared inside
`PlaceLargeOrder`. While legal Go, this is unusual and prevents reuse.

### 6.4 DurableCallTyped fallback duplicates DurableCallJSON logic

`hostCallsImpl.DurableCallTyped` replicates the marshal-call-unmarshal pattern
that `DurableCallJSON` already provides. If the host provides a direct
`durableCallTyped` implementation, it uses it; otherwise it falls back to
re-implementing the pattern.

---

## 7. Implementation Completeness

What exists:
- Package loading, type resolution, entry point detection
- Call graph construction with durable leaf identification
- Transitive closure computation and construct validation (E001-E007, W001)
- HostCalls threading verification (E010)
- Auto-threading transform (global h pattern)
- WASM import/export/adapter code generation
- Build directory assembly and WASM compilation
- SDK runtime with: DurableCall, Saga, PollUntil, Selector, CallError, RetryPolicy, DurableCallTyped
- `durabletest` testing framework with stubs, time control, signals, assertions
- `durable-gen` typed client code generator
- FoodDash example with typed clients and saga compensation
- `durable vet` for CI integration

What's missing:
- Host runtime (production wazero integration with PostgreSQL)
- `durable deploy` command
- tinygo build target
- Defer execution engine (host callback to execute deferred functions)
- Comprehensive WASM conformance tests
- Phase 9 polish (error code E008-E011, W002)

---

## Prioritized Action Plan

### Block 0: Hygiene (1-2 hours)
1. Remove binary artifacts (`ast_demo`, `ast_demo_v2`, `durable-gen`) from git
2. Add `ast_demo*` and `durable-gen` to `.gitignore`
3. Delete or update `PROGRESS.md` to reflect current state
4. Fix stale paths in `durable_context.md`
5. Rename repo directory or update `go.mod` module path to match

### Block 1: Critical fixes (1 day)
6. **FIX SIGNED RIGHT-SHIFT BUG (see 2.3)**: Replace all `uint32(result >> N)`
   with `uint32(uint64(result) >> N)` in `internal/wasm/adapter.go`. This is
   the highest-priority bug — it silently corrupts data for any string result
   longer than ~8MB or any extraction from the high 24 bits when the sign bit
   is set.
7. **Test WASM bit-packing round-trip**: Write a Go test that constructs packed
   int64 values for all 9 imports and verifies each field round-trips correctly
   at boundary values (0, max for each field width, sign-bit combinations).
8. **Fix `toSnakeCase` duplication**: Extract to `internal/wasm/names.go` and
   import from `cmd/durable/`.
9. **Add tests for internal/wasm, internal/transform, internal/callgraph**:
   Unit test the code generators at minimum with golden-file tests.
10. **Fix JSON injection in testdata fixtures**: Replace `fmt.Sprintf` JSON
    with `encoding/json.Marshal`.

### Block 2: Safety & correctness (2-3 days)
11. **Audit nil-guard consistency**: Standardize whether nil fields panic,
    return errors, or no-op. Document the contract.
12. **Test RetryPolicy**: Unit test the backoff calculation, max attempts
    exhaustion, and non-retryable error filtering.
13. **Test Selector child workflow path**: Either implement child polling or
    return an error when `AddChildWorkflow` is used before `Select`.
14. **Test the full durabletest-PlaceOrder integration for edge cases**:
    Concurrent signal delivery during sleep, multiple rapid signals,
    zero-duration timeouts.

### Block 3: Host runtime (1-2 weeks — largest gap)
15. **Build a minimal production host**: wazero-based host with WASM import
    registration, string-passing protocol, event history recording and replay,
    and divergence detection.
16. **Add a conformance test**: Compile `testdata/basic/` to WASM, instantiate
    in the test host, execute, verify event history matches expectations.

### Block 4: Developer experience (1 week)
17. **`durable deploy`**: INSERT WASM blob into PostgreSQL with metadata.
18. **Database schema**: Write the migration for `workflow_defs`,
    `workflow_instances`, `event_history` tables.
19. **Documentation**: README with getting-started guide, architecture overview,
    SDK API reference.

### Block 5: Polish (ongoing)
20. Phase 9 validation errors (E008-E011, W002)
21. tinygo build target
22. Defer execution engine (post-MVP)
23. Visibility dashboard (post-MVP)
