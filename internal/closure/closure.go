// Package closure computes the transitive closure of durable functions,
// validates supported Go constructs, and verifies HostCalls threading.
package closure

import (
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"github.com/rcownie/durable/internal/analyzer"
	"github.com/rcownie/durable/internal/callgraph"
)

// Compute computes the durable closure: the set of all functions that
// transitively reach a durable leaf. It annotates each function with
// its durability tag (DurableLeaf, DurableClosure, or Pure) and
// validates that durable functions don't use unsupported Go constructs.
func Compute(result *analyzer.AnalysisResult, cg *callgraph.Graph) *Result {
	cr := &Result{
		DurableLeaves:  make(map[string]bool),
		DurableClosure: make(map[string]bool),
		Pure:           make(map[string]bool),
		Errors:         make(map[string][]ValidationError),
		Warnings:       make(map[string][]ValidationWarning),
	}

	// Start with durable leaves.
	for name := range cg.DurableLeaves {
		cr.DurableLeaves[name] = true
	}

	// Compute transitive closure: any function that calls a function
	// in the durable closure is itself in the durable closure.
	changed := true
	for changed {
		changed = false
		for caller, callees := range cg.Calls {
			if cr.DurableLeaves[caller] || cr.DurableClosure[caller] {
				continue
			}
			for callee := range callees {
				if cr.DurableLeaves[callee] || cr.DurableClosure[callee] {
					cr.DurableClosure[caller] = true
					changed = true
					break
				}
			}
		}
	}

	// Tag every function.
	for name := range result.Funcs {
		if cr.DurableLeaves[name] {
			continue
		}
		if cr.DurableClosure[name] {
			continue
		}
		cr.Pure[name] = true
	}

	// Update analysis result tags.
	for name := range cr.DurableLeaves {
		if fd, ok := result.Funcs[name]; ok {
			fd.IsDurableLeaf = true
			fd.DurabilityTag = "DurableLeaf"
		}
	}
	for name := range cr.DurableClosure {
		if fd, ok := result.Funcs[name]; ok {
			fd.InDurableClosure = true
			fd.DurabilityTag = "DurableClosure"
		}
	}
	for name := range cr.Pure {
		if fd, ok := result.Funcs[name]; ok {
			fd.DurabilityTag = "Pure"
		}
	}

	result.NumDurableLeaves = len(cr.DurableLeaves)
	result.NumDurableClosure = len(cr.DurableClosure)
	result.NumPure = len(cr.Pure)

	// Validate supported constructs in all durable functions.
	for _, fd := range result.Funcs {
		if fd.DurabilityTag == "DurableLeaf" || fd.DurabilityTag == "DurableClosure" {
			validateConstructs(fd, cr)
		}
	}

	return cr
}

// Result holds the results of closure computation and validation.
type Result struct {
	DurableLeaves  map[string]bool
	DurableClosure map[string]bool
	Pure           map[string]bool
	Errors         map[string][]ValidationError
	Warnings       map[string][]ValidationWarning
}

// ValidationError represents a validation error with its error code.
type ValidationError struct {
	Code       string
	FuncName   string
	Message    string
	Suggestion string
	Line       int
}

// ValidationWarning represents a validation warning.
type ValidationWarning struct {
	Code     string
	FuncName string
	Message  string
	Line     int
}

// NumErrors returns the total number of validation errors.
func (cr *Result) NumErrors() int {
	count := 0
	for _, errs := range cr.Errors {
		count += len(errs)
	}
	return count
}

// NumWarnings returns the total number of warnings.
func (cr *Result) NumWarnings() int {
	count := 0
	for _, warns := range cr.Warnings {
		count += len(warns)
	}
	return count
}

// validateConstructs checks a function for unsupported Go constructs.
func validateConstructs(fd *analyzer.FuncDecl, cr *Result) {
	if fd.Ast.Body == nil {
		return
	}

	var fset *token.FileSet
	if fd.Pkg != nil {
		fset = fd.Pkg.Fset
	}

	name := fd.FullyQualifiedName()
	ast.Inspect(fd.Ast.Body, func(n ast.Node) bool {
		switch stmt := n.(type) {
		case *ast.GoStmt:
			line := 0
			if fset != nil {
				line = fset.Position(stmt.Pos()).Line
			}
			cr.Errors[name] = append(cr.Errors[name], ValidationError{
				Code:       "E001",
				FuncName:   name,
				Message:    "goroutines are not allowed in durable functions",
				Suggestion: "Use child workflows (h.ChildWorkflow) for parallelism.",
				Line:       line,
			})

		case *ast.SendStmt:
			line := 0
			if fset != nil {
				line = fset.Position(stmt.Pos()).Line
			}
			cr.Errors[name] = append(cr.Errors[name], ValidationError{
				Code:       "E002",
				FuncName:   name,
				Message:    "channel send operations are not allowed in durable functions",
				Suggestion: "Use signals (h.AwaitSignals) instead of channels.",
				Line:       line,
			})

		case *ast.UnaryExpr:
			if stmt.Op.String() == "<-" {
				line := 0
				if fset != nil {
					line = fset.Position(stmt.Pos()).Line
				}
				cr.Errors[name] = append(cr.Errors[name], ValidationError{
					Code:       "E002",
					FuncName:   name,
					Message:    "channel receive operations are not allowed in durable functions",
					Suggestion: "Use signals (h.PollSignal) instead of channels.",
					Line:       line,
				})
			}

		case *ast.CallExpr:
			checkForbiddenCall(stmt, fd, name, cr, fset)
			checkInterfaceDispatch(stmt, fd, name, cr, fset)
			checkFuncValueCall(stmt, fd, name, cr, fset)

		case *ast.IfStmt:
			checkFloatInExpr(stmt.Cond, fd, name, cr, fset)

		case *ast.ForStmt:
			if stmt.Cond != nil {
				checkFloatInExpr(stmt.Cond, fd, name, cr, fset)
			}

		case *ast.SwitchStmt:
			if stmt.Tag != nil {
				checkFloatInExpr(stmt.Tag, fd, name, cr, fset)
			}

		case *ast.RangeStmt:
			if stmt.Key != nil && stmt.X != nil && fd.Pkg.Info != nil {
				if tv, ok := fd.Pkg.Info.Types[stmt.X]; ok {
					if _, isMap := tv.Type.Underlying().(*types.Map); isMap {
						line := 0
						if fset != nil {
							line = fset.Position(stmt.Pos()).Line
						}
						cr.Warnings[name] = append(cr.Warnings[name], ValidationWarning{
							Code:     "W001",
							FuncName: name,
							Message:  "map iteration order is non-deterministic; use sorted slices for deterministic replay",
							Line:     line,
						})
					}
				}
			}

		case *ast.SelectorExpr:
			checkForbiddenPackageRef(stmt, fd, name, cr, fset)
		}
		return true
	})
}

// checkForbiddenCall checks if a call expression uses a forbidden function.
func checkForbiddenCall(call *ast.CallExpr, fd *analyzer.FuncDecl, funcName string, cr *Result, fset *token.FileSet) {
	// Check builtins.
	if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "close" {
		line := 0
		if fset != nil {
			line = fset.Position(call.Pos()).Line
		}
		cr.Errors[funcName] = append(cr.Errors[funcName], ValidationError{
			Code:       "E012",
			FuncName:   funcName,
			Message:    "close() is not allowed in durable functions",
			Suggestion: "Channel operations are non-deterministic; use signals instead.",
			Line:       line,
		})
		return
	}

	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return
	}

	pkgIdent, ok := sel.X.(*ast.Ident)
	if !ok {
		return
	}

	pkgPath := resolveImportPath(fd, pkgIdent)
	selName := sel.Sel.Name

	var code, msg, suggestion string

	switch {
	case pkgPath == "time" && selName == "Now":
		code, msg, suggestion = "E003",
			"time.Now() is not allowed in durable functions",
			"Use h.Now() for deterministic time."

	case pkgPath == "time" && selName == "Sleep":
		code, msg, suggestion = "E004",
			"time.Sleep() is not allowed in durable functions",
			"Use h.DurableSleep() instead."

	case pkgPath == "net/http" || strings.HasPrefix(pkgPath, "net/http/"):
		code, msg, suggestion = "E005",
			"direct net/http calls are not allowed in durable functions",
			"Use h.DurableCall() with a service name instead."

	case pkgPath == "database/sql":
		code, msg, suggestion = "E006",
			"direct database/sql calls are not allowed in durable functions",
			"Use h.DurableCall() with a service name instead."

	case pkgPath == "math/rand":
		code, msg, suggestion = "E007",
			"math/rand calls are not allowed in durable functions",
			"Use h.Random() for deterministic randomness."
	}

	if code != "" {
		line := 0
		if fset != nil {
			line = fset.Position(call.Pos()).Line
		}
		cr.Errors[funcName] = append(cr.Errors[funcName], ValidationError{
			Code:       code,
			FuncName:   funcName,
			Message:    msg,
			Suggestion: suggestion,
			Line:       line,
		})
	}
}

// checkInterfaceDispatch detects calls through interface dispatch (other
// than HostCalls), which cannot be statically resolved.
func checkInterfaceDispatch(call *ast.CallExpr, fd *analyzer.FuncDecl, funcName string, cr *Result, fset *token.FileSet) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return
	}
	xIdent, ok := sel.X.(*ast.Ident)
	if !ok {
		return
	}
	if fd.Pkg == nil || fd.Pkg.Info == nil {
		return
	}
	tv, ok := fd.Pkg.Info.Types[xIdent]
	if !ok {
		return
	}
	// Only flag interface types that are NOT durable.HostCalls.
	_, isInterface := tv.Type.Underlying().(*types.Interface)
	if !isInterface {
		return
	}
	if analyzer.IsHostCallsType(tv.Type) {
		return
	}
	line := 0
	if fset != nil {
		line = fset.Position(call.Pos()).Line
	}
	cr.Errors[funcName] = append(cr.Errors[funcName], ValidationError{
		Code:       "E008",
		FuncName:   funcName,
		Message:    "unresolvable function call through interface dispatch",
		Suggestion: "Use concrete types or refactor to avoid interface dispatch in durable functions.",
		Line:       line,
	})
}

// checkFuncValueCall detects calls where the function is a variable
// holding a function value, which cannot be statically resolved.
func checkFuncValueCall(call *ast.CallExpr, fd *analyzer.FuncDecl, funcName string, cr *Result, fset *token.FileSet) {
	ident, ok := call.Fun.(*ast.Ident)
	if !ok {
		return
	}
	if fd.Pkg == nil || fd.Pkg.Info == nil {
		return
	}
	obj, ok := fd.Pkg.Info.Uses[ident]
	if !ok {
		return
	}
	// If it's a *types.Func, it's a direct function call — that's fine.
	if _, isFunc := obj.(*types.Func); isFunc {
		return
	}
	// If it's a *types.Var, it's a function value call — not statically resolvable.
	if _, isVar := obj.(*types.Var); !isVar {
		return
	}
	line := 0
	if fset != nil {
		line = fset.Position(call.Pos()).Line
	}
	cr.Errors[funcName] = append(cr.Errors[funcName], ValidationError{
		Code:       "E009",
		FuncName:   funcName,
		Message:    "function value call cannot be statically resolved",
		Suggestion: "Replace with a direct function call or inline the logic.",
		Line:       line,
	})
}

// checkForbiddenPackageRef detects references to packages that are forbidden
// in durable functions (os, reflect).
func checkForbiddenPackageRef(sel *ast.SelectorExpr, fd *analyzer.FuncDecl, funcName string, cr *Result, fset *token.FileSet) {
	pkgIdent, ok := sel.X.(*ast.Ident)
	if !ok {
		return
	}

	pkgPath := resolveImportPath(fd, pkgIdent)
	if pkgPath == "" {
		return
	}

	var code, msg, suggestion string

	switch {
	case pkgPath == "os":
		code, msg, suggestion = "E010",
			"direct os package usage is not allowed in durable functions",
			"Use h.DurableCall() with a service name instead of direct OS operations."
	case pkgPath == "reflect":
		code, msg, suggestion = "E011",
			"direct reflect package usage is not allowed in durable functions",
			"Avoid runtime type introspection; use compile-time generics where possible."
	}

	if code != "" {
		line := 0
		if fset != nil {
			line = fset.Position(sel.Pos()).Line
		}
		cr.Errors[funcName] = append(cr.Errors[funcName], ValidationError{
			Code:       code,
			FuncName:   funcName,
			Message:    msg,
			Suggestion: suggestion,
			Line:       line,
		})
	}
}

// checkFloatInExpr walks an expression looking for identifiers whose type
// is float32 or float64 and issues a W002 warning if any are found.
func checkFloatInExpr(expr ast.Expr, fd *analyzer.FuncDecl, funcName string, cr *Result, fset *token.FileSet) {
	if fd.Pkg == nil || fd.Pkg.Info == nil {
		return
	}
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if found {
			return false
		}
		ident, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		tv, ok := fd.Pkg.Info.Types[ident]
		if !ok {
			return true
		}
		basic, ok := tv.Type.Underlying().(*types.Basic)
		if !ok {
			return true
		}
		if basic.Kind() == types.Float32 || basic.Kind() == types.Float64 {
			found = true
			return false
		}
		return true
	})
	if found {
		line := 0
		if fset != nil {
			line = fset.Position(expr.Pos()).Line
		}
		cr.Warnings[funcName] = append(cr.Warnings[funcName], ValidationWarning{
			Code:     "W002",
			FuncName: funcName,
			Message:  "floating-point value in control flow condition may cause non-deterministic replay",
			Line:     line,
		})
	}
}

// resolveImportPath resolves a package identifier to its full import path.
// It first tries type-checked resolution (exact), then falls back to
// matching by explicit import name or last path component.
func resolveImportPath(fd *analyzer.FuncDecl, pkgIdent *ast.Ident) string {
	// Prefer type-checked info for exact resolution.
	if fd.Pkg != nil && fd.Pkg.Info != nil {
		if obj, ok := fd.Pkg.Info.Uses[pkgIdent]; ok {
			if pkgName, ok := obj.(*types.PkgName); ok {
				return pkgName.Imported().Path()
			}
		}
	}

	name := pkgIdent.Name
	// Fallback: match by explicit import name or last path component.
	for _, file := range fd.Pkg.Files {
		for _, imp := range file.Imports {
			importPath := strings.Trim(imp.Path.Value, `"`)
			if imp.Name != nil && imp.Name.Name == name {
				return importPath
			}
			if analyzer.LastComponent(importPath) == name {
				return importPath
			}
		}
	}
	return ""
}
