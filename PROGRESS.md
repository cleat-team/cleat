# Transformer Implementation Progress

## Status: Phases 1-8 complete (of 13)

## What was built

Phases 1-8 of `transformer-plan.md`: package loading, call graph construction,
durable closure computation, HostCalls threading verification, WASM import
generation, host adapter generation, WASM export generation, and WASM compilation.

```
cmd/durable/main.go              — CLI: `durable build` and `durable vet`
durable/runtime.go               — HostCalls struct (SDK type for workflow authors)
internal/analyzer/types.go       — IR: Package, FuncDecl, AnalysisResult
internal/analyzer/loader.go      — go/packages loader, entry point detection
internal/callgraph/callgraph.go  — Call graph builder, durable leaf identification
internal/closure/closure.go      — Transitive closure, construct validation (E001-E007, W001)
internal/closure/threading.go    — HostCalls threading verification (E010)
internal/wasm/usage.go           — Host function usage analysis
internal/wasm/generator.go       — WASM import stub + memory utility code generation
internal/wasm/adapter.go         — Host adapter code generation (HostCalls → import stubs)
internal/wasm/exports.go         — WASM export generation (entry point wrappers)
internal/wasm/build.go           — Build directory assembly + go.mod generation
testdata/basic/order.go          — PlaceOrder + CancelOrder workflow fixture
testdata/errors/invalid.go       — Threading error + goroutine error fixture
```

Module: `github.com/rcownie/durable` (go.mod at repo root).

## Working output

```
$ go run ./cmd/durable/ build -o /tmp/out ./testdata/basic/
  Analyzing package github.com/rcownie/durable/testdata/basic...
  Found 12 functions, 2 entry point(s), 12 in durable closure.
  Durable leaves: chargeCustomer, checkItemAvailability, fulfillOrder, ...
  Verifying HostCalls threading... OK
  Generating WASM imports (1 host functions used)... OK
  Generating host adapter... OK
  Generating WASM exports (2 entry point(s))... OK
  Build directory: /tmp/out
  Compiling WASM module (GOOS=wasip1 GOARCH=wasm)...
  Wrote /tmp/out/cancel_order.wasm (78.5 KB)
```

The compiled WASM binary contains the expected exports (`cancel_order`, `place_order`)
and imports (`durable_call`).

## Key design decisions (new in Phase 8)

- **Single return value for wasmimport/wasmexport** — Go 1.24+ only supports 0 or 1 return
  values for these directives. Restructured all host function signatures to use output
  buffer parameters for string results and `int64` return for packing length + error code:
  `int64` = `(uint64(actualLen) << 32) | uint64(errCode)`.
- **Output buffers** — string results are written to pre-allocated buffers passed as
  `(outPtr unsafe.Pointer, maxLen uint32)` parameters. Default buffer size: 64KB.
  The adapter reads results using `unsafe.String(&buf[0], int(len))`.
- **Export signature** — `func <name>(argsPtr unsafe.Pointer, argsLen uint32, outPtr unsafe.Pointer, maxOutLen uint32) int64`.
  Input JSON at `argsPtr`, result/error JSON written to `outPtr`, return packs length + errCode.
- **Build directory assembly** — `PrepareBuildDir` copies user source files to the output
  directory, writes generated files, and creates a `go.mod` with a `replace` directive
  pointing to the project module root. This isolates the build from the source tree.
- **Module info** — `AnalysisResult` now carries `ModulePath`, `ModuleDir`, and
  `GoVersion` from the `go/packages` loader, used for `go.mod` generation.

## Go toolchain

```bash
export GOROOT=/tmp/go1.26.2/go
export PATH=/tmp/go1.26.2/go/bin:$PATH
```

## Next: Phase 9 — Validation rules and clear error messages

Goal: comprehensive error checking with error codes (E001-E011) and actionable
messages. `durable vet` for CI integration.

Phases 9 (validation), 10 (tinygo), 11 (deployment) are not on the critical path.
Phase 12 (testing) is the next critical path item after Phase 8.
