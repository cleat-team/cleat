# Plan: Replace TinyGo with Standard Go WASM Compilation

## Problem

TinyGo has too many restrictions and bugs:
- Limited standard library support (no `net/http`, partial `reflect`, partial `regexp`, etc.)
- Broken `encoding/json` (reflection bugs cause "invalid table access" / "unreachable" panics)
- Capped at Go 1.23 compatibility — can't use newer Go features
- Requires complex `.deps/` workaround to avoid the project's real `go.mod`
- Cooperative goroutine scheduler with subtle edge cases
- Manual JSON helpers needed as workarounds

The docs already reference a `--target go` option but it was never implemented.

## Current State

| Component | TinyGo (current) | Standard Go (target) |
|-----------|-----------------|---------------------|
| Build command | `tinygo build -target=wasip1 -o X.wasm .` | `GOOS=wasip1 GOARCH=wasm go build -o X.wasm .` |
| go.mod version | Capped at 1.23 | Project's real version (1.25+) |
| Main stub | `<-make(chan struct{})` | `select{}` |
| deps workaround | `.deps/` tree with copied cleat SDK | Direct use of project go.mod |
| JSON handling | Manual helpers (no encoding/json) | Standard encoding/json works |
| Binary size | 50-200 KB | 4-10 MB |
| Runtime | wazero / wasmtime | wazero / wasmtime (both already support wasip1) |
| WASM imports/exports | `//go:wasmimport` / `//go:wasmexport` | Same — identical ABI |
| Generated stubs | gen_wasm_imports.go, gen_wasm_memory.go, gen_host_adapter.go, gen_wasm_exports.go | Same — identical generated code |
| Build tags | `//go:build wasip1` | Same |
| Goroutine model | TinyGo asyncify (cooperative) | Go wasip1 scheduler (different mechanism, same semantics) |

## Experiments to Validate

### Exp 1: Can we compile a simple workflow with standard Go?
- Take a minimal workflow (e.g., `testdata/basic/order.go`)
- Run the full pipeline (analyze → transform → generate stubs → build) but replace the TinyGo compile step with `GOOS=wasip1 GOARCH=wasm go build`
- Check if it produces a valid WASM binary
- Measure binary size

### Exp 2: Does the standard Go WASM binary run on wazero?
- Load the binary into the existing wazero Runtime
- Call an exported entry point
- Verify host function imports are resolved
- Verify export results are decoded correctly

### Exp 3: Does the standard Go WASM binary run on wasmtime?
- Same as Exp 2 but with the wasmtime backend
- Verify WASI initialization works

### Exp 4: Does `select{}` work for keeping the module alive?
- Standard Go wasip1 uses `select{}` (not `<-make(chan struct{})`)
- Verify that exports remain callable after `main()` blocks
- Verify goroutine scheduling works for concurrent host calls

### Exp 5: Do all host functions work end-to-end?
- Test each import: cleat_call, cleat_sleep, cleat_await_signals, etc.
- Verify suspend/resume works (the suspend sentinel via bit 62)
- Verify child workflows
- Verify error handling

### Exp 6: Size and performance comparison
- Build the same workflow with both TinyGo and standard Go
- Compare binary size
- Compare cold start latency
- Compare execution speed

### Exp 7: Can we drop the manual JSON helpers?
- Standard Go `encoding/json` works in wasip1
- But the vet checker blocks `encoding/json` (W001 warning) and `reflect` (E011 error)
- Decision: keep manual helpers for TinyGo compat, but standard Go can use real JSON

## Implementation Plan

### Phase 1: Add `--target go` to the build pipeline

1. **`internal/wasm/usage.go`**: Add `GoTarget = "go"` constant
2. **`internal/wasm/build.go`**: 
   - Add `Go` case to `PrepareBuildDir` that:
     - Uses project's real go.mod (no version capping, no `.deps/`)
     - Uses `select{}` for main stub
     - Uses a replace directive pointing to the real project root
   - Or refactor `PrepareBuildDir` to accept target-dependent behavior
3. **`internal/wasm/adapter.go`**: Allow `encoding/json` usage when target is "go" (skip manual JSON helpers)
4. **`cmd/cleat/main.go`**:
   - Add "go" to valid targets list
   - Add dispatch: when `target == "go"`, use `GOOS=wasip1 GOARCH=wasm go build` instead of `tinygo build`
   - Remove the `target == "tinygo"` gate on orphaned import check (apply to both)
   - Update help text
5. **`internal/wasm/generator.go`**: The `BuildOutputs` function already accepts `target` — verify it works for "go"

### Phase 2: Update main stub

The `gen_main_stub.go` content in `PrepareBuildDir` line 147 is hardcoded:
```go
mainStub := "package main\n\nfunc main() {\n\t<-make(chan struct{})\n}\n"
```
For standard Go, use:
```go
mainStub := "package main\n\nfunc main() {\n\tselect{}\n}\n"
```

### Phase 3: Update go.mod generation

For standard Go:
```go
module cleat-build

go <projectGoVersion>

require github.com/cleat-team/cleat/cleat v0.0.0

replace github.com/cleat-team/cleat/cleat => <projectRoot>/cleat
```
No `.deps/` workaround needed.

### Phase 4: Update tests

- Add `TestPrepareBuildDirGoTarget` 
- Add `TestRunBuild_GoTarget`
- Update CI to test `--target go` builds
- Cross-language tests: add Go target variant

### Phase 5: Update documentation

- Update `docs/explanation/wasm-compilation.md`
- Update `docs/workflow-go-constraints.md`
- Update `CONTRIBUTING.md`
- Update `CLAUDE.md`
- Update CLI help text

## Decision Points

1. **Default target**: Keep TinyGo as default for now (smaller binaries), or switch to standard Go?
   - Recommendation: Keep TinyGo default initially, add `--target go` as opt-in, gather feedback

2. **JSON helpers**: Drop manual JSON for standard Go, or keep for consistency?
   - Recommendation: Use `encoding/json` for standard Go, keep manual helpers for TinyGo

3. **Both backends or just one**: Test on both wazero and wasmtime?
   - Recommendation: Both — the backend abstraction already supports this

4. **Go version**: What minimum Go version is needed?
   - `GOOS=wasip1` support stabilized in Go 1.21
   - `//go:wasmexport` requires Go 1.24+
   - Recommendation: Require Go 1.24+ for `--target go`
