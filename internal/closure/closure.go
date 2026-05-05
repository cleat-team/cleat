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
		}
		return true
	})
}

// checkForbiddenCall checks if a call expression uses a forbidden function.
func checkForbiddenCall(call *ast.CallExpr, fd *analyzer.FuncDecl, funcName string, cr *Result, fset *token.FileSet) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return
	}

	pkgIdent, ok := sel.X.(*ast.Ident)
	if !ok {
		return
	}

	pkgPath := resolveImportPath(fd, pkgIdent.Name)
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

// resolveImportPath resolves a package identifier to its full import path.
func resolveImportPath(fd *analyzer.FuncDecl, name string) string {
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
