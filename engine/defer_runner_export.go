package engine

// deferRunnerExport is the export a guest emits so the HOST can drain the defer
// table of a workflow it killed. IMPROVEMENT-PLAN §3.35 phase 4.
//
// Every SDK emits it: Go codegen (wasm/exports.go), the Rust proc macro, the
// AssemblyScript transformer, and Java's CleatEntryProcessor. Python does not,
// and cannot yet -- its WIT world exports one function (§3.73).
//
// It lives in its own file, with no build tag, because both execution paths
// need it and only one of them has CGO. It was declared in
// backend_wasmtime.go, which is `//go:build cgo`, so the wazero path in
// executor.go could not name the export it was supposed to be calling -- and
// called `cleat_defer_<id>` instead, a convention no guest in any language has
// ever produced.
//
// Re-derive the producers with:
//
//	grep -rn "__cleat_run_deferred" --include="*.go" --include="*.rs" \
//	  --include="*.js" --include="*.java" . | grep -v node_modules
const deferRunnerExport = "__cleat_run_deferred"
