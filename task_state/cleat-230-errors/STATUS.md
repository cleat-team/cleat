# cleat-230-errorse — STATUS

**Phase:** implementing
**Started:** 2026-06-06T07:14:56Z
**Updated:** 2026-06-06T07:25:00Z
**Budget spent:** ~$3.00 / $6.67

## Changes Made

### 1. Fixed WASM trap capitalization inconsistency (`engine/dwarf_trap.go:42`)
- `enrichTrapMessage()` was returning `"WASM trap: %s"` (uppercase) while `formatWasmCallError()` in `runtime.go` returns `"wasm trap: ..."` (lowercase)
- The `resolveWasmTrap()` check at line 27 matches lowercase only, so panic-recovered errors got the uppercase variant
- Changed to lowercase `"wasm trap:"` for consistency — all WASM trap errors now use the same format regardless of the code path that produces them

### 2. Wrapped WASM trap errors with operation context (`engine/engine.go:1117-1120, 1315-1318`)
- Errors from `executeFresh` and `executeCompiled` were returned as raw trap strings (e.g. `"wasm trap: unreachable\n..."`)
- Now wrapped as `"host: workflow execution failed: wasm trap: unreachable\n..."` — consistent with other engine errors using the `"host:"` prefix
- Fallback errors (non-WASM trap failures) also wrapped with `"host: workflow execution failed:"`

### 3. Added workflow ID to worker error messages (`cmd/cleat-worker/main.go:1466, 1504`)
- `"history load: %v"` → `"workflow %s: history load: %v"` (includes workflow ID)
- `"create runtime: %v"` → `"workflow %s: create runtime: %v"` (includes workflow ID)
- These messages go directly to `error_msg` in the `workflow_instances` table and are shown in the clew dashboard

## Existing Good Error Messages (no changes needed)

- **Replay divergence messages** (engine.go ~8 locations): Already include step number, expected vs actual calls, and HOW guidance ("Run 'cleat vet' on your workflow code...")
- **Version mismatch / plugin mismatch** errors (worker main.go): Already detailed with specific versions and available plugins
- **Memory cap exceeded** (worker main.go): Already actionable ("increase --wasm-memory-max-mb or reduce module memory usage")
- **WASM trap messages** (runtime.go): Already include trap type and DWARF-resolved stack traces via wazero v1.9.0
- **"host:" prefix** pattern: Already used consistently for most engine-level errors

## Build & Test

- `go build ./engine/` — passes
- `go build ./cmd/cleat-worker/` — passes  
- `go vet ./cmd/cleat-worker/` — passes
- `go test ./engine/ -run "TestDwarf|TestRuntime|TestError|TestFail|TestExecuteWorkflow"` — all pass
- One pre-existing vet warning in `engine/component_cgo.go:1435` (unsafe.Pointer, unrelated)

## Remaining Work (out of scope for this task)

The `plans/full-codebase-message-quality-plan.md` identifies ~100+ additional error message improvements across the entire codebase (Python SDK, AssemblyScript SDK, Rust SDK, Java SDK, CLI tools, plugins). These are outside the scope of this $6.67 task which focuses specifically on engine/worker error messages surfacing to the clew dashboard.
