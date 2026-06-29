package closure

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"github.com/cleat-team/cleat/internal/analyzer"
	"github.com/cleat-team/cleat/internal/callgraph"
)

// ThreadingError records a function in the cleat closure that lacks
// access to HostCalls with the call chain that leads to it.
type ThreadingError struct {
	FuncName string
	Chain    []string // call chain from entry point to this function
	Line     int
	Message  string
}

// VerifyThreading checks that every function in the cleat closure has
// access to cleat.HostCalls through its parameter list, through a
// package-level global var h, or through a caller that passes it.
func VerifyThreading(result *analyzer.AnalysisResult, cg *callgraph.Graph, cr *Result) []ThreadingError {
	// Build the set of functions in the cleat closure.
	durableSet := make(map[string]bool)
	for name := range cr.DurableLeaves {
		durableSet[name] = true
	}
	for name := range cr.DurableClosure {
		durableSet[name] = true
	}

	// Track which functions have HostCalls access and how.
	threaded := make(map[string]bool)

	// Phase 0: Detect package-level var h *cleat.HostCalls.
	// Functions that reference this global have implicit access.
	globalHObj := findGlobalHostCalls(result)
	usesGlobalH := make(map[string]bool)
	if globalHObj != nil {
		usesGlobalH = findGlobalHUsers(result, globalHObj)
		for name := range durableSet {
			if usesGlobalH[name] {
				threaded[name] = true
			}
		}
	}

	// Phase 1: Functions whose first param is HostCalls are directly threaded,
	// or that were auto-threaded by the transform.
	for name := range durableSet {
		if threaded[name] {
			continue
		}
		fd := result.Funcs[name]
		if fd == nil {
			continue
		}
		if hasHostCallsParam(fd) || fd.AutoThreaded {
			threaded[name] = true
		}
	}

	// Phase 1b: Methods on PluginCaller types are trusted boundaries -- auto-threaded.
	for name := range durableSet {
		if threaded[name] {
			continue
		}
		fd := result.Funcs[name]
		if fd == nil || fd.RecvType == nil {
			continue
		}
		if analyzer.ImplementsPluginCaller(fd.RecvType) {
			threaded[name] = true
		}
	}

	// Phase 2: Functions called by threaded callers that pass their
	// HostCalls as an argument become threaded transitively.
	changed := true
	for changed {
		changed = false
		for _, fd := range result.Funcs {
			name := fd.FullyQualifiedName()
			if !durableSet[name] || threaded[name] {
				continue
			}
			// Check if any caller is threaded AND passes HostCalls to this function.
			for callerName := range cg.CalledBy[name] {
				if threaded[callerName] && callerPassesHostCalls(callerName, name, result, cg) {
					threaded[name] = true
					changed = true
					break
				}
			}
		}
	}

	// Phase 3: Struct methods where the struct has a HostCalls field.
	for name := range durableSet {
		if threaded[name] {
			continue
		}
		fd := result.Funcs[name]
		if fd == nil || fd.RecvType == nil {
			continue
		}
		if structHasHostCallsField(fd.RecvType, fd.Pkg) {
			threaded[name] = true
		}
	}

	// Collect errors for unthreaded functions.
	var errors []ThreadingError
	for name := range durableSet {
		if threaded[name] {
			continue
		}
		fd := result.Funcs[name]
		if fd == nil {
			continue
		}
		chain := findCallChain(name, result.EntryPoints, cg)
		line := 0
		if fd.Pkg.Fset != nil {
			line = fd.Pkg.Fset.Position(fd.Ast.Pos()).Line
		}
		errors = append(errors, ThreadingError{
			FuncName: name,
			Chain:    chain,
			Line:     line,
			Message: fmt.Sprintf(
				"%s is reachable from a workflow entry point (it calls durable SDK methods) but does not have a HostCalls parameter. "+
					"Add 'h cleat.HostCalls' as the first parameter, or declare a package-level 'var h cleat.HostCalls' that this function can reference.",
				analyzer.ShortName(name)),
		})
	}

	// Augment DebugInfo with threading errors.
	if cr.DebugInfo != nil {
		for _, te := range errors {
			for i, d := range cr.DebugInfo.Decisions {
				if d.FuncName == te.FuncName {
					cr.DebugInfo.Decisions[i].Reasons = append(
						cr.DebugInfo.Decisions[i].Reasons,
						"HostCalls threading error: "+te.Message)
					break
				}
			}
		}
	}

	return errors
}

// hasHostCallsParam checks if the function's first parameter is HostCalls.
func hasHostCallsParam(fd *analyzer.FuncDecl) bool {
	if fd.Type == nil {
		return false
	}
	params := fd.Type.Params()
	if params == nil || params.Len() == 0 {
		return false
	}
	return analyzer.IsHostCallsType(params.At(0).Type())
}

// callerPassesHostCalls checks if caller passes its HostCalls as an
// argument when calling callee.
func callerPassesHostCalls(callerName, calleeName string, result *analyzer.AnalysisResult, cg *callgraph.Graph) bool {
	callerFd := result.Funcs[callerName]
	if callerFd == nil || callerFd.Ast.Body == nil {
		return false
	}

	calleeShortName := analyzer.ShortName(calleeName)

	passes := false
	ast.Inspect(callerFd.Ast.Body, func(n ast.Node) bool {
		if passes {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		// Check if this call is to our callee.
		calleeIdent := resolveCallIdent(call)
		if calleeIdent != calleeShortName && !strings.HasSuffix(calleeIdent, "."+calleeShortName) {
			return true
		}
		// Check if any argument is the caller's HostCalls parameter.
		for _, arg := range call.Args {
			if ident, ok := arg.(*ast.Ident); ok {
				if isCallerHostCallsParam(ident.Name, callerFd) {
					passes = true
					return false
				}
			}
		}
		return true
	})
	return passes
}

// resolveCallIdent returns the identifier text for a call expression's function.
func resolveCallIdent(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		return fun.Sel.Name
	}
	return ""
}

// isCallerHostCallsParam checks if a parameter name refers to the caller's
// HostCalls parameter.
func isCallerHostCallsParam(paramName string, fd *analyzer.FuncDecl) bool {
	if fd.Type == nil || fd.Type.Params() == nil || fd.Type.Params().Len() == 0 {
		return false
	}
	firstParam := fd.Type.Params().At(0)
	if !analyzer.IsHostCallsType(firstParam.Type()) {
		return false
	}
	return firstParam.Name() == paramName
}

// structHasHostCallsField checks if a type (pointer to struct) has a
// field of type *cleat.HostCalls or cleat.HostCalls.
func structHasHostCallsField(t types.Type, pkg *analyzer.Package) bool {
	// Unwrap pointer.
	named := t
	if ptr, ok := t.(*types.Pointer); ok {
		if n, ok := ptr.Elem().(*types.Named); ok {
			named = n
		} else {
			return false
		}
	}

	n, ok := named.(*types.Named)
	if !ok {
		return false
	}

	strct, ok := n.Underlying().(*types.Struct)
	if !ok {
		return false
	}

	for i := 0; i < strct.NumFields(); i++ {
		if analyzer.IsHostCallsType(strct.Field(i).Type()) {
			return true
		}
	}
	return false
}

// findCallChain finds a call chain from any entry point to the target function.
func findCallChain(target string, entryPoints []string, cg *callgraph.Graph) []string {
	for _, ep := range entryPoints {
		chain := dfsChain(ep, target, cg, nil)
		if chain != nil {
			return chain
		}
	}
	return nil
}

func dfsChain(current, target string, cg *callgraph.Graph, visited map[string]bool) []string {
	if current == target {
		return []string{analyzer.ShortName(current)}
	}
	if visited == nil {
		visited = make(map[string]bool)
	}
	if visited[current] {
		return nil
	}
	visited[current] = true

	for callee := range cg.Calls[current] {
		if chain := dfsChain(callee, target, cg, visited); chain != nil {
			return append([]string{analyzer.ShortName(current)}, chain...)
		}
	}
	return nil
}

// findGlobalHostCalls looks for var h *cleat.HostCalls in the target
// package and returns the types.Object for it, or nil if not found.
// Starts from the AST to avoid matching function parameters named h.
func findGlobalHostCalls(result *analyzer.AnalysisResult) types.Object {
	info := result.TargetPkg.Info
	if info == nil {
		return nil
	}
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
									return obj
								}
							}
						}
					}
				}
			}
		}
	}
	return nil
}

// findGlobalHUsers returns the set of function FQNames that reference the
// global h object. Uses *types.Info.Uses to find references.
func findGlobalHUsers(result *analyzer.AnalysisResult, globalObj types.Object) map[string]bool {
	users := make(map[string]bool)
	info := result.TargetPkg.Info
	if info == nil {
		return users
	}

	for id, obj := range info.Uses {
		if obj != globalObj {
			continue
		}
		// Find the enclosing function for this identifier.
		enclosing := analyzer.FindEnclosingFuncName(result.TargetPkg.Files, id)
		if enclosing == "" {
			continue
		}
		// Match against FuncDecl entries.
		for fqname, fd := range result.Funcs {
			if fd.Ast.Name.Name == enclosing {
				users[fqname] = true
				break
			}
		}
	}
	return users
}
