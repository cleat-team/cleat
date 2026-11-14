package main

// cleat#3803, cleat#3805. A workflow that calls an SDK function which makes a host call on its behalf -- the
// documented cleat.NewPromiseTyped, Promise.Await, SagaTyped.Run, CallTyped, PollUntil -- built clean, linked no
// import for it, and failed at run time with "the HostCalls runtime was not initialized" (or a nil dereference,
// when the function holding `h` was not seen as using host calls and so was never handed it).
//
// The build used to learn what an SDK function needs from two hand lists keyed by "Type.Method". A package-level
// function has no such key, and a method on a generic type never reached the list. It now reads the function's
// own body (analyzer.DepHostCalls), so these tests are not a mirror of a list: each fixture under
// testdata/sdkhelpers calls ONE SDK API, writes no host call of its own, and is asked what it imports.

import (
	"context"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero"

	"github.com/cleat-team/cleat/internal/analyzer"
)

// sdkFixtures: fixture directory -> the imports its one SDK call must link. Asserted as a subset: an SDK
// function that reaches more (Promise.Await also wires the update handshake) is not a defect, a missing one is.
var sdkFixtures = []struct {
	name   string
	want   []string
	leaves []string // short names that must be durable leaves; the entry point is always Run
}{
	{"promisetyped", []string{"cleat_create_promise", "cleat_await_promise"}, []string{"Run"}},
	// The seven imports the hand-list row for PluginFunc.WaitFor carried (cleat#3794), now derived.
	{"waitfor", []string{"cleat_wait_for_event", "plugin_call", "cleat_await_signals", "cleat_now", "cleat_complete_update", "cleat_log", "cleat_poll_update"}, []string{"Run"}},
	{"pluginviadep", []string{"plugin_call", "plugin_call_streaming"}, []string{"Run"}},
	{"sagarun", []string{"cleat_log", "set_query_state"}, []string{"Run"}},
	{"sagatyped", []string{"cleat_log"}, []string{"Run"}},
	{"calltyped", []string{"cleat_call"}, []string{"Run"}},
	{"polluntil", []string{"cleat_now", "cleat_sleep"}, []string{"Run"}},
	{"awaitcondition", []string{"cleat_await_signals", "cleat_now"}, []string{"Run"}},
	{"sideeffecttyped", []string{"cleat_side_effect"}, []string{"Run"}},
	{"typedupdate", []string{"cleat_register_update_handler"}, []string{"Run"}},
	{"plugincalltyped", []string{"plugin_call"}, []string{"Run"}},
	{"pluginfunc", []string{"plugin_call"}, []string{"Run"}},
	// The helper never names a HostCalls method, so it is a leaf only if the call into the SDK counts.
	{"globalhhelper", []string{"cleat_create_promise"}, []string{"newApproval"}},
}

func sdkFixturePattern(t *testing.T, name string) string {
	return filepath.Join(testdataDir(t), "sdkhelpers", name)
}

// A fixture that wrote a host call itself could not tell "the SDK function was seen" from "the workflow named
// the same call". This is the control that keeps the table above meaning something.
func TestTheSDKFixturesWriteNoHostCallOfTheirOwn(t *testing.T) {
	hostCall := regexp.MustCompile(`\bh\.[A-Z]\w*\(`)
	for _, f := range sdkFixtures {
		files, _ := filepath.Glob(filepath.Join(sdkFixturePattern(t, f.name), "*.go"))
		if len(files) == 0 {
			t.Fatalf("%s: no Go file", f.name)
		}
		src, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatalf("%s: %v", f.name, err)
		}
		var code []string
		for _, line := range strings.Split(string(src), "\n") {
			if i := strings.Index(line, "//"); i >= 0 {
				line = line[:i]
			}
			code = append(code, line)
		}
		if m := hostCall.FindString(strings.Join(code, "\n")); m != "" {
			t.Errorf("testdata/sdkhelpers/%s writes %q itself; the import it is asked for could then come from here rather than from the SDK function", f.name, m)
		}
	}
}

func TestAnSDKFunctionsHostCallsAreWiredIntoTheWorkflowThatCallsIt(t *testing.T) {
	for _, f := range sdkFixtures {
		f := f
		t.Run(f.name, func(t *testing.T) {
			_, cg, cr, terrs, usage, _ := analyze(sdkFixturePattern(t, f.name))

			for _, imp := range f.want {
				if !usage.Used[imp] {
					t.Errorf("the module does not import %s, so the call reaches a nil adapter at run time (imports linked: %v)", imp, sortedKeys(usage.Used))
				}
			}
			for _, leaf := range f.leaves {
				found := false
				for fq := range cg.DurableLeaves {
					if strings.HasSuffix(fq, "."+leaf) {
						found = true
					}
				}
				if !found {
					t.Errorf("%s is not a durable leaf, so it is outside the cleat closure and is never handed h (leaves: %v)", leaf, sortedKeys(cg.DurableLeaves))
				}
			}
			if len(terrs) != 0 || len(cr.Errors) != 0 {
				t.Errorf("analysis refused a fixture that is valid: threading=%v closure=%v", terrs, cr.Errors)
			}
			if msg := opaqueHostCallError(usage); msg != "" {
				t.Errorf("a fixture calling one documented SDK function was refused as unreadable:\n%s", msg)
			}
		})
	}
}

// The scan the issue asked for, kept as a guard. Every exported function or method in the SDK that reaches a
// host call must have a fixture here (or a named other fixture that already covers it). The BUILD does not depend
// on this list -- it reads bodies -- so a function missing from it is not a silent gap, it is a failing test that
// asks for one fixture.
func TestEverySDKFunctionThatReachesAHostCallHasAFixture(t *testing.T) {
	covered := map[string]string{
		"NewPromiseTyped":            "promisetyped",
		"Promise.Await":              "promisetyped",
		"SagaTyped.Run":              "sagatyped",
		"CallTyped":                  "calltyped",
		"PollUntil":                  "polluntil",
		"AwaitCondition":             "awaitcondition",
		"SideEffectTyped":            "sideeffecttyped",
		"RegisterTypedUpdateHandler": "typedupdate",
		"PluginCallTyped":            "plugincalltyped",
		"PluginFunc.Call":            "pluginfunc",
		"PluginFunc.Bind":            "pluginfunc",
		"PluginFunc.WaitFor":         "waitfor",
		"PluginFunc.WithWake":        "waitfor",
		"BoundPluginFunc.Call":       "pluginfunc",
		// Covered by fixtures that predate this one.
		"Saga.AddStepCall":   "testdata/sagaparameterised",
		"Saga.Run":           "sagarun",
		"Saga.RunWithResult": "sagarun",
		"Selector.Select":    "wasm: TestSelector*",
		"Selector.AddTimer":  "wasm: TestSelector*",
		// agentworkflow is wired by its own //cleat:require directive as well; see testdata/agentguest.
		"agentworkflow.Run":        "testdata/agentguest",
		"agentworkflow.RunAsChild": "testdata/agentclient",
		// dagrun is wired by its //cleat:require directive too; see testdata/dagguest.
		"dagrun.DAG.Execute":            "testdata/dagguest",
		"dagrun.DAG.ExecuteWithOptions": "testdata/dagguest",
	}

	var reaching []string
	seen := map[string]bool{}
	for _, pattern := range []string{"agentguest", "dagguest"} {
		fset := token.NewFileSet()
		result, err := analyzer.LoadPackages(filepath.Join(testdataDir(t), pattern), fset)
		if err != nil {
			t.Fatalf("LoadPackages(%s): %v", pattern, err)
		}
		for _, p := range result.ImportedPkgs {
			if !strings.HasPrefix(p.Path, "github.com/cleat-team/cleat/cleat") || seen[p.Path] {
				continue
			}
			seen[p.Path] = true
			for _, file := range p.Files {
				for _, decl := range file.Decls {
					fd, ok := decl.(*ast.FuncDecl)
					if !ok || !fd.Name.IsExported() || fd.Body == nil {
						continue
					}
					key := fd.Name.Name
					if fd.Recv != nil && len(fd.Recv.List) == 1 {
						recv := strings.TrimPrefix(types.ExprString(fd.Recv.List[0].Type), "*")
						if i := strings.Index(recv, "["); i >= 0 {
							recv = recv[:i]
						}
						// HostCallsImpl IS the runtime: its methods call the adapter, they are not callers of it.
						if recv == "HostCallsImpl" || !ast.IsExported(recv) {
							continue
						}
						key = recv + "." + fd.Name.Name
					}
					if p.Name != "cleat" {
						key = p.Name + "." + key
					}
					fn, _ := p.Info.Defs[fd.Name].(*types.Func)
					if s := result.DepHostCalls(fn); s != nil && (len(s.Methods) > 0 || s.PluginCall) {
						reaching = append(reaching, key)
					}
				}
			}
		}
	}
	sort.Strings(reaching)
	if len(reaching) < 15 {
		t.Fatalf("the scan found %d SDK functions that reach a host call, expected at least 15 -- it is matching almost nothing and the check below would pass on an empty population: %v", len(reaching), reaching)
	}
	for _, key := range reaching {
		if _, ok := covered[key]; !ok {
			t.Errorf("%s reaches a host call and no fixture covers it. Add testdata/sdkhelpers/<name> calling just this API and a row in sdkFixtures (the build itself reads the body, so this is not a gap in the build, it is a gap in what is proven).", key)
		}
	}
}

// An SDK function used as a VALUE (`mk := cleat.NewPromiseTyped[string]; mk(h, ...)`) is the one shape a reference
// scan cannot attribute to a call site. It is refused rather than built, by the closure analysis' E009
// ("function-value calls cannot be statically resolved"), and this pins that: if E009 is ever relaxed, this is the
// test that says the SDK-function route needs a decision, instead of a module that fails at run time.
func TestAnSDKFunctionUsedAsAValueIsRefusedAndNotMiswired(t *testing.T) {
	_, _, cr, _, usage, _ := analyze(sdkFixturePattern(t, "funcvalue"))
	refused := false
	for fn, errs := range cr.Errors {
		for _, e := range errs {
			if strings.HasSuffix(fn, ".Run") && e.Code == "E009" {
				refused = true
			}
		}
	}
	if !refused {
		t.Errorf("a call through a function value that holds an SDK function was not refused (errors: %v); the build would emit a module whose import depends on a call it cannot attribute", cr.Errors)
	}
	// The reference itself is still counted, so even a relaxed E009 would not lose the import.
	if !usage.Used["cleat_create_promise"] {
		t.Errorf("a reference to cleat.NewPromiseTyped as a value linked no cleat_create_promise (imports: %v)", sortedKeys(usage.Used))
	}
}

// Two dependency packages that each declare a method of the same name on a type of the same name must not be
// taken for one another. The index was keyed by analyzer.FuncFQName, which carries no package path for a method,
// so the later package's declaration replaced the earlier one's and a workflow calling the earlier one linked no
// import and had no durable leaf (found in review of cleat#3814). stampclash calls the one that makes a host call
// and stamponlyb, the control, the one that makes none.
func TestTwoDependenciesDeclaringTheSameMethodAreNotConflated(t *testing.T) {
	_, cg, _, _, usage, _ := analyze(sdkFixturePattern(t, "stampclash"))
	if !usage.Used["cleat_now"] {
		t.Errorf("stampa.Client.Stamp makes a host call and stampb.Client.Stamp, declared the same way, makes none; the build linked no cleat_now (imports: %v), so it took the wrong one", sortedKeys(usage.Used))
	}
	found := false
	for fq := range cg.DurableLeaves {
		if strings.HasSuffix(fq, ".Run") {
			found = true
		}
	}
	if !found {
		t.Errorf("Run calls the Stamp that reads the clock and is not a durable leaf (leaves: %v)", sortedKeys(cg.DurableLeaves))
	}

	_, cg2, _, _, usage2, _ := analyze(sdkFixturePattern(t, "stamponlyb"))
	if usage2.Used["cleat_now"] {
		t.Errorf("stamponlyb calls only the Stamp that makes no host call, and the build linked cleat_now: methods are matched by name across packages")
	}
	for fq := range cg2.DurableLeaves {
		if strings.HasSuffix(fq, ".Run") {
			t.Errorf("stamponlyb's Run was made a durable leaf by a Stamp that makes no host call")
		}
	}
}

// What the build links is what the COMPILED MODULE imports. This builds the documented typed promise, a method on
// a generic type, and the package-level-h helper for real and reads the import section of the .wasm, which is the
// thing the worker links against.
func TestACompiledModuleImportsTheHostCallsOfTheSDKFunctionsItCalls(t *testing.T) {
	for _, name := range []string{"promisetyped", "sagatyped", "globalhhelper"} {
		name := name
		t.Run(name, func(t *testing.T) {
			outDir := t.TempDir()
			runBuild(sdkFixturePattern(t, name), outDir, "go", "", "", false, false, false, 1)

			wasmPath := filepath.Join(outDir, "main.wasm")
			b, err := os.ReadFile(wasmPath)
			if err != nil {
				t.Fatalf("reading the built module: %v", err)
			}
			got := wasmImportNames(t, b)

			var want []string
			for _, f := range sdkFixtures {
				if f.name == name {
					want = f.want
				}
			}
			for _, imp := range want {
				if !got[imp] {
					t.Errorf("the built module does not import %s (it imports %v), so the call fails at run time", imp, sortedKeys(got))
				}
			}
		})
	}
}

func wasmImportNames(t *testing.T, module []byte) map[string]bool {
	t.Helper()
	ctx := context.Background()
	rt := wazero.NewRuntime(ctx)
	defer rt.Close(ctx)
	cm, err := rt.CompileModule(ctx, module)
	if err != nil {
		t.Fatalf("compiling the built module: %v", err)
	}
	out := map[string]bool{}
	for _, f := range cm.ImportedFunctions() {
		if _, name, isImport := f.Import(); isImport {
			out[name] = true
		}
	}
	return out
}

// The refusal. A dependency function that passes the runtime to an interface method could run anything; "no host
// calls" would be a guess, and the module it produced could fail at run time. The build says it cannot tell.
func TestTheBuildRefusesAWorkflowThatReachesCodeItCannotRead(t *testing.T) {
	_, _, _, _, usage, _ := analyze(sdkFixturePattern(t, "opaqueuser"))
	msg := opaqueHostCallError(usage)
	t.Logf("the refusal reads:\n%s", msg)
	if msg == "" {
		t.Fatalf("no refusal for a workflow that reaches opaquedep.Do, which hands the runtime to an interface method (opaque: %+v)", usage.OpaqueHostCalls)
	}
	for _, want := range []string{"opaquedep.Do", "r.Run", "opaquedep.go", "//cleat:require"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not name %q:\n%s", want, msg)
		}
	}
	// And it is reported where `cleat vet` and `cleat build` both take their refusals from.
	found := false
	for _, r := range analysisRefusals(usage, "") {
		if strings.Contains(r, "opaquedep.Do") {
			found = true
		}
	}
	if !found {
		t.Error("analysisRefusals does not carry the refusal, so vet and build would not both report it")
	}
}

// No function in the SDK is opaque today. If one becomes so, every workflow reaching it would be refused, so this
// is the test that says so before a release does.
func TestNoSDKFunctionHandsTheRuntimeToCodeTheBuildCannotRead(t *testing.T) {
	for _, pattern := range []string{"agentguest", "dagguest"} {
		fset := token.NewFileSet()
		result, err := analyzer.LoadPackages(filepath.Join(testdataDir(t), pattern), fset)
		if err != nil {
			t.Fatalf("LoadPackages(%s): %v", pattern, err)
		}
		for _, p := range result.ImportedPkgs {
			if !strings.HasPrefix(p.Path, "github.com/cleat-team/cleat/cleat") {
				continue
			}
			for _, file := range p.Files {
				for _, decl := range file.Decls {
					fd, ok := decl.(*ast.FuncDecl)
					if !ok || fd.Body == nil {
						continue
					}
					fn, _ := p.Info.Defs[fd.Name].(*types.Func)
					if s := result.DepHostCalls(fn); s != nil && len(s.Opaque) > 0 {
						t.Errorf("%s.%s is opaque to the build: %+v", p.Name, fd.Name.Name, s.Opaque)
					}
				}
			}
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
