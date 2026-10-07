package wasm

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/internal/analyzer"
)

// TestEverySDKHelperHasItsImports is TestEveryCompositeHostCallHasAnImportRow
// for the population that one cannot see.
//
// That test walks the SDK and skips every function whose receiver is not
// *HostCallsImpl (receiverIsHostCallsImpl). The skip is not an oversight in its
// own terms -- it was written for #775, where a HostCalls method delegated to
// another HostCalls method -- but it makes the check silent about a type that
// reproduces the identical failure from one level up:
//
//	AnalyzeUsage scans WORKFLOW code for h.<Method>(...). An SDK type that
//	holds or receives a HostCalls and calls it INSIDE a method of its own
//	contributes no import, the adapter field stays nil, HostCallsImpl's nil
//	branch logs and returns a zero value, and the workflow builds, deploys and
//	runs -- wrongly, with no error anywhere.
//
// Saga.AddStepCall was the first (cleat#1131) and got sdkHelperImports. It was
// found while the feature was being written. cleat.Selector was the second and
// nobody found it, because no check had it in scope: a Selector timer fired
// instantly in every compiled workflow whose own source did not happen to
// mention h.DurableSleep, for as long as the type had existed (cleat#1404).
//
// So this walks the same tree with the receiver filter INVERTED and asserts the
// sdkHelperImports row covers what the method actually calls.
//
// Both routes to a HostCalls are accepted, and that is load-bearing rather than
// thorough. Selector STORES one in a field; Saga.Run RECEIVES one as a
// parameter. A first version of this scan matched only the field route and
// reported Saga as clean; a version matching only a bare `h` identifier
// reported Selector as clean. Each looked complete on its own output, which is
// how the population stayed hidden -- so the test accepts both and the fixture
// below pins both.
func TestEverySDKHelperHasItsImports(t *testing.T) {
	sdk := sdkDirForTest(t)

	needs := map[string]map[string]bool{}
	addNeed := func(method, imp string) {
		if needs[method] == nil {
			needs[method] = map[string]bool{}
		}
		needs[method][imp] = true
	}
	for _, hf := range hostFunctions {
		addNeed(hf.FieldName, hf.ImportName)
	}
	for method, imports := range compositeRequires {
		for _, imp := range imports {
			addNeed(method, imp)
		}
	}

	var problems []string
	for _, m := range scanSDKHelperMethods(t, sdk) {
		have := map[string]bool{}
		for _, imp := range sdkHelperImports[m.key] {
			have[imp] = true
		}
		for _, callee := range m.calls {
			for imp := range needs[callee] {
				if imp == "" || have[imp] {
					continue
				}
				problems = append(problems, fmt.Sprintf(
					"%s calls h.%s (via %s) which needs %s, but sdkHelperImports[%q] does not provide it",
					m.key, callee, m.route, imp, m.key))
			}
		}
	}

	sort.Strings(problems)
	if len(problems) > 0 {
		t.Errorf("%d SDK helper method(s) make a host call the workflow never wrote, "+
			"with the import unwired -- the workflow would build, deploy and get a zero "+
			"value at run time:\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
}

// TestTheSDKHelperScanSeesBothRoutesToAHostCalls is the positive control for
// the scan above.
//
// Without it, a scan that silently stopped matching -- a renamed field, a
// receiver spelled differently, an AST shape not handled -- would report zero
// problems, which is exactly what a passing run looks like. The two entries
// here are the two routes, taken from the two types that actually use them, so
// the control fails for the same reason the real check would.
func TestTheSDKHelperScanSeesBothRoutesToAHostCalls(t *testing.T) {
	found := map[string][]string{}
	routes := map[string]string{}
	for _, m := range scanSDKHelperMethods(t, sdkDirForTest(t)) {
		found[m.key] = m.calls
		routes[m.key] = m.route
	}

	for _, want := range []struct {
		key   string
		call  string
		route string
	}{
		{"Selector.Select", "DurableSleep", "field"},
		// Saga.RunWithResult, not Saga.Run: cleat#2627 moved the body there and
		// left Run delegating, so Run makes no host call of its own and the scan
		// no longer returns it. The ROUTE is what this control pins, and
		// RunWithResult receives its HostCalls as a parameter exactly as Run
		// did -- but the name has to follow the body, or the control stops
		// exercising the parameter route while still passing.
		{"Saga.RunWithResult", "LogKV", "parameter"},
	} {
		calls, ok := found[want.key]
		if !ok {
			t.Errorf("the scan did not find %s at all; it can no longer see the %s route, "+
				"so TestEverySDKHelperHasItsImports is passing vacuously for it",
				want.key, want.route)
			continue
		}
		if routes[want.key] != want.route {
			t.Errorf("%s was found via the %q route, expected %q",
				want.key, routes[want.key], want.route)
		}
		var seen bool
		for _, c := range calls {
			if c == want.call {
				seen = true
			}
		}
		if !seen {
			t.Errorf("the scan found %s but not its h.%s call; it sees the method and not "+
				"the call inside it (found %v)", want.key, want.call, calls)
		}
	}
}

type sdkHelperMethod struct {
	key   string // "Type.Method", the sdkHelperImports key
	calls []string
	route string // "field", "parameter", or "delegation" for a body that only calls another helper

	// delegates are SDK helper methods this body calls on its OWN receiver --
	// Run's body is `s.RunWithResult(h)` -- which is not a host call and so is
	// invisible to the h.<Method> scan below. They are resolved transitively;
	// see the closure in scanSDKHelperMethods. cleat#2627.
	delegates []string
}

// sdkHelperScanSkipDirs are SDK subdirectories this scan does not walk, with
// the reason, because an undocumented exclusion is the defect this whole file
// is about.
//
// cleattest, localdev and embedded run the HOST side: their HostCalls is one
// they constructed themselves with every field populated, so a missing import
// is not a thing that can happen there.
//
// dagrun is excluded for a different reason, and it is no longer a live gap.
// cleat/dagrun's DAG.ExecuteWithOptions calls h.AwaitAnyChild and
// DAG.startChild calls h.ChildWorkflowWithOptions, both through a HostCalls
// parameter -- the same shape as Saga.Run. A row for them STILL would not be
// consulted, because SDKDurableHelper refuses any receiver whose package is
// not named "cleat" and dagrun's package is named dagrun. That has not
// changed, so dagrun stays out of this scan: this table is not how dagrun
// declares its needs.
//
// What changed is that it now declares them somewhere that works. cleat#1617
// was the two routes disagreeing -- dagrun had written a //cleat:require
// directive, correct about what it needs, and collectRequirements read only
// the workflow's own package, so the directive sat in the one place that could
// not act on it. It now reads imported packages too, and
// TestARequiredHostCallIsReadFromAnImportedPackage is the guard for that
// route, with a testdata guest whose only host-call path is dagrun.
//
// So the two routes are deliberate and separate: this table for helpers in
// package "cleat", the directive for everything else, including a user's own
// helper package in their own module.
var sdkHelperScanSkipDirs = map[string]string{
	"cleattest": "host-side test harness; constructs its own fully-populated HostCalls",
	"localdev":  "host-side runner; same",
	"embedded":  "host-side runner; same",
	"dagrun":    "SDKDurableHelper refuses non-\"cleat\" packages, so a row here would never be read; dagrun declares its needs with //cleat:require instead -- cleat#1617",
}

func scanSDKHelperMethods(t *testing.T, sdk string) []sdkHelperMethod {
	t.Helper()
	fset := token.NewFileSet()

	var files []*ast.File
	err := filepath.WalkDir(sdk, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if reason, skip := sdkHelperScanSkipDirs[d.Name()]; skip && p != sdk {
				t.Logf("not walking %s: %s", d.Name(), reason)
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		f, perr := parser.ParseFile(fset, p, src, 0)
		if perr != nil {
			return nil // not this test's business to police SDK syntax
		}
		files = append(files, f)
		return nil
	})
	if err != nil {
		// Fatal, not Skip: cleat/ is in this repository. An unreadable tree
		// means the test has lost its subject, and passing would assert nothing.
		t.Fatalf("walking the SDK at %s: %v", sdk, err)
	}
	if len(files) == 0 {
		t.Fatalf("no SDK sources found under %s", sdk)
	}

	// Struct name -> field names whose type is HostCalls, whatever they are
	// called. Derived rather than assumed: Selector spells it `h` and dagrun's
	// TaskContext spells it `H`.
	hostFields := map[string]map[string]bool{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok || st.Fields == nil {
				return true
			}
			for _, fld := range st.Fields.List {
				if baseTypeName(fld.Type) != "HostCalls" {
					continue
				}
				for _, nm := range fld.Names {
					if hostFields[ts.Name.Name] == nil {
						hostFields[ts.Name.Name] = map[string]bool{}
					}
					hostFields[ts.Name.Name][nm.Name] = true
				}
			}
			return true
		})
	}

	out := map[string]*sdkHelperMethod{}
	record := func(key, call, route string) {
		m := out[key]
		if m == nil {
			m = &sdkHelperMethod{key: key, route: route}
			out[key] = m
		}
		for _, c := range m.calls {
			if c == call {
				return
			}
		}
		m.calls = append(m.calls, call)
	}
	recordDelegate := func(key, target string) {
		m := out[key]
		if m == nil {
			m = &sdkHelperMethod{key: key, route: "delegation"}
			out[key] = m
		}
		for _, d := range m.delegates {
			if d == target {
				return
			}
		}
		m.delegates = append(m.delegates, target)
	}

	for _, f := range files {
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || fd.Body == nil || len(fd.Recv.List) != 1 {
				continue
			}
			recvType := baseTypeName(fd.Recv.List[0].Type)
			// HostCallsImpl is the other test's entire denominator; this one is
			// everything else.
			if recvType == "" || recvType == "HostCallsImpl" {
				continue
			}
			var recvName string
			if len(fd.Recv.List[0].Names) == 1 {
				recvName = fd.Recv.List[0].Names[0].Name
			}
			params := map[string]bool{}
			if fd.Type.Params != nil {
				for _, p := range fd.Type.Params.List {
					if baseTypeName(p.Type) != "HostCalls" {
						continue
					}
					for _, nm := range p.Names {
						params[nm.Name] = true
					}
				}
			}
			fields := hostFields[recvType]
			if len(fields) == 0 && len(params) == 0 {
				continue
			}
			key := recvType + "." + fd.Name.Name
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || !startsUpper(sel.Sel.Name) {
					return true
				}
				switch x := sel.X.(type) {
				case *ast.Ident: // h.Method(...) where h is a HostCalls parameter
					switch {
					case params[x.Name]:
						record(key, sel.Sel.Name, "parameter")
					case recvName != "" && x.Name == recvName:
						// s.Method(...) on this method's OWN receiver: an
						// SDK-internal delegation, not a host call. Resolved
						// transitively below.
						recordDelegate(key, recvType+"."+sel.Sel.Name)
					}
				case *ast.SelectorExpr: // s.h.Method(...) where h is a HostCalls field
					if !fields[x.Sel.Name] {
						return true
					}
					if id, ok := x.X.(*ast.Ident); ok && (recvName == "" || id.Name == recvName) {
						record(key, sel.Sel.Name, "field")
					}
				}
				return true
			})
		}
	}

	// FOLLOW SDK-INTERNAL DELEGATION, because a helper's row must cover what it
	// eventually does, not only what it spells. Run's body is now
	// `s.RunWithResult(h)` -- a call on its own receiver -- so a scan reading
	// only direct h.<Method> calls finds no host call in Run at all and stops
	// requiring its row to cover anything. The row is still needed: a workflow
	// writing saga.Run(h) performs exactly what RunWithResult performs.
	//
	// This is the half of cleat#2627's defect that survived the other test:
	// dropping set_query_state from the Saga.Run row left wasm and analyzer
	// green while every compiled workflow calling saga.Run(h) -- three in this
	// tree, and every external user -- silently published nothing.
	var closure func(key string, onPath map[string]bool) map[string]bool
	closure = func(key string, onPath map[string]bool) map[string]bool {
		found := map[string]bool{}
		m := out[key]
		if m == nil || onPath[key] {
			return found
		}
		onPath[key] = true
		for _, c := range m.calls {
			found[c] = true
		}
		for _, d := range m.delegates {
			for c := range closure(d, onPath) {
				found[c] = true
			}
		}
		delete(onPath, key)
		return found
	}
	for key, m := range out {
		merged := map[string]bool{}
		for _, c := range m.calls {
			merged[c] = true
		}
		for c := range closure(key, map[string]bool{}) {
			merged[c] = true
		}
		m.calls = m.calls[:0]
		for c := range merged {
			m.calls = append(m.calls, c)
		}
	}

	var methods []sdkHelperMethod
	for _, m := range out {
		sort.Strings(m.calls)
		methods = append(methods, *m)
	}
	sort.Slice(methods, func(i, j int) bool { return methods[i].key < methods[j].key })
	return methods
}

func baseTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return baseTypeName(t.X)
	case *ast.SelectorExpr:
		return t.Sel.Name
	case *ast.Ident:
		return t.Name
	}
	return ""
}

func startsUpper(s string) bool { return s != "" && s[0] >= 'A' && s[0] <= 'Z' }

// TestEverySDKHelperRowIsAcceptedByTheGate pins the half of the binding between
// the two hand lists that nothing covered before.
//
// TestEverySDKHelperHasItsImports checks that a row COVERS what the helper
// calls. This checks that the row is REACHABLE: wasm consults sdkHelperImports
// only for a selection analyzer.SDKDurableHelper accepts, so a row whose key
// the gate refuses is never read, contributes no import, and lets the module
// build and return a zero value at run time. cleat#2627 had to update both
// lists in lockstep and only one of them was covered by a test.
//
// ONE DIRECTION ONLY, deliberately. The converse -- a key the gate accepts with
// no row -- is already covered, and by a stronger check: a helper the gate
// accepts is one that makes host calls (directly or, since cleat#2627, through
// delegation), so TestEverySDKHelperHasItsImports finds it and fails on the
// missing row. Asserting it again here would test the same thing twice and
// would need the accepted set enumerated, which is exactly the mutable-map
// export this avoids.
func TestEverySDKHelperRowIsAcceptedByTheGate(t *testing.T) {
	for key := range sdkHelperImports {
		if !analyzer.SDKDurableHelperName(key) {
			t.Errorf("sdkHelperImports has a row for %q, but analyzer.SDKDurableHelper "+
				"does not accept it -- the gate is consulted before the row is read, so "+
				"this row is dead and its imports are never added", key)
		}
	}
}
