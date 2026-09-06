package wasm

import (
	"fmt"
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

// TestEveryCompositeHostCallHasAnImportRow is the guard for #775.
//
// AnalyzeUsage scans the user's AST for h.<Method>(...) and looks each name up
// in hostFunctions. It does NOT follow into the SDK. So a HostCalls method
// implemented in terms of ANOTHER host call needs its own row, or the inner
// import is never generated, the adapter field stays nil, and
// HostCallsImpl's nil branch returns a zero value -- silently, in a compiled
// workflow, with no build or deploy error.
//
// Ten methods were in that state when this test was written: NewUUID returned
// a constant zero UUID in every workflow, and a workflow whose whole body was
// h.Log(...) plus h.Call(...) compiled with no host calls wired at all.
//
// This walks the SDK for methods that call another h.X(...) and fails if the
// wrapper has no row covering the inner method's import. It is deliberately
// source-derived rather than a hand-maintained list: a hand-maintained list is
// what hostFunctions already is, and it is what went stale.
func TestEveryCompositeHostCallHasAnImportRow(t *testing.T) {
	sdk := sdkDirForTest(t)

	// import name -> set of methods that produce it
	importsFor := map[string][]string{}
	methodImports := map[string]map[string]bool{}
	for _, hf := range hostFunctions {
		importsFor[hf.FieldName] = append(importsFor[hf.FieldName], hf.ImportName)
		if methodImports[hf.FieldName] == nil {
			methodImports[hf.FieldName] = map[string]bool{}
		}
		methodImports[hf.FieldName][hf.ImportName] = true
	}

	inner := regexp.MustCompile(`\bh\.([A-Z]\w*)\(`)
	fset := token.NewFileSet()

	entries, err := os.ReadDir(sdk)
	if err != nil {
		t.Skipf("SDK sources not available at %s: %v", sdk, err)
	}

	var problems []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(sdk, e.Name())
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			continue // not our business to police SDK syntax
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || fd.Body == nil {
				continue
			}
			if !receiverIsHostCallsImpl(fd) {
				continue
			}
			outer := fd.Name.Name
			body := string(src[fset.Position(fd.Body.Pos()).Offset:fset.Position(fd.Body.End()).Offset])

			for _, m := range inner.FindAllStringSubmatch(body, -1) {
				callee := m[1]
				if callee == outer {
					continue // recursion, not delegation
				}
				needed, isHostCall := methodImports[callee]
				if !isHostCall {
					continue // callee is not itself a mapped host call
				}
				have := methodImports[outer]
				for imp := range needed {
					if imp == "" {
						continue // tracked-but-no-import, e.g. RunDetached
					}
					if !have[imp] {
						problems = append(problems, fmt.Sprintf(
							"h.%s calls h.%s which needs %s, but hostFunctions has no {%q, %q} row",
							outer, callee, imp, imp, outer))
					}
				}
			}
		}
	}

	sort.Strings(problems)
	if len(problems) > 0 {
		t.Errorf("%d composite HostCalls method(s) would compile with the inner "+
			"import unwired, returning a zero value at run time:\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
}

func receiverIsHostCallsImpl(fd *ast.FuncDecl) bool {
	if len(fd.Recv.List) != 1 {
		return false
	}
	star, ok := fd.Recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	id, ok := star.X.(*ast.Ident)
	return ok && id.Name == "HostCallsImpl"
}

func sdkDirForTest(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Join(filepath.Dir(wd), "cleat")
}
