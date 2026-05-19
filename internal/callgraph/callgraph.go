// Package callgraph builds a directed graph of function calls within
// user packages and identifies cleat leaves (functions that directly
// call HostCalls methods).
package callgraph

import (
	"fmt"
	"go/ast"
	"go/types"

	"github.com/cleat-team/cleat/internal/analyzer"
)

// Graph represents the directed call graph of functions.
type Graph struct {
	// Calls maps caller → set of callees (fully-qualified names).
	Calls map[string]map[string]bool

	// CalledBy maps callee → set of callers (reverse edges).
	CalledBy map[string]map[string]bool

	// DurableLeaves maps fully-qualified names of functions that directly
	// call at least one HostCalls method.
	DurableLeaves map[string]bool
}

// Build constructs a call graph from the given analysis result.
func Build(result *analyzer.AnalysisResult) (*Graph, error) {
	g := &Graph{
		Calls:         make(map[string]map[string]bool),
		CalledBy:      make(map[string]map[string]bool),
		DurableLeaves: make(map[string]bool),
	}

	// Initialize entries for all known functions.
	for _, fd := range result.Funcs {
		fqname := fd.FullyQualifiedName()
		g.Calls[fqname] = make(map[string]bool)
	}

	// Walk function bodies and resolve calls.
	for _, fd := range result.Funcs {
		callerName := fd.FullyQualifiedName()
		if fd.Ast.Body == nil {
			continue
		}
		ast.Inspect(fd.Ast.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if calleeName := resolveCallee(call, fd.Pkg.Info); calleeName != "" {
				g.Calls[callerName][calleeName] = true
			}
			return true
		})
	}

	// Build reverse edges.
	for caller, callees := range g.Calls {
		for callee := range callees {
			if g.CalledBy[callee] == nil {
				g.CalledBy[callee] = make(map[string]bool)
			}
			g.CalledBy[callee][caller] = true
		}
	}

	// Identify cleat leaves (functions calling HostCalls methods).
	for _, fd := range result.Funcs {
		if hasHostCallsCall(fd) {
			fqname := fd.FullyQualifiedName()
			fd.IsDurableLeaf = true
			fd.DurabilityTag = "DurableLeaf"
			g.DurableLeaves[fqname] = true
		}
	}

	return g, nil
}

// resolveCallee resolves a call expression to a fully-qualified function name.
// Returns empty string if the callee cannot be resolved.
func resolveCallee(call *ast.CallExpr, info *types.Info) string {
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
			return analyzer.FuncFQName(fn)
		}

	case *ast.SelectorExpr:
		// Method call: receiver.Method(args)
		if sel, ok := info.Selections[fun]; ok {
			if fn, ok := sel.Obj().(*types.Func); ok {
				return analyzer.FuncFQName(fn)
			}
			// Struct field access (e.g., h.DurableCall where
			// DurableCall is a func-typed field). Not a tracked call.
			return ""
		}
		// Package-qualified call: pkg.Func(args)
		if obj, ok := info.Uses[fun.Sel]; ok {
			if fn, ok := obj.(*types.Func); ok {
				return analyzer.FuncFQName(fn)
			}
		}

	case *ast.IndexExpr:
		// Generic function instantiation: Func[T](args)
		if ident, ok := fun.X.(*ast.Ident); ok {
			obj, ok := info.Uses[ident]
			if !ok {
				return ""
			}
			if fn, ok := obj.(*types.Func); ok {
				return analyzer.FuncFQName(fn)
			}
			return ""
		}
		// Generic method call with explicit type args: obj.Method[T](args)
		if sel, ok := fun.X.(*ast.SelectorExpr); ok {
			if selInfo, ok := info.Selections[sel]; ok {
				if fn, ok := selInfo.Obj().(*types.Func); ok {
					return analyzer.FuncFQName(fn)
				}
			}
			if obj, ok := info.Uses[sel.Sel]; ok {
				if fn, ok := obj.(*types.Func); ok {
					return analyzer.FuncFQName(fn)
				}
			}
		}
	}
	return ""
}

// hasHostCallsCall checks if a function's body contains any call to
// a method on a HostCalls value.
func hasHostCallsCall(fd *analyzer.FuncDecl) bool {
	if fd.Ast.Body == nil || fd.Pkg.Info == nil {
		return false
	}
	// PluginCaller methods are trusted boundaries — automatically durable leaves.
	if fd.RecvType != nil && analyzer.ImplementsPluginCaller(fd.RecvType) {
		return true
	}
	found := false
	ast.Inspect(fd.Ast.Body, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		selExpr, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		sel, ok := fd.Pkg.Info.Selections[selExpr]
		if !ok {
			return true
		}
		if analyzer.HostCallsMethod(sel) || analyzer.PluginCallerMethod(sel) {
			found = true
			return false
		}
		return true
	})
	return found
}

// NumEdges returns the total number of edges in the call graph.
func (g *Graph) NumEdges() int {
	count := 0
	for _, callees := range g.Calls {
		count += len(callees)
	}
	return count
}

// String returns a human-readable summary.
func (g *Graph) String() string {
	return fmt.Sprintf("Call graph: %d functions, %d edges, %d cleat leaves",
		len(g.Calls), g.NumEdges(), len(g.DurableLeaves))
}
