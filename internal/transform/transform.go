// Package transform implements AST source-to-source transformation that
// automatically threads cleat.HostCalls through the cleat closure.
//
// Developers declare a package-level var h *cleat.HostCalls (the "context
// object" pattern). Functions in the cleat closure that reference this
// global get h inserted as a first parameter. Call sites are updated to
// pass h through. The global is removed when no longer referenced.
//
// Entry points still declare h as a first parameter — the transform
// primarily helps with internal helper functions.
package transform

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/token"
	"go/types"
	"strings"

	"github.com/rcownie/cleat/internal/analyzer"
	"github.com/rcownie/cleat/internal/callgraph"
	"github.com/rcownie/cleat/internal/closure"
)

// Result holds the output of the transformation pass.
type Result struct {
	Files  map[string][]byte // filename → transformed Go source
	AddedH []string          // fully-qualified names of functions that got h added
}

// Config holds the inputs for the transformation.
type Config struct {
	Result    *analyzer.AnalysisResult
	CallGraph *callgraph.Graph
	Closure   *closure.Result
}

// Transform modifies ASTs to automatically thread cleat.HostCalls.
func Transform(cfg *Config) (*Result, error) {
	durableSet := make(map[string]bool)
	for name := range cfg.Closure.DurableLeaves {
		durableSet[name] = true
	}
	for name := range cfg.Closure.DurableClosure {
		durableSet[name] = true
	}

	fset := cfg.Result.TargetPkg.Fset

	// Find the global h (var h *cleat.HostCalls) and who references it.
	globalH, globalHUsers, globalHFile := findGlobalH(cfg.Result)
	hasGlobalH := globalH != nil
	if hasGlobalH {
		for _, fd := range cfg.Result.Funcs {
			fqname := fd.FullyQualifiedName()
			if !durableSet[fqname] {
				continue
			}
			if !globalHUsers[fqname] {
				continue
			}
			// This function is in the closure and uses the global h.
			// After we add h as a param, the param shadows the global.
		}
	}

	// Determine which functions need h added.
	hasH := make(map[string]bool)
	needsH := make(map[string]bool)

	for _, fd := range cfg.Result.Funcs {
		fqname := fd.FullyQualifiedName()
		if !durableSet[fqname] {
			continue
		}
		if hasHostCallsParam(fd) {
			hasH[fqname] = true
			continue
		}
		if fd.RecvType != nil {
			continue
		}
		if hasGlobalH && globalHUsers[fqname] {
			needsH[fqname] = true
			continue
		}
		// Pass-through function: in the closure but doesn't reference h
		// directly. Only auto-thread if the context object pattern is active.
		if hasGlobalH {
			needsH[fqname] = true
		}
	}

	if len(needsH) == 0 {
		return &Result{Files: make(map[string][]byte)}, nil
	}

	// Build func-to-file mapping.
	funcFile := buildFuncFileMap(cfg.Result.TargetPkg.Files, cfg.Result)

	// Phase 1: Add h parameter.
	for fqname := range needsH {
		fd := cfg.Result.Funcs[fqname]
		if fd == nil || fd.Ast == nil {
			continue
		}
		file := funcFile[fqname]
		if file == nil {
			file = findFileForFunc(cfg.Result.TargetPkg.Files, fd.Ast)
		}
		if file == nil {
			continue
		}
		addHostCallsParam(fd.Ast)
		fd.AutoThreaded = true
		ensureHostCallsImport(file, cfg.Result)
	}

	// Phase 2: Update call sites.
	modifiedFuncs := make(map[string]bool)
	for name := range needsH {
		modifiedFuncs[name] = true
	}
	for name := range hasH {
		modifiedFuncs[name] = true
	}

	for _, file := range cfg.Result.TargetPkg.Files {
		updateCallSites(file, cfg.Result.TargetPkg.Info, modifiedFuncs, fset)
	}

	// Phase 3: Remove unused global h.
	if hasGlobalH && globalHFile != nil {
		if canRemoveGlobalH(globalHUsers, needsH, cfg.Result) {
			removeGlobalDecl(globalHFile, globalH)
		}
	}

	// Phase 4: Print modified files.
	tr := &Result{
		Files:  make(map[string][]byte),
		AddedH: make([]string, 0, len(needsH)),
	}
	for name := range needsH {
		tr.AddedH = append(tr.AddedH, name)
	}

	for _, file := range cfg.Result.TargetPkg.Files {
		filename := fset.Position(file.Pos()).Filename
		if filename == "" {
			continue
		}
		var buf bytes.Buffer
		if err := format.Node(&buf, fset, file); err != nil {
			return nil, fmt.Errorf("formatting %s: %w", filename, err)
		}
		tr.Files[filename] = buf.Bytes()
	}

	return tr, nil
}

// globalHInfo tracks the package-level var h *cleat.HostCalls.
type globalHInfo struct {
	Spec *ast.ValueSpec // the var h spec
	Obj  types.Object   // the types.Object for the global
}

// findGlobalH finds var h *cleat.HostCalls in the target package and
// returns it, plus the set of function FQNames that reference it,
// and the file containing the declaration.
func findGlobalH(result *analyzer.AnalysisResult) (*globalHInfo, map[string]bool, *ast.File) {
	info := result.TargetPkg.Info
	if info == nil {
		return nil, nil, nil
	}

	// Find the AST declaration for var h. Start from the AST (not Defs)
	// to avoid matching function parameters named h.
	var globalObj types.Object
	var globalSpec *ast.ValueSpec
	var globalFile *ast.File
	for _, file := range result.TargetPkg.Files {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, name := range vs.Names {
					if name.Name == "h" {
						if obj, ok := info.Defs[name]; ok {
							if v, ok := obj.(*types.Var); ok {
								if analyzer.IsHostCallsType(v.Type()) {
									globalObj = obj
									globalSpec = vs
									globalFile = file
									break
								}
							}
						}
					}
				}
			}
		}
	}
	if globalObj == nil {
		return nil, nil, nil
	}

	// Find functions that reference the global h.
	users := make(map[string]bool)
	for id, obj := range info.Uses {
		if obj == globalObj {
			enclosing := analyzer.FindEnclosingFuncName(result.TargetPkg.Files, id)
			if enclosing != "" {
				users[enclosing] = true
			}
		}
	}

	return &globalHInfo{Spec: globalSpec, Obj: globalObj}, users, globalFile
}

// canRemoveGlobalH checks if the global h can be removed (no non-durable
// functions reference it, and all cleat functions that used it are
// getting h added as a param).
func canRemoveGlobalH(users, needsH map[string]bool, result *analyzer.AnalysisResult) bool {
	for fqname := range users {
		if needsH[fqname] {
			continue // getting h added as param
		}
		fd := result.Funcs[fqname]
		if fd != nil && hasHostCallsParam(fd) {
			continue // already has h
		}
		return false
	}
	return true
}

// removeGlobalDecl removes the var h spec from the file.
func removeGlobalDecl(file *ast.File, gh *globalHInfo) {
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		var newSpecs []ast.Spec
		for _, spec := range gen.Specs {
			if spec == gh.Spec {
				continue
			}
			newSpecs = append(newSpecs, spec)
		}
		gen.Specs = newSpecs
	}
}

// hasHostCallsParam checks if the function's first parameter is HostCalls.
func hasHostCallsParam(fd *analyzer.FuncDecl) bool {
	if fd.Type == nil || fd.Type.Params() == nil || fd.Type.Params().Len() == 0 {
		return false
	}
	return analyzer.IsHostCallsType(fd.Type.Params().At(0).Type())
}

// addHostCallsParam inserts h cleat.HostCalls as the first parameter.
func addHostCallsParam(fn *ast.FuncDecl) {
	paramName := "h"
	if fn.Type.Params != nil {
		for _, field := range fn.Type.Params.List {
			for _, name := range field.Names {
				if name.Name == "h" {
					if isHostCallsField(field) {
						return // already has it
					}
					// A non-HostCalls param named "h" exists; use a unique name.
					paramName = "h2"
				}
			}
		}
	}

	newParam := &ast.Field{
		Names: []*ast.Ident{ast.NewIdent(paramName)},
		Type: &ast.SelectorExpr{
			X:   ast.NewIdent("durable"),
			Sel: ast.NewIdent("HostCalls"),
		},
	}

	if fn.Type.Params == nil {
		fn.Type.Params = &ast.FieldList{}
	}
	fn.Type.Params.List = append([]*ast.Field{newParam}, fn.Type.Params.List...)
}

// isHostCallsField checks if a field is of type cleat.HostCalls.
func isHostCallsField(field *ast.Field) bool {
	sel, ok := field.Type.(*ast.SelectorExpr)
	if !ok {
		// Also check for *cleat.HostCalls (pointer to struct, backward compat).
		star, ok := field.Type.(*ast.StarExpr)
		if !ok {
			return false
		}
		sel, ok = star.X.(*ast.SelectorExpr)
		if !ok {
			return false
		}
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "durable" && sel.Sel.Name == "HostCalls"
}

// ensureHostCallsImport ensures the file imports "github.com/rcownie/cleat/durable".
func ensureHostCallsImport(file *ast.File, result *analyzer.AnalysisResult) {
	importPath := "github.com/rcownie/cleat/durable"

	for _, imp := range file.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if path == importPath {
			if imp.Name == nil || imp.Name.Name != "durable" {
				imp.Name = ast.NewIdent("durable")
			}
			return
		}
		if imp.Name != nil && imp.Name.Name == "durable" {
			return
		}
		if strings.HasSuffix(path, "/durable") {
			return
		}
	}

	newImport := &ast.ImportSpec{
		Name: ast.NewIdent("durable"),
		Path: &ast.BasicLit{
			Kind:  token.STRING,
			Value: `"` + importPath + `"`,
		},
	}

	var importDecl *ast.GenDecl
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if ok && gen.Tok == token.IMPORT {
			importDecl = gen
			break
		}
	}

	if importDecl == nil {
		importDecl = &ast.GenDecl{
			Tok:   token.IMPORT,
			Specs: []ast.Spec{newImport},
		}
		file.Decls = append([]ast.Decl{importDecl}, file.Decls...)
	} else {
		importDecl.Specs = append(importDecl.Specs, newImport)
	}
}

// updateCallSites inserts h as the first argument when calling a function
// that has h (either originally or after auto-threading).
func updateCallSites(file *ast.File, info *types.Info, modifiedFuncs map[string]bool, fset *token.FileSet) {
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		calleeFQName := resolveCalleeFQName(call, info)
		if calleeFQName == "" || !modifiedFuncs[calleeFQName] {
			return true
		}
		// Check if h is already the first argument.
		if len(call.Args) > 0 {
			if ident, ok := call.Args[0].(*ast.Ident); ok && ident.Name == "h" {
				return true
			}
		}
		call.Args = append([]ast.Expr{ast.NewIdent("h")}, call.Args...)
		return true
	})
}

// resolveCalleeFQName resolves a call expression to a fully-qualified name.
func resolveCalleeFQName(call *ast.CallExpr, info *types.Info) string {
	if info == nil {
		return ""
	}
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		obj, ok := info.Uses[fun]
		if !ok {
			return ""
		}
		if fn, ok := obj.(*types.Func); ok {
			return fn.FullName()
		}
	case *ast.SelectorExpr:
		if sel, ok := info.Selections[fun]; ok {
			if fn, ok := sel.Obj().(*types.Func); ok {
				return fn.FullName()
			}
		}
		if obj, ok := info.Uses[fun.Sel]; ok {
			if fn, ok := obj.(*types.Func); ok {
				return fn.FullName()
			}
		}
	}
	return ""
}

// findFileForFunc finds the *ast.File containing the given FuncDecl.
func findFileForFunc(files []*ast.File, target *ast.FuncDecl) *ast.File {
	for _, file := range files {
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn == target {
				return file
			}
		}
	}
	return nil
}

// buildFuncFileMap creates a mapping from fully-qualified function names
// to the *ast.File that contains them.
func buildFuncFileMap(files []*ast.File, result *analyzer.AnalysisResult) map[string]*ast.File {
	m := make(map[string]*ast.File)
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			for fqname, fd := range result.Funcs {
				if fd.Ast == fn {
					m[fqname] = file
					break
				}
			}
		}
	}
	return m
}
