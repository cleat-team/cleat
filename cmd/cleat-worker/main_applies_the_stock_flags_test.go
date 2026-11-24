package main

import (
	"context"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// cleat#4212. #4171's tests read each flag's DECLARED default and compare it with the engine's constant, and pass a flag value through a helper main
// calls. None of them ran the code main runs to APPLY the flags, so a stock worker's real engine values were inferred, never measured: that is how a 32 KiB
// output-buffer override survived next to the engine's 1 MiB.
//
// Three tests close that together, and none is enough alone:
//
//  1. the factored functions, run with the flags at their stock values, give the engine its stock values (the globals read back directly; the memory
//     limit observed through a REAL backend built from the same options, because WasmtimeOption is an opaque closure);
//  2. a different flag value DOES change the engine value, so (1) is not satisfied by a function that ignores the flags;
//  3. main() calls both functions, unconditionally, which is the one thing (1) and (2) cannot show.
//
// The memory fixtures are in engine/testdata, outside this package directory, so a local `go test` here does not notice an edited fixture (go test caches on
// files opened inside the package directory); CI passes -count=1.

// setFlag sets one flag on the real command line and restores it when the test ends.
func setFlag(t *testing.T, name, value string) {
	t.Helper()
	f := flag.CommandLine.Lookup(name)
	if f == nil {
		t.Fatalf("flag --%s is not declared", name)
	}
	prev := f.Value.String()
	t.Cleanup(func() { _ = f.Value.Set(prev) })
	if err := f.Value.Set(value); err != nil {
		t.Fatalf("set --%s=%q: %v", name, value, err)
	}
}

// stockFlag puts one flag at its declared default, whatever an earlier test left in it.
func stockFlag(t *testing.T, name string) {
	t.Helper()
	f := flag.CommandLine.Lookup(name)
	if f == nil {
		t.Fatalf("flag --%s is not declared", name)
	}
	setFlag(t, name, f.DefValue)
}

func TestStockFlagsGiveTheEngineItsStockGlobals(t *testing.T) {
	stockFlag(t, "wasm-output-buffer-size")
	stockFlag(t, "wasm-max-string-len")
	prevOut, prevStr := engine.OutBufSize, engine.MaxWasmStringLen
	t.Cleanup(func() { engine.OutBufSize, engine.MaxWasmStringLen = prevOut, prevStr })

	// Sentinels the engine would never choose, so a function that did nothing cannot pass by finding the stock value already there.
	engine.OutBufSize, engine.MaxWasmStringLen = 7, 7
	applyEngineGlobalsFromFlags()
	if engine.OutBufSize != engine.DefaultOutBufSize {
		t.Errorf("a stock worker's engine.OutBufSize = %d, want the engine's own default %d", engine.OutBufSize, engine.DefaultOutBufSize)
	}
	if engine.DefaultOutBufSize != 1<<20 {
		t.Errorf("engine.DefaultOutBufSize = %d, want 1 MiB (cleat#4171, owner decision); change this with that decision, not around it", engine.DefaultOutBufSize)
	}
	if engine.MaxWasmStringLen != engine.DefaultMaxWasmStringLen {
		t.Errorf("a stock worker's engine.MaxWasmStringLen = %d, want %d, the engine's own default", engine.MaxWasmStringLen, engine.DefaultMaxWasmStringLen)
	}

	// The negative control: a different flag value reaches the engine, so the stock values above are the flags' doing.
	setFlag(t, "wasm-output-buffer-size", "12345")
	setFlag(t, "wasm-max-string-len", "54321")
	applyEngineGlobalsFromFlags()
	if engine.OutBufSize != 12345 || engine.MaxWasmStringLen != 54321 {
		t.Errorf("flags set to 12345 and 54321 gave the engine %d and %d: applyEngineGlobalsFromFlags does not apply the flags", engine.OutBufSize, engine.MaxWasmStringLen)
	}
}

type noopCaller struct{}

func (noopCaller) Call(context.Context, string, string, string) (string, error) { return `{}`, nil }

// runOnTheWorkersBackend runs one of the memory fixtures on a wasmtime backend built from wasmtimeOptionsFromFlags, as the worker builds it.
func runOnTheWorkersBackend(t *testing.T, fixture string) (string, error) {
	t.Helper()
	wasmBytes, err := os.ReadFile(filepath.Join("..", "..", "engine", "testdata", "wasi_minor", fixture))
	if err != nil {
		t.Fatalf("read %s: %v (engine/testdata/wasi_minor/build.sh regenerates it)", fixture, err)
	}
	ctx := context.Background()
	rt, err := engine.NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	wt, err := engine.NewWasmtimeBackend(ctx, wasmtimeOptionsFromFlags(slog.New(slog.NewTextHandler(io.Discard, nil)))...)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { wt.Close(ctx) })
	eng := engine.NewEngine(rt, noopCaller{}, engine.WithBackends(engine.WasmtimeLanguages, wt))
	res, _, _, _, _, err := eng.Execute(ctx, wasmBytes, "run", []byte(`{}`))
	return res, err
}

// The memory limit a stock worker's backend enforces, measured by what it will and will not run: the 44 MiB component (the least a .NET guest needs, and
// under the 256 MiB default) runs, and the 260 MiB one (over the default) is refused. The explicit-512 row is the negative control: the same component runs
// when the flag says so, so the refusal is the flag-derived limit and not a fixture that cannot run.
func TestAStockWorkersBackendEnforcesA256MiBMemoryLimit(t *testing.T) {
	stockFlag(t, "wasm-memory-max-mb")

	if res, err := runOnTheWorkersBackend(t, "bigmem.wasm"); err != nil || res != `{"ok":1}` {
		t.Fatalf("a stock worker's backend did not run the 44 MiB component: result %q, err %v", res, err)
	}
	if res, err := runOnTheWorkersBackend(t, "bigmem-over-default.wasm"); err == nil {
		t.Fatalf("a stock worker's backend RAN a 260 MiB component (result %q): the memory limit it was built with is above the 256 MiB default or absent", res)
	}

	setFlag(t, "wasm-memory-max-mb", "512")
	if res, err := runOnTheWorkersBackend(t, "bigmem-over-default.wasm"); err != nil || res != `{"ok":1}` {
		t.Fatalf("PRECONDITION: with --wasm-memory-max-mb=512 the 260 MiB component must run (result %q, err %v), or its refusal above proves nothing", res, err)
	}
}

// callsOnTheLeg lists which of names are called from main's own top-level statements: not from inside an if, for, switch or block (those run
// conditionally), and not from inside a function literal (which runs when something else decides). "Unconditionally" is the requirement, because an
// assignment that only happens on some path is the same gap as one nobody tests.
func callsOnTheLeg(t *testing.T, src string, names ...string) (found map[string]bool) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	found = map[string]bool{}
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "main" || fn.Recv != nil || fn.Body == nil {
			continue
		}
		for _, stmt := range fn.Body.List {
			switch stmt.(type) {
			case *ast.ExprStmt, *ast.AssignStmt, *ast.DeclStmt, *ast.DeferStmt:
			default:
				continue // an if, for, switch, select, block, go or return: not unconditional
			}
			ast.Inspect(stmt, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.FuncLit:
					return false
				case *ast.CallExpr:
					if id, ok := x.Fun.(*ast.Ident); ok && want[id.Name] {
						found[id.Name] = true
					}
				}
				return true
			})
		}
	}
	return found
}

func TestMainAppliesTheFlagsThroughTheFunctionsTheTestsRun(t *testing.T) {
	names := []string{"applyEngineGlobalsFromFlags", "wasmtimeOptionsFromFlags"}

	// Known positive: the real main.go. A scan that matched nothing could not pass this.
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	got := callsOnTheLeg(t, string(src), names...)
	var missing []string
	for _, n := range names {
		if !got[n] {
			missing = append(missing, n)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("main() does not call %v from one of its own top-level statements: the flags are not applied through the functions the tests run (cleat#4212)", missing)
	}

	// Negative controls: the scan must say "missing" for each way a call can fail to be unconditional, or the check above decides nothing.
	for _, c := range []struct {
		name, body string
		want       bool
	}{
		{"absent", `x := 1; _ = x`, false},
		{"inside an if", `if len(os.Args) > 1 { applyEngineGlobalsFromFlags() }`, false},
		{"inside a function literal", `f := func() { applyEngineGlobalsFromFlags() }; _ = f`, false},
		{"inside a for", `for i := 0; i < 1; i++ { applyEngineGlobalsFromFlags() }`, false},
		{"a top-level statement", `applyEngineGlobalsFromFlags()`, true},
		{"an argument of a top-level assignment", `wt, err := engine.NewWasmtimeBackend(ctx, applyEngineGlobalsFromFlags()...); _, _ = wt, err`, true},
	} {
		res := callsOnTheLeg(t, "package main\nfunc main() {\n"+c.body+"\n}\n", "applyEngineGlobalsFromFlags")
		if res["applyEngineGlobalsFromFlags"] != c.want {
			t.Errorf("scan control %q: found = %v, want %v", c.name, res["applyEngineGlobalsFromFlags"], c.want)
		}
	}
}
