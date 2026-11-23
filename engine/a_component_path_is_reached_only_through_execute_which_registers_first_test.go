package engine

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// ExecuteComponentCGo does not register the fence restart; Execute registers it before it dispatches there. That is only true of a held run for as long as nothing else
// calls ExecuteComponentCGo and Execute keeps registering first, and neither is visible to a test that runs a component, because a component run through any other door
// still completes. So the exception named in fenceBudgetBuildersRegisteredByTheirCaller is checked here, by reading the source, and each way it can become false is a
// known-positive below.

// callsIn returns, for each function in src, the byte offsets of its calls to a method or function called name.
func callsIn(t *testing.T, file, src, name string) map[string][]int {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	out := map[string][]int{}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch x := call.Fun.(type) {
			case *ast.SelectorExpr:
				if x.Sel.Name == name {
					out[fn.Name.Name] = append(out[fn.Name.Name], fset.Position(call.Pos()).Offset)
				}
			case *ast.Ident:
				if x.Name == name {
					out[fn.Name.Name] = append(out[fn.Name.Name], fset.Position(call.Pos()).Offset)
				}
			}
			return true
		})
	}
	return out
}

// componentPathProblems reports every way the claim "ExecuteComponentCGo is reached only through Execute, which registers the restart first" is false in srcs (file name to
// source). Source order stands for execution order, which is exact for the straight-line prefix of Execute that registers and then dispatches and would not be for a
// registration inside a branch; the nightly timing row measures the behaviour itself.
func componentPathProblems(t *testing.T, srcs map[string]string) []string {
	t.Helper()
	var problems []string
	sawExecuteCall := false
	names := make([]string, 0, len(srcs))
	for n := range srcs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, file := range names {
		src := srcs[file]
		calls := callsIn(t, file, src, "ExecuteComponentCGo")
		for fn, offs := range calls {
			if fn != "Execute" {
				problems = append(problems, fmt.Sprintf("%s calls ExecuteComponentCGo from %s (%d call(s)): that path does not register the fence restart, so a held component run reached through it would be killed for the work of both segments", file, fn, len(offs)))
			}
		}
		if offs, ok := calls["Execute"]; ok {
			sawExecuteCall = true
			sort.Ints(offs)
			regs := callsIn(t, file, src, "onLimitsRestart")["Execute"]
			sort.Ints(regs)
			switch {
			case len(regs) == 0:
				problems = append(problems, file+": Execute calls ExecuteComponentCGo and never registers the fence restart")
			case regs[0] > offs[0]:
				problems = append(problems, file+": Execute registers the fence restart AFTER it dispatches to ExecuteComponentCGo, so the component run is already underway")
			}
		}
	}
	if !sawExecuteCall {
		problems = append(problems, "no call to ExecuteComponentCGo from Execute was found: the scan is reading the wrong files, or the component path is no longer reached through Execute")
	}
	return problems
}

func TestTheComponentPathIsOnlyReachedThroughExecuteWhichRegistersFirst(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	srcs := map[string]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		srcs[f] = string(raw)
	}
	if len(srcs) < 20 {
		t.Fatalf("PRECONDITION FAILED: read only %d non-test files, so a clean result would measure nothing", len(srcs))
	}
	for _, p := range componentPathProblems(t, srcs) {
		t.Error(p)
	}
}

// KNOWN-POSITIVES: each way the claim can become false must be reported, and the good shape must not be.
func TestTheComponentPathScanReportsEveryWayTheClaimCanBecomeFalse(t *testing.T) {
	const good = "package engine\nfunc (b *B) Execute() { onLimitsRestart(nil, nil); b.ExecuteComponentCGo() }\n"
	for _, tc := range []struct {
		name string
		srcs map[string]string
		want string // "" means no problem expected; otherwise a substring of exactly one problem
	}{
		{"the good shape", map[string]string{"a.go": good}, ""},
		{"a second caller", map[string]string{"a.go": good, "b.go": "package engine\nfunc (b *B) other() { b.ExecuteComponentCGo() }\n"}, "calls ExecuteComponentCGo from other"},
		{"registration after the dispatch", map[string]string{"a.go": "package engine\nfunc (b *B) Execute() { b.ExecuteComponentCGo(); onLimitsRestart(nil, nil) }\n"}, "AFTER it dispatches"},
		{"no registration at all", map[string]string{"a.go": "package engine\nfunc (b *B) Execute() { b.ExecuteComponentCGo() }\n"}, "never registers"},
		{"no caller (the scan reads the wrong thing)", map[string]string{"a.go": "package engine\nfunc (b *B) Execute() { onLimitsRestart(nil, nil) }\n"}, "no call to ExecuteComponentCGo from Execute"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := componentPathProblems(t, tc.srcs)
			switch {
			case tc.want == "" && len(got) != 0:
				t.Fatalf("the good shape was reported: %v", got)
			case tc.want != "" && (len(got) != 1 || !strings.Contains(got[0], tc.want)):
				t.Fatalf("want exactly one problem containing %q, got %v", tc.want, got)
			}
		})
	}
}
