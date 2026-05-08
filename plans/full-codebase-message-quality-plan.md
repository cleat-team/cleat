# Full-Codebase Error Message Quality Improvement Plan

## Summary

After fixing the transformer/validation messages in the first pass, we applied the same five
criteria audit (WHAT, WHERE, WHY, HOW, CLARITY) to the remaining ~95 source files across the
Go host runtime, Python SDK, AssemblyScript SDK, Rust SDK, Java SDK, WASM generator, CLI tools,
and plugins. The audit found 6 bugs (silently dropped errors), 1 security vulnerability,
~100 opaque messages, and systemic gaps in HOW guidance.

---

## Part A: Bugs (Silent Data Loss / Corruption)

These are not message quality issues — they are logic bugs where errors are silently discarded.

| # | File:Line | Issue | Severity |
|---|-----------|-------|----------|
| B1 | `internal/host/engine.go:1748` | `CreatePromise` result silently discarded via `_ =` | **Critical** — failed promise creation silently accepted, workflow may hang |
| B2 | `internal/host/engine.go:1929` | `DeliverSignal` result silently discarded | **Critical** — failed signal delivery silently accepted, workflow deadlocks |
| B3 | `internal/host/engine.go:1947,1981,2092` | `ReleaseConcurrencyKey` result discarded in 3 places | **High** — scope key leaks on failure |
| B4 | `crates/cleat-java/.../JsonHelper.java:421-424` | `mapToPojo` catches `Exception` and returns `null` silently | **High** — causes confusing downstream NPEs |
| B5 | `crates/cleat-java/.../Plugins.java:193-197` | `extractLong` catches `NumberFormatException` and returns `0` | **Medium** — corrupted plugin responses silently produce zero values |
| B6 | `crates/cleat-java/.../TestHostCalls.java:605-609` | `incrState` catches `NumberFormatException` and resets to `0` | **Medium** — non-numeric state silently reset |

---

## Part B: Security Vulnerability

| # | File | Issue | Severity |
|---|------|-------|----------|
| S1 | `packages/cleat-as/assembly/plugins.ts` (14 methods, lines 224-465) | **JSON injection via string concatenation.** All plugin methods build JSON with `'{"key":"' + key + '"}'` — no escaping. User-supplied strings containing `"` or `\` produce malformed JSON or allow field injection. | **High** |

Fix: use `jsonEscape()` from `json.ts` (already exists at line 772) on all string fields before concatenation, or switch to `json_buildObject()` / `JsonBuilder`.

---

## Part C: Opaque "error code N" Messages (~100 occurrences)

The single most pervasive message quality problem across the codebase. Numeric error codes
are returned from the WASM ABI with no explanation of what they mean.

| Language | File | Count | Example |
|----------|------|-------|---------|
| Go | `internal/wasm/adapter.go` (generated code) | ~18 | `"cleat_call: error code %d"` |
| Go | `cleat/runtime.go` | ~8 | `"cleat_call_with_heartbeat failed: %v"` |
| Python | `python-sdk/cleat_sdk/host_calls.py` | ~15 | `"child_workflow failed with error code: {err_code}"` |
| AS | `packages/cleat-as/assembly/host-calls.ts` | ~24 | `"defer error code: " + decoded.errCode.toString()` |
| Java | `crates/cleat-java/.../HostCalls.java` | ~32 (already fixed in pass 1) | N/A — already done |

**Fix for Go adapter.go**: Define named constants for error codes in the generated code so messages become `"cleat_call: timeout (code 1)"` instead of `"cleat_call: error code 1"`.

**Fix for Python host_calls.py**: Map `_CALL_ERROR_CODE_MAP` to human-readable string names and include them in messages. Include parameter context (service name, operation, key, etc.).

**Fix for AS host-calls.ts**: Same pattern — define a mapping from error code to name, include parameter context in messages.

---

## Part D: "no shard available" — 39 Identical Messages With Zero Context

**File:** `internal/host/sharded_store.go`

Every mutation and query method shares the exact same `"no shard available"` message. When
this appears in logs, the operator has no way to know which operation failed or what workflow
was affected.

**Fix:** Prefix each occurrence with the operation name:
- `"claim_workflows: no shard available"`
- `"load_history: no shard available"`
- `"append_history: no shard available"`
- etc.

Also add guidance: `"check shard configuration in CLEAT_SHARD_CONFIG"`.

---

## Part E: "not initialized" — 30+ Messages With No WHY/HOW

**File:** `cleat/runtime.go`

~30 methods return `errors.New("durable: <Method> not initialized")` with no explanation
of WHY it's not initialized (typically: called outside a workflow entry point) or HOW to fix.

**Fix:** Change the message pattern to:
```
"durable: <Method> can only be called from within a workflow function (not initialized). Ensure this call is inside a @cleat_entry / #[cleat_entry] / @CleatEntry / @cleatEntry function."
```

---

## Part F: 5 Panics That Should Be Errors

| # | File:Line | Current | Fix |
|---|-----------|---------|-----|
| P1 | `cleat/runtime.go:512` | `panic("durable: virtual object name must not be empty")` | Return error |
| P2 | `cleat/runtime.go:515` | `panic(... already registered)` | Return error |
| P3 | `cleat/runtime.go:2020` | `panic("durable: Now not initialized")` | Return error — crashes worker on invalid SDK usage |
| P4 | `cleat/runtime.go:2028` | `panic("durable: Random not initialized")` | Return error |
| P5 | `cmd/cleat/dag.go:228` | `panic(err)` on type assertion | Return wrapped error |

---

## Part G: Missing WHERE / Parameter Context

### Go host runtime

| File:Line | Current | Fix |
|-----------|---------|-----|
| `internal/host/engine.go:777,865` | `"plugin function %s/%s not registered"` | Add: "Check that the plugin is deployed and its version satisfies the workflow's plugin_deps." |
| `internal/host/engine.go:560` | `"no update handler configured"` | Add: "Call WithUpdateHandler before DispatchUpdate." |
| `internal/host/runtime.go:107,113,117` | `"compile: %w"` etc. | Prefix with `"host: "` for consistency with engine.go |
| `internal/host/runtime.go:185` | `"host: export %q returned no results"` | Add WHY: "The WASM module may have panicked or returned void." |
| `internal/host/plugin_loader.go:427,432` | `"scan plugin def: %w"` etc. | Include plugin name |
| `internal/host/plugin_call_guard.go:46` | `"plugin %q is not allowed to call plugin %q"` | Add HOW: "Add %q to the caller's call_plugin capability in its manifest." |
| `internal/host/db.go:345` | `"begin tx: %w"` | Prefix with the operation name: `"<operation>: begin tx: %w"` |
| `internal/host/db.go:349` | `"set rls: %w"` | Spell out: `"set row-level security: %w"` |
| `plugins/dag/dag.go:285` | `"dag: cycle detected"` | Include the tasks involved: `"dag: cycle detected involving tasks: %v"` |

### Go CLI

| File:Line | Current | Fix |
|-----------|---------|-----|
| `cmd/cleat/main.go:842` | `"Error in transform: %v"` | `"Error in AST transformation: %v"` ("transform" is jargon) |
| `cmd/cleat/plugin_cmd.go:75` | `"Error: %v"` | Include operation: `"Error loading manifest: %v"` |

### WASM generator

| File:Line | Current | Fix |
|-----------|---------|-----|
| `internal/wasm/metadata.go:108,112,171,184,188` | `"corrupt WASM: ..."` (5 messages) | Add byte offset: `"corrupt WASM at offset %d: ..."` |

### Python SDK

| File:Line | Current | Fix |
|-----------|---------|-----|
| `python-sdk/cleat_sdk/host_calls.py:328-877` | 34 `NotImplementedError` stubs with no WHERE | Include the function name: `"<func_name> can only be called within a cleat WASM runtime."` |
| `python-sdk/cleat_sdk/test_harness.py:459` | `"cleat_call failed: {stub.error}"` | Include service.operation |
| `python-sdk/scripts/build_wasm.py:154-155` | `"componentize-py failed with exit code N"` | Include stdout/stderr from the subprocess |
| `python-sdk/scripts/build_wasm.py:254` | `"WIT directory not found: {wit_dir}"` | Add: "Run `componentize-py init` or check that the SDK is installed." |
| `python-sdk/scripts/stamp_metadata.py:74,76` | `"LEB128 overflow"`, `"Incomplete LEB128 encoding"` | Replace jargon: `"invalid WASM binary: corrupted section length encoding at offset %d"` |

### AssemblyScript SDK

| File:Line | Current | Fix |
|-----------|---------|-----|
| `packages/cleat-as/assembly/host-calls.ts:874,1480` | `"unknown error"` with zero context | Include the operation name and error code |

### Rust SDK

| File:Line | Current | Fix |
|-----------|---------|-----|
| `crates/cleat-sdk/src/lib.rs:41` | `"serialize: {e}"` | Include the type name: `"serialize result of type {type_name}: {e}"` |
| `crates/cleat-sdk/src/plugins.rs:457,441` | `"parse error: {e}"` | Include plugin/function name context |
| `crates/cleat-sdk/src/test.rs:598` | `"SendSignalAndWait timed out"` | Include targetRunId, signalName, timeout value |
| `crates/cleat-sdk/src/bin/inject_metadata.rs:91,155` | `expect("Failed to read/write WASM file")` | Include the file path |

### Java SDK

| File:Line | Current | Fix |
|-----------|---------|-----|
| `crates/cleat-java/.../TestHostCalls.java:463` | `"plugin_call failed: " + stub.error` | Include pluginName.functionName |
| `crates/cleat-java/.../TestHostCalls.java:599` | `"SendSignalAndWait timed out"` | Include targetRunId, signalName |
| `crates/cleat-java/.../JsonHelper.java:200,219,237` | Several parser messages missing "what was found" | Include the actual character/value at the position |
| `crates/cleat-java/.../Saga.java:300,347` | Compensation message names wrong step | Name the step whose compensation actually failed, not just the last completed step |

---

## Part H: Missing HOW Guidance in Replay Divergence Messages

**File:** `internal/host/engine.go` (~8 locations)

Replay divergence messages are excellent on WHAT/WHERE/WHY (they include step numbers, expected
vs actual calls, and explain it's a non-determinism issue). But they never tell the developer
HOW to fix the non-determinism.

**Fix pattern:** Add to each divergence message:
```
"This usually means the workflow code was modified in a way that changes its execution path, or it uses a non-deterministic construct (time.Now(), random values, map iteration, goroutines). Run 'cleat vet' on your workflow code to check for common non-determinism issues."
```

---

## Implementation Priority

### Sprint 1 (Week 1) — Bugs & Security

1. **S1**: Fix JSON injection in AS plugins.ts (14 methods) — use `jsonEscape()` or `JsonBuilder`
2. **B1-B3**: Add error handling for silently-dropped engine.go errors — log at minimum, return error where possible
3. **B4-B6**: Fix silent error swallowing in Java JsonHelper, Plugins, TestHostCalls

### Sprint 2 (Week 1-2) — Opaque Error Codes

4. **Go adapter.go**: Define named error code constants, use them in generated messages (~18 locations)
5. **Python host_calls.py**: Map error codes to names, include parameter context (~15 locations)
6. **AS host-calls.ts**: Same pattern (~24 locations)

### Sprint 3 (Week 2) — Context & Clarity

7. **sharded_store.go**: Prefix 39 "no shard available" messages with operation names
8. **runtime.go**: Fix 30+ "not initialized" messages with WHY/HOW
9. **runtime.go**: Convert 4 panics to error returns
10. All remaining WHERE/HOW gaps from Part G (~25 locations)

### Sprint 4 (Week 2-3) — Generated Code & Tools

11. **engine.go**: Add HOW guidance to replay divergence messages (~8 locations)
12. **metadata.go**: Add byte offsets to corrupt WASM messages (5 locations)
13. **build_wasm.py**: Improve exit-code and timeout messages
14. **stamp_metadata.py + inject-metadata.js**: Replace LEB128 jargon

---

## Success Metrics

- Zero silently dropped errors in the hot path (engine.go)
- Zero JSON injection via string concatenation (AS plugins.ts)
- Every "error code N" message maps N to a human-readable name
- Every "no shard available" message identifies the operation that failed
- Every "not initialized" message explains WHY and HOW to fix it
- No panics from recoverable user errors
