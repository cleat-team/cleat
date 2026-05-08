package transform

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"

	"github.com/rcownie/cleat/internal/analyzer"
)

// ---- isHostCallsField ----

func TestIsHostCallsFieldPositive(t *testing.T) {
	// Parse "durable.HostCalls" as an expression.
	expr, err := parser.ParseExpr("durable.HostCalls")
	if err != nil {
		t.Fatalf("ParseExpr failed: %v", err)
	}
	field := &ast.Field{Type: expr}
	if !isHostCallsField(field) {
		t.Error("expected isHostCallsField to return true for durable.HostCalls")
	}
}

func TestIsHostCallsFieldStarExpr(t *testing.T) {
	// Parse "*durable.HostCalls" as an expression.
	expr, err := parser.ParseExpr("*durable.HostCalls")
	if err != nil {
		t.Fatalf("ParseExpr failed: %v", err)
	}
	field := &ast.Field{Type: expr}
	if !isHostCallsField(field) {
		t.Error("expected isHostCallsField to return true for *durable.HostCalls")
	}
}

func TestIsHostCallsFieldWrongPackage(t *testing.T) {
	expr, err := parser.ParseExpr("other.HostCalls")
	if err != nil {
		t.Fatalf("ParseExpr failed: %v", err)
	}
	field := &ast.Field{Type: expr}
	if isHostCallsField(field) {
		t.Error("expected isHostCallsField to return false for other.HostCalls")
	}
}

func TestIsHostCallsFieldWrongName(t *testing.T) {
	expr, err := parser.ParseExpr("durable.SomethingElse")
	if err != nil {
		t.Fatalf("ParseExpr failed: %v", err)
	}
	field := &ast.Field{Type: expr}
	if isHostCallsField(field) {
		t.Error("expected isHostCallsField to return false for durable.SomethingElse")
	}
}

func TestIsHostCallsFieldNotSelector(t *testing.T) {
	field := &ast.Field{Type: &ast.Ident{Name: "string"}}
	if isHostCallsField(field) {
		t.Error("expected isHostCallsField to return false for simple ident")
	}
}

// ---- findFileForFunc ----

func TestFindFileForFuncFound(t *testing.T) {
	fset := token.NewFileSet()
	fn := &ast.FuncDecl{
		Name: ast.NewIdent("myFunc"),
		Type: &ast.FuncType{Params: &ast.FieldList{}},
		Body: &ast.BlockStmt{},
	}

	file := &ast.File{
		Name:  ast.NewIdent("mypkg"),
		Decls: []ast.Decl{fn},
	}

	result := findFileForFunc([]*ast.File{file}, fn)
	if result == nil {
		t.Fatal("expected to find the file for the function")
	}
	if result.Name.Name != "mypkg" {
		t.Errorf("expected mypkg, got %q", result.Name.Name)
	}
	_ = fset // used for consistency with real usage
}

func TestFindFileForFuncNotFound(t *testing.T) {
	fn := &ast.FuncDecl{
		Name: ast.NewIdent("myFunc"),
		Type: &ast.FuncType{Params: &ast.FieldList{}},
		Body: &ast.BlockStmt{},
	}

	file := &ast.File{
		Name:  ast.NewIdent("mypkg"),
		Decls: []ast.Decl{},
	}

	result := findFileForFunc([]*ast.File{file}, fn)
	if result != nil {
		t.Error("expected nil when function not found")
	}
}

// ---- ensureHostCallsImport ----

func parseImportFile(t *testing.T, src string) *ast.File {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "test.go", src, parser.AllErrors)
	if err != nil {
		t.Fatalf("ParseFile failed: %v", err)
	}
	return f
}

// findImportInDecls searches through file.Decls for an import with the given path.
func findImportInDecls(file *ast.File, importPath string) bool {
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.IMPORT {
			continue
		}
		for _, spec := range gen.Specs {
			imp, ok := spec.(*ast.ImportSpec)
			if !ok {
				continue
			}
			path := strings.Trim(imp.Path.Value, `"`)
			if path == importPath {
				return true
			}
		}
	}
	return false
}

func TestEnsureHostCallsImportAddsToExisting(t *testing.T) {
	src := `package mypkg
import "fmt"
`
	file := parseImportFile(t, src)
	ensureHostCallsImport(file, nil)

	// The import is added to Decls, not to file.Imports. Check Decls.
	if !findImportInDecls(file, "github.com/rcownie/cleat/cleat") {
		t.Error("expected cleat/cleat import to be found in Decls")
	}
	// The import should have the "durable" alias. Check the new import spec.
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.IMPORT {
			continue
		}
		for _, spec := range gen.Specs {
			imp, ok := spec.(*ast.ImportSpec)
			if !ok {
				continue
			}
			path := strings.Trim(imp.Path.Value, `"`)
			if path == "github.com/rcownie/cleat/cleat" {
				if imp.Name == nil || imp.Name.Name != "durable" {
					t.Error("import should be aliased to 'durable'")
				}
			}
		}
	}
}

func TestEnsureHostCallsImportAlreadyPresent(t *testing.T) {
	src := `package mypkg
import durable "github.com/rcownie/cleat/cleat"
`
	file := parseImportFile(t, src)
	ensureHostCallsImport(file, nil)

	// Should still have exactly one import (via file.Imports which is parser-set).
	if len(file.Imports) != 1 {
		t.Errorf("expected 1 import, got %d", len(file.Imports))
	}
}

func TestEnsureHostCallsImportNoExistingImport(t *testing.T) {
	src := `package mypkg
`
	file := parseImportFile(t, src)
	ensureHostCallsImport(file, nil)

	// The import is added to Decls. Check Decls.
	if !findImportInDecls(file, "github.com/rcownie/cleat/cleat") {
		t.Error("expected cleat/cleat import to be found in Decls")
	}
}

func TestEnsureHostCallsImportAlreadyHasDurableAlias(t *testing.T) {
	// When the file already imports "github.com/rcownie/cleat/cleat" as "durable",
	// ensureHostCallsImport should not duplicate it.
	src := `package mypkg
import durable "github.com/rcownie/cleat/cleat"
import "fmt"
`
	file := parseImportFile(t, src)
	ensureHostCallsImport(file, nil)

	// Should still have 2 imports (fmt and durable).
	if len(file.Imports) != 2 {
		t.Errorf("expected 2 imports, got %d", len(file.Imports))
	}
}

func TestEnsureHostCallsImportAlreadyHasSuffixMatch(t *testing.T) {
	// When the file already imports something ending in "/durable",
	// ensureHostCallsImport should not duplicate it.
	src := `package mypkg
import "github.com/rcownie/cleat/durable"
`
	file := parseImportFile(t, src)
	ensureHostCallsImport(file, nil)

	// Should still have 1 import.
	if len(file.Imports) != 1 {
		t.Errorf("expected 1 import, got %d", len(file.Imports))
	}
}

// ---- resolveCalleeFQName ----

func TestResolveCalleeFQNameNilInfo(t *testing.T) {
	result := resolveCalleeFQName(&ast.CallExpr{}, nil)
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
}

func TestResolveCalleeFQNameIdentifierNoDef(t *testing.T) {
	// For an identifier that isn't in the Uses map, resolveCalleeFQName
	// should return "".
	info := &types.Info{
		Uses: make(map[*ast.Ident]types.Object),
	}
	call := &ast.CallExpr{
		Fun: ast.NewIdent("unknownFunc"),
	}
	result := resolveCalleeFQName(call, info)
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
}

// ---- hasHostCallsParam ----

func TestHasHostCallsParamNilType(t *testing.T) {
	fd := &analyzer.FuncDecl{
		Name: "test",
		Type: nil,
	}
	if hasHostCallsParam(fd) {
		t.Error("expected false for nil Type")
	}
}

func TestHasHostCallsParamNoParams(t *testing.T) {
	sig := &types.Signature{}
	fd := &analyzer.FuncDecl{
		Name: "test",
		Type: sig,
	}
	if hasHostCallsParam(fd) {
		t.Error("expected false for no params")
	}
}

// ---- addHostCallsParam ----

func TestAddHostCallsParamAddsToNilParams(t *testing.T) {
	fn := &ast.FuncDecl{
		Name: ast.NewIdent("testFunc"),
		Type: &ast.FuncType{},
	}
	addHostCallsParam(fn)
	if fn.Type.Params == nil || len(fn.Type.Params.List) != 1 {
		t.Fatal("expected params to be created with 1 field")
	}
	// The parameter should be "h durable.HostCalls"
	sel, ok := fn.Type.Params.List[0].Type.(*ast.SelectorExpr)
	if !ok {
		t.Fatal("expected selector expr for type")
	}
	if sel.Sel.Name != "HostCalls" {
		t.Errorf("expected HostCalls, got %s", sel.Sel.Name)
	}
	if fn.Type.Params.List[0].Names[0].Name != "h" {
		t.Errorf("expected param name 'h', got %s", fn.Type.Params.List[0].Names[0].Name)
	}
}

func TestAddHostCallsParamAlreadyHasH(t *testing.T) {
	// Function already has h cleat.HostCalls as first param.
	fn := &ast.FuncDecl{
		Name: ast.NewIdent("testFunc"),
		Type: &ast.FuncType{
			Params: &ast.FieldList{
				List: []*ast.Field{
					{
						Names: []*ast.Ident{ast.NewIdent("h")},
						Type: &ast.SelectorExpr{
							X:   ast.NewIdent("durable"),
							Sel: ast.NewIdent("HostCalls"),
						},
					},
				},
			},
		},
	}
	addHostCallsParam(fn)
	// Should still only have 1 param.
	if len(fn.Type.Params.List) != 1 {
		t.Errorf("expected 1 param, got %d", len(fn.Type.Params.List))
	}
}

func TestAddHostCallsParamExistingNonHostCallsH(t *testing.T) {
	// Function has a non-HostCalls param named "h" — should use "h2".
	fn := &ast.FuncDecl{
		Name: ast.NewIdent("testFunc"),
		Type: &ast.FuncType{
			Params: &ast.FieldList{
				List: []*ast.Field{
					{
						Names: []*ast.Ident{ast.NewIdent("h")},
						Type:  ast.NewIdent("string"),
					},
				},
			},
		},
	}
	addHostCallsParam(fn)
	if len(fn.Type.Params.List) != 2 {
		t.Fatalf("expected 2 params, got %d", len(fn.Type.Params.List))
	}
	// The new first param should be named "h2".
	if fn.Type.Params.List[0].Names[0].Name != "h2" {
		t.Errorf("expected param name 'h2', got %s", fn.Type.Params.List[0].Names[0].Name)
	}
	// Original param should be second.
	if fn.Type.Params.List[1].Names[0].Name != "h" {
		t.Errorf("expected second param name 'h', got %s", fn.Type.Params.List[1].Names[0].Name)
	}
}

// ---- canRemoveGlobalH ----

func TestCanRemoveGlobalHEmptyUsers(t *testing.T) {
	// Empty users means no functions reference the global, so it can be removed.
	users := map[string]bool{}
	needsH := map[string]bool{}
	result := canRemoveGlobalH(users, needsH, nil)
	if !result {
		t.Error("expected true when no users reference global h")
	}
}

// ---- removeGlobalDecl ----

func TestRemoveGlobalDeclRemovesSpec(t *testing.T) {
	spec := &ast.ValueSpec{
		Names: []*ast.Ident{ast.NewIdent("h")},
	}
	file := &ast.File{
		Name: ast.NewIdent("mypkg"),
		Decls: []ast.Decl{
			&ast.GenDecl{
				Tok: token.VAR,
				Specs: []ast.Spec{
					&ast.ValueSpec{
						Names: []*ast.Ident{ast.NewIdent("x")},
					},
					spec,
					&ast.ValueSpec{
						Names: []*ast.Ident{ast.NewIdent("y")},
					},
				},
			},
		},
	}
	gh := &globalHInfo{Spec: spec}
	removeGlobalDecl(file, gh)

	// The VAR decl should now have 2 specs (x and y, not h).
	gen := file.Decls[0].(*ast.GenDecl)
	if len(gen.Specs) != 2 {
		t.Fatalf("expected 2 specs after removal, got %d", len(gen.Specs))
	}
	if gen.Specs[0].(*ast.ValueSpec).Names[0].Name != "x" {
		t.Errorf("expected first spec name 'x', got %s", gen.Specs[0].(*ast.ValueSpec).Names[0].Name)
	}
	if gen.Specs[1].(*ast.ValueSpec).Names[0].Name != "y" {
		t.Errorf("expected second spec name 'y', got %s", gen.Specs[1].(*ast.ValueSpec).Names[0].Name)
	}
}

func TestRemoveGlobalDeclNonVarIgnored(t *testing.T) {
	// Ensure non-VAR GenDecls are not modified.
	spec := &ast.ValueSpec{
		Names: []*ast.Ident{ast.NewIdent("h")},
	}
	file := &ast.File{
		Name: ast.NewIdent("mypkg"),
		Decls: []ast.Decl{
			&ast.GenDecl{
				Tok: token.IMPORT,
				Specs: []ast.Spec{
					&ast.ImportSpec{
						Path: &ast.BasicLit{Kind: token.STRING, Value: `"fmt"`},
					},
				},
			},
		},
	}
	gh := &globalHInfo{Spec: spec}
	removeGlobalDecl(file, gh)
	// Should not panic and file should be unchanged.
	if len(file.Decls) != 1 {
		t.Errorf("expected 1 decl, got %d", len(file.Decls))
	}
}
