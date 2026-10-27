package engine

// deferRunnerExport is the export a guest emits so the HOST can drain the defer
// table of a workflow it killed. IMPROVEMENT-PLAN §3.35 phase 4.
//
// Every core-module SDK emits it: Go codegen (wasm/exports.go), the Rust proc
// macro, the AssemblyScript transformer, and Java's CleatEntryProcessor.
// Python is a Component Model guest and exports componentDeferRunnerExport
// below instead -- the same job, spelled the way a WIT world spells it.
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

// componentDeferRunnerExport is deferRunnerExport for a Component Model guest:
// the `run-deferred` export of the cleat-workflow world
// (python-sdk/wit/cleat.wit).
//
// A different name because it is a different kind of thing. The core-module
// export is a raw WASM function whose name is a private convention between
// cleat's codegen and cleat's host, so it carries the double underscore that
// says "nobody else's". A WIT export is part of a published interface: its
// name is kebab-case because that is what WIT names look like, and the world
// declaring it is what makes it exist rather than a codegen step.
//
// This is why the two are not one constant. The lookup paths differ too --
// instance.GetFunc against wasmtime_component_instance_get_export_index -- so
// there is no call site that could take either.
const componentDeferRunnerExport = "run-deferred"

// componentRunExport is the `run` export every cleat-workflow world component
// has, whatever the workflow's own logical entry point is named
// (python-sdk/wit/cleat.wit: "export run: func(args: string) -> run-outcome;").
//
// cleat#2937. A core-module guest (Go, Rust, Java, AssemblyScript) exports a
// function NAMED AFTER the logical entry point, so determineEntryPoint's
// result is a real export name there and ExecuteComponentCGo's sibling path
// can call it directly. A Component Model guest cannot: the WIT world
// declares exactly one entry export, always named "run", for every workflow
// regardless of how many @cleat_entry functions it registers or what they
// are called. Calling componentGetFunc with the LOGICAL name instead of this
// constant is exactly cleat#2937's defect -- "component export \"PlaceOrder\"
// not found" against a component that exports "run" and nothing else by that
// name.
//
// The logical name is not discarded; the caller folds it into the input JSON
// as `__cleat_entry__` before this export is called, which is how Python's
// own _dispatcher_run (entry.py) knows which @cleat_entry wrapper to invoke
// when a module registers more than one.
const componentRunExport = "run"
