package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestEveryWitCallOutcomeHasADispatcherThatBuildsOne is the mechanism, and the
// reason this change is not eight edits.
//
// python-sdk/wit/cleat.wit says which host calls return
// `result<string, call-failure>` and which return a bare `string`. The
// component dispatchers in component_callbacks.go have to agree, and nothing
// makes them: they build a `wasmtime_component_val_t` by hand, so a dispatcher
// that writes a STRING for a function the WIT types as a RESULT is a Go
// program that compiles, links, and produces a value of the wrong shape at the
// canonical ABI boundary -- which is the class CLAUDE.md names and the class
// this whole change is about.
//
// The direction that matters is not the obvious one. Adding a function to the
// WIT with a `result` return and forgetting the dispatcher fails loudly at
// runtime. Going the other way -- widening a dispatcher to
// setResultCallOutcome while its WIT function still says `string` -- is the
// same defect and just as easy to write, so both are asserted.
func TestEveryWitCallOutcomeHasADispatcherThatBuildsOne(t *testing.T) {
	witPath := filepath.Join("..", "python-sdk", "wit", "cleat.wit")
	src, err := os.ReadFile(witPath)
	if err != nil {
		t.Fatalf("read %s: %v", witPath, err)
	}

	outcome, plain := witFunctionReturns(t, string(src))

	// Vacuity guards. Every one of these has been the shape of a test that
	// checked nothing in this repo: a parser that quietly matched zero
	// functions reports perfect agreement.
	if len(outcome) < 8 {
		t.Fatalf("parsed only %d WIT functions returning result<string, call-failure>: %v.\n\n"+
			"There are eight -- the three durable-call variants, two child-workflow "+
			"starts, two plugin calls and fetch. Fewer means the parser stopped "+
			"matching and every assertion below is vacuous.", len(outcome), sortedWitNames(outcome))
	}
	if !outcome["durable-call"] || !outcome["plugin-call"] {
		t.Fatalf("durable-call and plugin-call are the anchors and both must parse "+
			"as returning the outcome type; got %v", sortedWitNames(outcome))
	}
	if !plain["durable-poll-signal"] || !plain["durable-defer"] {
		t.Fatalf("durable-poll-signal and durable-defer are the anchors for the "+
			"plain-string side; got %v", sortedWitNames(plain))
	}

	witToCB := parseWitTypeMap(t)
	cbToDispatch := parseCBTypeSwitch(t)
	dispatchBuilds := parseDispatcherSetters(t)

	if len(witToCB) < 40 || len(cbToDispatch) < 40 || len(dispatchBuilds) < 40 {
		t.Fatalf("parsed witTypeMap=%d cbType switch=%d dispatchers=%d; all three are "+
			"~50 entries, so a small number means a parser broke",
			len(witToCB), len(cbToDispatch), len(dispatchBuilds))
	}

	check := func(fn string, wantOutcome bool) {
		cb, ok := witToCB[fn]
		if !ok {
			// Not every WIT function has a typed dispatcher; the untyped ones
			// fall through to cbTypeDefault and cannot be checked here.
			return
		}
		dispatch, ok := cbToDispatch[cb]
		if !ok {
			t.Errorf("%s maps to %s, which the cbType switch in component_cgo.go does not handle",
				fn, cb)
			return
		}
		builds, ok := dispatchBuilds[dispatch]
		if !ok {
			t.Errorf("%s dispatches to %s, which component_callbacks.go does not define", fn, dispatch)
			return
		}
		switch {
		case wantOutcome && !builds:
			t.Errorf("cleat.wit types %s as returning result<string, call-failure>, but "+
				"%s writes a bare string.\n\n"+
				"The guest lifts the result according to the WIT, so this is a value of "+
				"the wrong shape at the canonical ABI boundary. Use "+
				"setResultCallOutcome(results, nresults, decodeCallOutcome(packed, buf, ...)).",
				fn, dispatch)
		case !wantOutcome && builds:
			t.Errorf("%s builds a result<string, call-failure>, but cleat.wit types %s as "+
				"returning a bare string.\n\n"+
				"Widening the dispatcher without widening the WIT is the same defect from "+
				"the other side. Change the WIT in the same commit, and remember that a "+
				"host call only needs the outcome type if stopBeforeNewWork can refuse it "+
				"or it can fail.", dispatch, fn)
		}
	}

	for fn := range outcome {
		check(fn, true)
	}
	for fn := range plain {
		check(fn, false)
	}
}

// witFunctionReturns splits the WIT's interface functions by return type.
//
// World exports are excluded: `run` returns run-outcome, which is a different
// type answering a different question (what the GUEST did), and folding the two
// together would let a change to one satisfy the assertion about the other.
func witFunctionReturns(t *testing.T, wit string) (outcome, plain map[string]bool) {
	t.Helper()

	// Strip doc comments first. They contain the words "result<string,
	// call-failure>" in prose -- this file's own explanation of the type is in
	// there -- and a parser that reads them finds functions that do not exist.
	var body strings.Builder
	for _, line := range strings.Split(wit, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		body.WriteString(line)
		body.WriteString("\n")
	}

	fnRe := regexp.MustCompile(`(?s)(^|\n)\s*([a-z][a-z0-9-]*)\s*:\s*func\s*\(([^()]*)\)\s*(->\s*([^;]+?))?\s*;`)
	outcome = map[string]bool{}
	plain = map[string]bool{}
	for _, m := range fnRe.FindAllStringSubmatch(body.String(), -1) {
		name, ret := m[2], strings.TrimSpace(m[5])
		switch {
		case strings.HasPrefix(ret, "result<string, call-failure>"):
			outcome[name] = true
		case ret == "string":
			plain[name] = true
		}
	}
	return outcome, plain
}

// parseWitTypeMap reads witTypeMap out of component_callbacks.go: WIT function
// name -> cbType identifier.
func parseWitTypeMap(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	file := parseEngineFile(t, "component_callbacks.go")
	ast.Inspect(file, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "witTypeMap" || len(vs.Values) != 1 {
			return true
		}
		outer, ok := vs.Values[0].(*ast.CompositeLit)
		if !ok {
			return true
		}
		for _, moduleKV := range outer.Elts {
			kv, ok := moduleKV.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			inner, ok := kv.Value.(*ast.CompositeLit)
			if !ok {
				continue
			}
			for _, fnKV := range inner.Elts {
				fkv, ok := fnKV.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := fkv.Key.(*ast.BasicLit)
				if !ok || key.Kind != token.STRING {
					continue
				}
				ident, ok := fkv.Value.(*ast.Ident)
				if !ok {
					continue
				}
				out[strings.Trim(key.Value, `"`)] = ident.Name
			}
		}
		return false
	})
	return out
}

// parseCBTypeSwitch reads goComponentCallback's switch: cbType -> dispatch
// method name.
func parseCBTypeSwitch(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	file := parseEngineFile(t, "component_cgo.go")
	ast.Inspect(file, func(n ast.Node) bool {
		cc, ok := n.(*ast.CaseClause)
		if !ok || len(cc.List) != 1 || len(cc.Body) != 1 {
			return true
		}
		caseIdent, ok := cc.List[0].(*ast.Ident)
		if !ok || !strings.HasPrefix(caseIdent.Name, "cbType") {
			return true
		}
		ret, ok := cc.Body[0].(*ast.ReturnStmt)
		if !ok || len(ret.Results) != 1 {
			return true
		}
		call, ok := ret.Results[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		out[caseIdent.Name] = sel.Sel.Name
		return true
	})
	return out
}

// parseDispatcherSetters reports, per dispatch method, whether its body builds
// a result<string, call-failure> rather than writing a bare string.
func parseDispatcherSetters(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	file := parseEngineFile(t, "component_callbacks.go")
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Recv == nil || !strings.HasPrefix(fd.Name.Name, "dispatch") {
			continue
		}
		builds := false
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "setResultCallOutcome" {
				builds = true
			}
			return true
		})
		out[fd.Name.Name] = builds
	}
	return out
}

func parseEngineFile(t *testing.T, name string) *ast.File {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return file
}

func sortedWitNames(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
