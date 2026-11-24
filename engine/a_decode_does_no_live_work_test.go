package engine

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// cleat#4152, G3: a step's Decode (and DecodeRun) turns a consumed record into the call's result, and DOES NOT TOUCH THE WORLD.
//
// That sentence has been on stepSpec.Decode since the dispatcher began, and plugin_call broke it from the start: its replay half re-invoked a function live from
// the place a recorded answer is served. Decode runs no stop refusal and no suspension refusal (those belong to the Fresh path), so live work there runs in a
// stopping, suspended or defer-segment session, which the dispatcher exists to prevent. It was reachable for blobstore.blobGet and llm.embed and any plugin
// declaring both replay flags (cleat#4330). Reinvoke is now how a spec says "run the call again": the step goes through the refusals and Fresh.
//
// This test is the structural half. It reads the engine's source and, for every stepSpec literal, walks the spec's Decode and DecodeRun and what they call, to
// depth 3 through the package's own functions, and fails on a callee that runs live work. WHAT "LIVE" MEANS IS A DENYLIST, and that is the limit of this check: a
// name that is not on it passes. It is: callService, invokePlugin, any function whose name starts with `fresh`, and the Call and CallContext methods of a caller.
// A new way to reach the world belongs on the list, and the known positive below fails if the list stops seeing the shapes it is meant to.

func decodeLiveCallee(name string) bool {
	return name == "callService" || name == "invokePlugin" || name == "Call" || name == "CallContext" || strings.HasPrefix(name, "fresh")
}

// decodeLiveWork returns, per spec, the live callees reachable from its Decode or DecodeRun, and how many specs had a Decode to read.
func decodeLiveWork(sources map[string]string) (findings []string, specsWithDecode int, err error) {
	fset := token.NewFileSet()
	var files []*ast.File
	names := make([]string, 0, len(sources))
	for n := range sources {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		f, perr := parser.ParseFile(fset, n, sources[n], 0)
		if perr != nil {
			return nil, 0, perr
		}
		files = append(files, f)
	}
	decls := map[string][]*ast.FuncDecl{}
	for _, f := range files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
				decls[fd.Name.Name] = append(decls[fd.Name.Name], fd)
			}
		}
	}
	calleeName := func(call *ast.CallExpr) string {
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			return fn.Name
		case *ast.SelectorExpr:
			return fn.Sel.Name
		}
		return ""
	}
	// walk returns the live callees reachable from body, following package functions to the given depth.
	var walk func(body ast.Node, depth int, seen map[string]bool) []string
	walk = func(body ast.Node, depth int, seen map[string]bool) []string {
		var out []string
		ast.Inspect(body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := calleeName(call)
			if name == "" {
				return true
			}
			if decodeLiveCallee(name) {
				out = append(out, name)
				return true
			}
			if depth > 0 && !seen[name] {
				seen[name] = true
				for _, fd := range decls[name] {
					for _, inner := range walk(fd.Body, depth-1, seen) {
						out = append(out, name+" -> "+inner)
					}
				}
			}
			return true
		})
		return out
	}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			cl, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if id, ok := cl.Type.(*ast.Ident); !ok || id.Name != "stepSpec" {
				return true
			}
			specName := "?"
			var decodes []ast.Expr
			for _, el := range cl.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, _ := kv.Key.(*ast.Ident)
				if key == nil {
					continue
				}
				switch key.Name {
				case "Name":
					if lit, ok := kv.Value.(*ast.BasicLit); ok {
						specName = strings.Trim(lit.Value, `"`)
					}
				case "Decode", "DecodeRun":
					decodes = append(decodes, kv.Value)
				}
			}
			for _, d := range decodes {
				specsWithDecode++
				var live []string
				switch v := d.(type) {
				case *ast.FuncLit:
					live = walk(v.Body, 3, map[string]bool{})
				case *ast.Ident:
					for _, fd := range decls[v.Name] {
						live = append(live, walk(fd.Body, 3, map[string]bool{})...)
					}
				case *ast.SelectorExpr:
					for _, fd := range decls[v.Sel.Name] {
						live = append(live, walk(fd.Body, 3, map[string]bool{})...)
					}
				}
				for _, l := range live {
					findings = append(findings, fmt.Sprintf("spec %q: Decode reaches live work: %s", specName, l))
				}
			}
			return true
		})
	}
	sort.Strings(findings)
	return findings, specsWithDecode, nil
}

func TestNoStepDecodeReachesLiveWork(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("UNMEASURED: cannot read the engine directory: %v", err)
	}
	sources := map[string]string{}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, err := os.ReadFile(n)
		if err != nil {
			t.Fatalf("UNMEASURED: cannot read %s: %v", n, err)
		}
		sources[n] = string(b)
	}
	findings, specs, err := decodeLiveWork(sources)
	if err != nil {
		t.Fatalf("UNMEASURED: the engine source does not parse: %v", err)
	}
	// Vacuity, per thing and not per total: a scan that reads no Decode passes whatever the tree says.
	if specs < 15 {
		t.Fatalf("PRECONDITION FAILED: the scan found %d specs with a Decode in %d files; it is not reading the registry's specs", specs, len(sources))
	}
	for _, f := range findings {
		t.Errorf("%s\n  A Decode runs no stop or suspension refusal, so live work there runs in a stopping or suspended session. Express a call that runs again on replay as the spec's Reinvoke (cleat#4330), which goes through the refusals and Fresh.", f)
	}
}

// KNOWN POSITIVE and NEGATIVE CONTROL for the scan above, on synthetic source: a Decode that reaches live work directly, one that reaches it through a helper two
// calls down (the old plugin_call's shape), and a clean one. Without the positives the test above could pass because it sees nothing.
func TestTheDecodeScanSeesLiveWorkDirectlyAndThroughHelpersAndPassesACleanDecode(t *testing.T) {
	src := `package engine
var directSpec = stepSpec{Name: "direct", Decode: func(c *stepCtx, rec *EventRecord) int64 { return c.s.invokePlugin(1) }}
var helperSpec = stepSpec{Name: "helper", Decode: func(c *stepCtx, rec *EventRecord) int64 { return c.s.answer(c, rec) }}
func (s *execSession) answer(c *stepCtx, rec *EventRecord) int64 { return s.again(c) }
func (s *execSession) again(c *stepCtx) int64 { return s.callService(1) }
var namedSpec = stepSpec{Name: "named", DecodeRun: decodeNamed}
func decodeNamed(c *stepCtx, run []EventRecord) int64 { return c.s.freshThing(c) }
var cleanSpec = stepSpec{Name: "clean", Decode: func(c *stepCtx, rec *EventRecord) int64 { return c.s.writeOut(rec.Response) }}
`
	findings, specs, err := decodeLiveWork(map[string]string{"x.go": src})
	if err != nil {
		t.Fatalf("the synthetic source did not parse: %v", err)
	}
	if specs != 4 {
		t.Fatalf("PRECONDITION FAILED: read %d specs with a Decode, want 4", specs)
	}
	want := map[string]bool{"direct": false, "helper": false, "named": false}
	for _, f := range findings {
		for n := range want {
			if strings.Contains(f, fmt.Sprintf("spec %q:", n)) {
				want[n] = true
			}
		}
		if strings.Contains(f, `spec "clean"`) {
			t.Errorf("a clean Decode was reported: %s", f)
		}
	}
	for n, saw := range want {
		if !saw {
			t.Errorf("the scan did not see live work in the %s spec: %q", n, findings)
		}
	}
}
