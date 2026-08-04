package main

// wasmtimeLanguages are the guest languages routed to the wasmtime backend.
// Everything else falls back to wazero.
//
// This list has to be stated explicitly now, and that is the point.
//
// Until 2026-08-04 the only registered language was "go", and AssemblyScript
// and TeaVM-compiled Java reached wasmtime anyway -- by accident. DetectLanguage
// identifies them from their import section, that parse had been broken since it
// was written (see wasm/skipImportDesc), and DetectLanguage's fallback for "I
// could not tell" is "go". Two languages were running on the primary backend
// because a parser was failing.
//
// Fixing the parser without this list would therefore have *removed* them from
// wasmtime: correctly identified as "assemblyscript" and "java", they would have
// matched nothing in the backend map and silently dropped to wazero. A bug fix
// that quietly downgrades two languages onto the fallback runtime is worse than
// the bug, and nothing in the test suite would have said so.
//
// Both were verified to load and execute on wasmtime before being listed here,
// against the checked-in plugin-harness fixtures.
//
// Not listed, and why:
//
//   - rust: loads and executes on wasmtime -- verified against
//     examples/rust-workflow -- but wasm/metadata.go routes it away deliberately,
//     "so Rust modules fall through to the default runtime instead of crashing in
//     wasmtime". That reason may well be stale, since it does not crash now, but
//     tests/cross-language is currently run by no CI job, so there is no signal
//     to regress against. Wire that suite up first.
//   - python: genuinely blocked. Its Component Model binary needs the native
//     component path in engine/component_cgo.go, which is behind the
//     wasmtime_component_cgo build tag that no build sets. See IMPROVEMENT-PLAN.
var wasmtimeLanguages = []string{"go", "assemblyscript", "java"}

// runsOnWasmtime reports whether a detected guest language is served by the
// wasmtime backend. It must agree with wasmtimeLanguages: the engine consults
// the backend map, while the worker uses this to decide whether to build a
// wazero Runtime at all, and a disagreement means either a wasted runtime or a
// workflow with no backend.
func runsOnWasmtime(lang string) bool {
	for _, l := range wasmtimeLanguages {
		if l == lang {
			return true
		}
	}
	return false
}
