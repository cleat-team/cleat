//go:build cgo

package engine

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/bytecodealliance/wasmtime-go/v44"
	"github.com/cleat-team/cleat/wasm"
	"github.com/tetratelabs/wazero/api"
)

// The host ABI is written twice.
//
// engine/imports.go registers the cleat_* functions on wazero's "env" host
// module; engine/wasmtime_hostfuncs*.go registers them on a wasmtime Linker.
// Both are live: wasmtime is the only WasmBackend and runs everything the
// worker executes, while engine.Runtime (wazero) still backs `cleatctl replay`,
// `cleatctl debug`, `cleat run`, `cleat-bench` and cleat/wasmtest.
//
// So a guest compiled against one must run against the other, and nothing made
// that true. Measured 2026-09-01, before these tests existed: both sides
// registered the same 56 cleat_* names and the sets were identical.
//
// BOTH NUMBERS IN THAT SENTENCE ARE NOW WRONG, in different ways, and the
// commands it used to give were the cause of the second:
//
//	grep -oE '"cleat_[a-z0-9_]+"' engine/imports.go | tr -d '"' | sort -u
//
// 56 is stale by one deletion -- cleat_child_workflow_in_schema was removed
// 2026-09-02 in #582, so the cleat_-prefixed count is 55. And cleat_* was
// never the right set: three host functions are unprefixed, so the engine
// registers 58. Re-derive without assuming the prefix:
//
//	grep -oE '\.Export\("[^"]+"\)' engine/imports.go | sed 's/.*Export("//;s/")//' | sort -u
//
// -- which is the good case, and exactly the case worth pinning. The two sides
// had already drifted in Go types without drifting in the ABI (wazero's
// cleat_sleep returns uint64, wasmtime's returns int64; both are i64 in WASM),
// which is what independent authorship looks like just before it becomes a bug.
//
// The decision this guards is recorded in IMPROVEMENT-PLAN.md: wazero stays,
// scoped to CLI and dev tooling, rather than being removed. Removal would make
// `cleat` CGO-only for no correctness gain, since #503 already made an
// unroutable language fail closed instead of silently falling back. Keeping a
// second implementation is only defensible if something checks it against the
// first.

// wasmSig is a function's WASM-level signature, which is the only level at
// which the two runtimes are comparable. The Go types differ on purpose:
// wazero's host functions take an api.Module, wasmtime's take a
// *wasmtime.Caller, and neither is a WASM parameter.
type wasmSig struct {
	params  []api.ValueType
	results []api.ValueType
}

func (s wasmSig) wat() string {
	var b strings.Builder
	b.WriteString("(func")
	if len(s.params) > 0 {
		b.WriteString(" (param")
		for _, p := range s.params {
			fmt.Fprintf(&b, " %s", api.ValueTypeName(p))
		}
		b.WriteString(")")
	}
	if len(s.results) > 0 {
		b.WriteString(" (result")
		for _, r := range s.results {
			fmt.Fprintf(&b, " %s", api.ValueTypeName(r))
		}
		b.WriteString(")")
	}
	b.WriteString(")")
	return b.String()
}

// wazeroCleatABI returns the cleat_* host functions wazero actually registers,
// with the signatures wazero actually derived from them.
//
// It builds the module the same way NewRuntime does -- through
// registerHostFunctions -- rather than reading the source, so it reflects what
// a guest would really link against.
func wazeroCleatABI(t *testing.T) map[string]wasmSig {
	t.Helper()
	ctx := context.Background()

	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(ctx) })

	// A second "env" module cannot be instantiated into the same runtime, so
	// build the host module against a throwaway runtime of its own and reuse
	// rt only as the receiver registerHostFunctions expects.
	probe, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime (probe): %v", err)
	}
	t.Cleanup(func() { _ = probe.Close(ctx) })

	builder := probe.wazeroRuntime.NewHostModuleBuilder("cleat_abi_probe")
	registerHostFunctions(builder, rt)
	mod, err := builder.Instantiate(ctx)
	if err != nil {
		t.Fatalf("instantiating the probe host module: %v", err)
	}

	out := map[string]wasmSig{}
	for name, def := range mod.ExportedFunctionDefinitions() {
		// NO PREFIX FILTER. Three host functions are not cleat_-prefixed --
		// plugin_call, plugin_call_streaming, set_query_state -- and a
		// `strings.HasPrefix(name, "cleat_")` here skipped all three, on both
		// sides, so this test compared 55 of 58 names and reported parity it
		// had never checked. See TestParityCoversEveryRegisteredHostFunction
		// below, which now fails if that filter comes back in any form.
		out[name] = wasmSig{params: def.ParamTypes(), results: def.ResultTypes()}
	}
	return out
}

// wasmtimeCleatLinker builds the linker the wasmtime backend really uses.
func wasmtimeCleatLinker(t *testing.T) (*wasmtime.Linker, *wasmtime.Store, *wasmtimeBackend) {
	t.Helper()
	ctx := context.Background()
	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { _ = b.Close(ctx) })

	linker := wasmtime.NewLinker(b.engine)
	var completeResult, completeErr string
	// needsWasi=false: the probe module below imports nothing but env.cleat_*.
	// abortTy=nil: registerEnvStubs then assumes the AssemblyScript shape, which
	// is irrelevant here and documented at engine/wasmtime_wasi.go:100.
	if err := b.registerAllImports(linker, &completeResult, &completeErr, false, nil); err != nil {
		t.Fatalf("registerAllImports: %v", err)
	}
	return linker, wasmtime.NewStore(b.engine), b
}

// TestWasmtimeSatisfiesEveryWazeroHostImport is the strong half.
//
// It builds a WASM module that imports every cleat_* function at exactly the
// signature wazero publishes, and links it against the wasmtime backend's real
// linker. Instantiation succeeds only if wasmtime defines every one of them at
// a matching type -- so a missing function, a changed parameter count, or an
// i32/i64 swap all fail here, and wasmtime names the offender itself.
//
// This is a conformance test rather than a text comparison: neither side's
// source is read, and both registration paths are the production ones.
func TestWasmtimeSatisfiesEveryWazeroHostImport(t *testing.T) {
	abi := wazeroCleatABI(t)
	if len(abi) == 0 {
		t.Fatal("wazero registered no cleat_* host functions.\n\n" +
			"That is not parity, it is an empty comparison: every assertion below " +
			"would pass against a runtime that registers nothing.")
	}

	names := make([]string, 0, len(abi))
	for n := range abi {
		names = append(names, n)
	}
	sort.Strings(names)

	var wat strings.Builder
	wat.WriteString("(module\n")
	for _, n := range names {
		fmt.Fprintf(&wat, "  (import \"env\" %q %s)\n", n, abi[n].wat())
	}
	wat.WriteString(")\n")

	wasmBytes, err := wasmtime.Wat2Wasm(wat.String())
	if err != nil {
		t.Fatalf("Wat2Wasm on the generated probe module: %v\n\nWAT:\n%s", err, wat.String())
	}

	linker, store, b := wasmtimeCleatLinker(t)
	module, err := wasmtime.NewModule(b.engine, wasmBytes)
	if err != nil {
		t.Fatalf("compiling the probe module: %v", err)
	}

	if _, err := linker.Instantiate(store, module); err != nil {
		t.Fatalf("the wasmtime backend cannot satisfy wazero's host ABI: %v\n\n"+
			"A guest compiled against one runtime would fail against the other. "+
			"wazero backs `cleatctl replay`, `cleatctl debug`, `cleat run`, "+
			"`cleat-bench` and cleat/wasmtest; wasmtime runs everything the worker "+
			"executes. Both must accept the same guest.\n\n"+
			"wasmtime names the mismatched import in the error above. Checked %d "+
			"cleat_* functions.", err, len(names))
	}
}

// fourParamCreatePromiseWat is a guest shaped like every SDK in the repo:
// cleat_create_promise takes four i32 parameters and returns i64, exactly as
// ABI.md 2.34 specifies.
const fourParamCreatePromiseWat = `(module
  (import "env" "cleat_create_promise" (func $cp (param i32 i32 i32 i32) (result i64)))
  (memory (export "memory") 1)
  (func (export "_start") (drop (call $cp (i32.const 0) (i32.const 0) (i32.const 0) (i32.const 0))))
)`

// TestCreatePromiseGuestLinksOnTheWorkerBackend is the regression test for the
// defect the parity guard above found on its first run.
//
// engine/wasmtime_hostfuncs_plugins.go registered cleat_create_promise with a
// fifth parameter, `ttlMs int64`, and discarded it with `_ = ttlMs`. An arity
// mismatch is a hard link error, so a guest that called cleat_create_promise
// could not instantiate on the wasmtime backend at all -- and the worker runs
// wasmtime exclusively. Durable promises were unusable in production for every
// SDK, while passing on wazero, which is what cleat/wasmtest and `cleat run`
// use.
//
// That split is why it survived: the runtime the tests exercise and the runtime
// the worker uses disagreed, and each was self-consistent.
//
// This test goes through the production path rather than a synthetic one:
// wasm.NeededEnvImports populates b.envNeeded exactly as
// backend_wasmtime.go:403 does, so skipIfNotNeeded behaves as it would for a
// real guest, and registerAllImports is the same call the executor makes.
func TestCreatePromiseGuestLinksOnTheWorkerBackend(t *testing.T) {
	ctx := context.Background()

	wasmBytes, err := wasmtime.Wat2Wasm(fourParamCreatePromiseWat)
	if err != nil {
		t.Fatalf("Wat2Wasm: %v", err)
	}

	b, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { _ = b.Close(ctx) })

	b.envNeeded = wasm.NeededEnvImports(wasmBytes)
	if !b.envNeeded["cleat_create_promise"] {
		t.Fatal("NeededEnvImports did not report cleat_create_promise for a guest that " +
			"imports it.\n\nWithout it skipIfNotNeeded skips the registration entirely " +
			"and the assertion below passes for the wrong reason -- there would be no " +
			"host function to mismatch against.")
	}

	linker := wasmtime.NewLinker(b.engine)
	var completeResult, completeErr string
	if err := b.registerAllImports(linker, &completeResult, &completeErr, false, nil); err != nil {
		t.Fatalf("registerAllImports: %v", err)
	}

	module, err := wasmtime.NewModule(b.engine, wasmBytes)
	if err != nil {
		t.Fatalf("NewModule: %v", err)
	}
	if _, err := linker.Instantiate(wasmtime.NewStore(b.engine), module); err != nil {
		t.Fatalf("a guest importing cleat_create_promise at the documented signature "+
			"cannot instantiate on the wasmtime backend: %v\n\n"+
			"ABI.md 2.34 specifies (param i32 i32 i32 i32) (result i64), and the Go, "+
			"Rust, Java and AssemblyScript SDKs all emit that. The worker runs wasmtime "+
			"exclusively, so this is durable promises being unusable in production.", err)
	}
}

// wasmtimeRegisteredNames extracts the env cleat_* names from the wasmtime
// registration source with go/ast.
//
// This exists because the instantiation test above is directional. It proves
// wasmtime satisfies everything wazero publishes; it cannot see a function
// wasmtime defines that wazero does not, because a linker with spare
// definitions instantiates a narrower module perfectly happily.
//
// That direction is the one that bites in production: a guest built against a
// new wasmtime-only host function runs on the worker and then fails under
// `cleatctl replay`, at the moment someone is trying to debug an incident.
func wasmtimeRegisteredNames(t *testing.T) map[string]string {
	t.Helper()
	matches, err := filepath.Glob("wasmtime_hostfuncs*.go")
	if err != nil || len(matches) == 0 {
		t.Fatalf("no wasmtime_hostfuncs*.go files found (err=%v).\n\n"+
			"If they were renamed, point this glob at the new name. Leaving it "+
			"unmatched turns this test into a vacuous pass.", err)
	}

	names := map[string]string{}
	fset := token.NewFileSet()
	for _, path := range matches {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			// Three spellings, differing only in where the module/name pair
			// starts:
			//
			//	b.hostFunc(linker, module, name, fn)   -- IMPROVEMENT-PLAN 3.90
			//	linker.FuncWrap(module, name, fn)
			//	linker.FuncNew(module, name, ty, fn)
			//
			// 3.90 routed every registration through the backend so the guest's
			// epoch budget can be bracketed around it. This test compares the
			// wasmtime and wazero host-ABI surfaces by NAME, so missing a
			// spelling does not make it fail -- it makes it see fewer names on
			// one side and report a parity gap that is really a parse gap. That
			// is what happened when 3.90 landed, and it is why the count check
			// at the bottom of this file exists.
			var argOff int
			switch sel.Sel.Name {
			case "hostFunc":
				argOff = 1
			case "FuncWrap", "FuncNew":
				argOff = 0
			default:
				return true
			}
			if len(call.Args) < argOff+2 {
				return true
			}
			mod, ok1 := stringLit(call.Args[argOff])
			name, ok2 := stringLit(call.Args[argOff+1])
			// Filtered on module only. A cleat_ prefix test here is what let
			// plugin_call, plugin_call_streaming and set_query_state drift
			// unwatched; see wazeroCleatABI.
			if !ok1 || !ok2 || mod != "env" {
				return true
			}
			names[name] = path
			return true
		})
	}
	return names
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

// TestNeitherRuntimeHasHostFunctionsTheOtherLacks closes the direction the
// instantiation test cannot see.
// TestParityCoversEveryRegisteredHostFunction asserts that the parity check
// above compares EVERY host function the engine registers, not a subset of them.
//
// This exists because it did not, for as long as it had existed. Both
// extraction sides carried `strings.HasPrefix(name, "cleat_")`, and three host
// functions are not prefixed: plugin_call, plugin_call_streaming and
// set_query_state. So the test that exists to catch ABI drift between the two
// runtimes compared 55 of 58 names and was structurally blind to three -- one
// of which, plugin_call, is the most-exercised host call in the repo
// (IMPROVEMENT-PLAN 3.306, #730, #732, #754).
//
// Measured before the fix, by renaming one registration on the wasmtime side
// and running the parity test:
//
//	plugin_call -> plugin_call_DRIFTED   ok      (silent)
//	cleat_sleep -> cleat_sleep_DRIFTED   FAIL    "wazero registers host
//	                                              functions wasmtime does not"
//
// The same defect, detected or not detected purely on whether the name began
// with five particular characters. That is why this asserts COVERAGE rather
// than a count: a count would have read 55 and looked healthy, which is what
// the doc comment on this file did until 2026-09-05.
func TestParityCoversEveryRegisteredHostFunction(t *testing.T) {
	compared := wazeroCleatABI(t)

	// The authority for "what the engine registers" is imports.go itself,
	// read as source rather than through the same builder the comparison
	// uses -- otherwise a filter applied in registerHostFunctions would hide
	// from both, and this test would agree with the bug.
	src, err := os.ReadFile("imports.go")
	if err != nil {
		t.Fatalf("reading imports.go: %v", err)
	}
	registered := map[string]bool{}
	for _, m := range regexp.MustCompile(`\.Export\("([^"]+)"\)`).FindAllStringSubmatch(string(src), -1) {
		registered[m[1]] = true
	}
	if len(registered) < 40 {
		t.Fatalf("found only %d .Export( registrations in imports.go, expected at least 40.\n\n"+
			"If registration moved behind a helper, this scan sees nothing and "+
			"reports coverage it never checked.", len(registered))
	}

	var missing []string
	for name := range registered {
		if _, ok := compared[name]; !ok {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("the runtime parity check does not compare %d of %d registered host "+
			"functions: %s\n\n"+
			"Every name imports.go exports must be compared, or the two runtimes can "+
			"drift on exactly the ones that are skipped and every parity test still "+
			"passes. This failed for plugin_call, plugin_call_streaming and "+
			"set_query_state until 2026-09-05, because both extraction sides filtered "+
			"on a cleat_ prefix.",
			len(missing), len(registered), strings.Join(missing, ", "))
	}
}

func TestNeitherRuntimeHasHostFunctionsTheOtherLacks(t *testing.T) {
	wazeroABI := wazeroCleatABI(t)
	wasmtimeNames := wasmtimeRegisteredNames(t)

	// Vacuous-pass control. Two empty sets are equal, and an equality
	// assertion over them would be the most confidently green nothing in the
	// repo. 58 on each side as of 2026-09-05 (55 until the cleat_ prefix
	// filter was removed -- see TestParityCoversEveryRegisteredHostFunction);
	// the floor is deliberately well below that so ordinary additions do not
	// trip it. It is a floor and not an equality on purpose: an equality here
	// would be a second place to update on every ABI addition, and the set
	// comparison below is what actually has to hold.
	const floor = 40
	if len(wazeroABI) < floor {
		t.Fatalf("wazero exposes only %d cleat_* functions, expected at least %d.\n\n"+
			"Either the ABI shrank dramatically or this test stopped finding it.",
			len(wazeroABI), floor)
	}
	if len(wasmtimeNames) < floor {
		t.Fatalf("found only %d cleat_* registrations in wasmtime_hostfuncs*.go, "+
			"expected at least %d.\n\n"+
			"The AST walk looks for b.hostFunc/linker.FuncWrap/FuncNew with a literal \"env\" "+
			"module and a literal name. If registration moved behind a helper or a "+
			"variable, this walk sees nothing and reports parity it never checked.",
			len(wasmtimeNames), floor)
	}

	var wasmtimeOnly []string
	for n, path := range wasmtimeNames {
		if _, ok := wazeroABI[n]; !ok {
			wasmtimeOnly = append(wasmtimeOnly, fmt.Sprintf("%s (%s)", n, path))
		}
	}
	sort.Strings(wasmtimeOnly)
	if len(wasmtimeOnly) > 0 {
		t.Errorf("wasmtime registers host functions wazero does not: %s\n\n"+
			"A guest using one of these runs on the worker and then fails under "+
			"`cleatctl replay` or `cleatctl debug` -- at the moment someone is "+
			"trying to debug an incident. Add them to registerHostFunctions in "+
			"engine/imports.go.", strings.Join(wasmtimeOnly, ", "))
	}

	var wazeroOnly []string
	for n := range wazeroABI {
		if _, ok := wasmtimeNames[n]; !ok {
			wazeroOnly = append(wazeroOnly, n)
		}
	}
	sort.Strings(wazeroOnly)
	if len(wazeroOnly) > 0 {
		t.Errorf("wazero registers host functions wasmtime does not: %s\n\n"+
			"These would fail on the worker, which runs wasmtime exclusively. Add "+
			"them to registerAllImports in engine/backend_wasmtime.go.",
			strings.Join(wazeroOnly, ", "))
	}
}
