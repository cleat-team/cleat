package engine

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

// cleat#4285, cleat#4295, cleat#4330: no host call reaches LIVE WORK by a path that skips the suspension refusal.
//
// every_host_call_refuses_after_a_suspension_or_is_named_test.go asks whether a host call REACHES the refusal: in its own body, or in a function it calls
// directly. That is an EXISTENTIAL question, and it was answered "yes" for PluginCall for as long as the call had one gated path. PluginCall had two: the usual
// one reached freshPluginCall, which refused; a replayed record whose function may be re-invoked went replayPluginCall -> freshPluginCallWithHistory ->
// freshPluginCallInternal, which ran the function live and refused nothing. The check was satisfied by the first path and never looked at the second. What the claim
// "every host call refuses" needs is the UNIVERSAL form: every path from a host call to live work passes a refusal.
//
// This is that form. From each HostHandler method it follows the engine's own functions and reports a live-work function it can reach WITHOUT passing a gate.
// A GATE is a function that calls refuseIfSuspended or refuseIfSuspendedWith, or calls step (the dispatcher applies the refusal to every non-Pure spec's Fresh).
// LIVE WORK is a DENYLIST, which is this check's limit: callService, invokePlugin, and any function whose name starts with `fresh`. A way to reach the world that is
// not named so passes. (`invoke` as a prefix was tried and is wrong: invokeStepCallback is the debugger's replay callback and does nothing in the world.) Calls are followed by NAME (a method and a function of the same name are one node), which can only add paths, never hide one.

// liveWorkReachedWithoutAGate names, per host call, the live work it can reach around a gate that is KNOWN and not yet closed, with the reason. An entry that
// describes nothing (the path is gone, or is gated) fails, so it cannot outlive its reason.
var liveWorkReachedWithoutAGate = map[string]string{}

func gateOrLiveIn(name string) (live bool) {
	return name == "callService" || name == "invokePlugin" || strings.HasPrefix(name, "fresh")
}

// liveWorkAroundGates returns, per start method, each path (as "A -> B -> live") to a live-work function that does not pass a gate.
func liveWorkAroundGates(sources map[string]string, starts []string) (map[string][]string, error) {
	fset := token.NewFileSet()
	names := make([]string, 0, len(sources))
	for n := range sources {
		names = append(names, n)
	}
	sort.Strings(names)
	decls := map[string][]*ast.FuncDecl{}
	for _, n := range names {
		f, err := parser.ParseFile(fset, n, sources[n], 0)
		if err != nil {
			return nil, err
		}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
				decls[fd.Name.Name] = append(decls[fd.Name.Name], fd)
			}
		}
	}
	callee := func(call *ast.CallExpr) string {
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			return fn.Name
		case *ast.SelectorExpr:
			return fn.Sel.Name
		}
		return ""
	}
	callsOf := func(body ast.Node) []string {
		var out []string
		ast.Inspect(body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if c := callee(call); c != "" {
					out = append(out, c)
				}
			}
			return true
		})
		return out
	}
	isGate := func(fd *ast.FuncDecl) bool {
		for _, c := range callsOf(fd.Body) {
			if c == "refuseIfSuspended" || c == "refuseIfSuspendedWith" || c == "step" {
				return true
			}
		}
		return false
	}
	gated := func(name string) bool {
		for _, fd := range decls[name] {
			if isGate(fd) {
				return true
			}
		}
		return false
	}
	result := map[string][]string{}
	for _, start := range starts {
		var found []string
		seen := map[string]bool{}
		var visit func(name string, path []string)
		visit = func(name string, path []string) {
			if seen[name] {
				return
			}
			seen[name] = true
			for _, fd := range decls[name] {
				if isGate(fd) {
					return // everything below a gate is refused first
				}
			}
			for _, c := range callsOf(bodyOf(decls[name])) {
				if _, declared := decls[c]; declared {
					if gated(c) {
						continue
					}
					if gateOrLiveIn(c) {
						found = append(found, strings.Join(append(append([]string{}, path...), name, c), " -> "))
						continue
					}
					visit(c, append(append([]string{}, path...), name))
				} else if gateOrLiveIn(c) {
					found = append(found, strings.Join(append(append([]string{}, path...), name, c), " -> "))
				}
			}
		}
		visit(start, nil)
		sort.Strings(found)
		found = slices.Compact(found)
		if len(found) > 0 {
			result[start] = found
		}
	}
	return result, nil
}

// bodyOf joins the bodies of every declaration of one name, so a name shared by two receivers is followed through both.
func bodyOf(fds []*ast.FuncDecl) ast.Node {
	list := &ast.BlockStmt{}
	for _, fd := range fds {
		list.List = append(list.List, fd.Body.List...)
	}
	return list
}

func TestNoHostCallReachesLiveWorkAroundTheSuspensionRefusal(t *testing.T) {
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
	ht := reflect.TypeOf((*HostHandler)(nil)).Elem()
	var methods []string
	for i := 0; i < ht.NumMethod(); i++ {
		methods = append(methods, ht.Method(i).Name)
	}
	if len(methods) < 40 {
		t.Fatalf("PRECONDITION FAILED: read %d HostHandler methods; the scan measured nothing", len(methods))
	}
	got, err := liveWorkAroundGates(sources, methods)
	if err != nil {
		t.Fatalf("UNMEASURED: the engine source does not parse: %v", err)
	}
	for m, paths := range got {
		if _, named := liveWorkReachedWithoutAGate[m]; named {
			continue
		}
		t.Errorf("host call %s reaches live work without passing a suspension or stop refusal:\n  %s\n  A path that skips the gate runs live work in a stopping or suspended session (cleat#4285, cleat#4330). Route it through step() or call refuseIfSuspended on it.", m, strings.Join(paths, "\n  "))
	}
	for m, why := range liveWorkReachedWithoutAGate {
		if _, still := got[m]; !still {
			t.Errorf("liveWorkReachedWithoutAGate names %s (%s) but it reaches no ungated live work: delete the entry", m, why)
		}
	}
}

// KNOWN POSITIVE, in the EXACT shape the old PluginCall had, and the NEGATIVE CONTROL for it: the same call with its re-invocation path gated. Without the
// positive the test above could pass because it sees nothing, which is how the existential check passed.
func TestTheLiveWorkScanSeesAnUngatedSecondPathAndPassesAGatedOne(t *testing.T) {
	ungated := `package engine
func (s *execSession) PluginCall() int64 {
	if s.isReplay {
		return s.replayPluginCall()
	}
	return s.freshPluginCall()
}
func (s *execSession) replayPluginCall() int64 { return s.freshPluginCallWithHistory() }
func (s *execSession) freshPluginCall() int64 {
	if res, ok := s.refuseIfSuspended("x", 1); ok {
		return res
	}
	return s.freshPluginCallInternal()
}
func (s *execSession) freshPluginCallWithHistory() int64 { return s.freshPluginCallInternal() }
func (s *execSession) freshPluginCallInternal() int64 { return s.invokePlugin() }
`
	gated := strings.Replace(ungated, "func (s *execSession) freshPluginCallWithHistory() int64 { return s.freshPluginCallInternal() }",
		"func (s *execSession) freshPluginCallWithHistory() int64 { s.refuseIfSuspended(\"x\", 1); return s.freshPluginCallInternal() }", 1)
	if gated == ungated {
		t.Fatalf("PRECONDITION FAILED: the gated control is the same text as the ungated source")
	}
	got, err := liveWorkAroundGates(map[string]string{"x.go": ungated}, []string{"PluginCall"})
	if err != nil {
		t.Fatalf("the synthetic source did not parse: %v", err)
	}
	if len(got["PluginCall"]) == 0 || !strings.Contains(strings.Join(got["PluginCall"], "|"), "freshPluginCallWithHistory") {
		t.Errorf("the scan did not report the ungated re-invocation path of the old PluginCall shape: %v", got)
	}
	got, err = liveWorkAroundGates(map[string]string{"x.go": gated}, []string{"PluginCall"})
	if err != nil {
		t.Fatalf("the synthetic source did not parse: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("the scan reported a path in the gated control: %v", fmt.Sprint(got))
	}
}
