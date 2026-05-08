// Package closure computes the transitive closure of cleat functions,
// validates supported Go constructs, and verifies HostCalls threading.
package closure

import (
	"go/ast"
	"go/token"
	"go/types"
	"fmt"
	"strings"

	"github.com/rcownie/cleat/internal/analyzer"
	"github.com/rcownie/cleat/internal/callgraph"
)

// Compute computes the cleat closure: the set of all functions that
// transitively reach a cleat leaf. It annotates each function with
// its durability tag (DurableLeaf, DurableClosure, or Pure) and
// validates that cleat functions don't use unsupported Go constructs.
func Compute(result *analyzer.AnalysisResult, cg *callgraph.Graph) *Result {
	cr := &Result{
		DurableLeaves:  make(map[string]bool),
		DurableClosure: make(map[string]bool),
		Pure:           make(map[string]bool),
		Errors:         make(map[string][]ValidationError),
		Warnings:       make(map[string][]ValidationWarning),
	}

	// Start with cleat leaves.
	for name := range cg.DurableLeaves {
		cr.DurableLeaves[name] = true
	}

	// Compute transitive closure: any function that calls a function
	// in the cleat closure is itself in the cleat closure.
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

	// Validate supported constructs in all cleat functions.
	for _, fd := range result.Funcs {
		if fd.DurabilityTag == "DurableLeaf" || fd.DurabilityTag == "DurableClosure" {
			validateConstructs(fd, cr)
		}
	}

	// Validate that init() functions do not call durable functions.
	validateInitFunctions(result, cr)

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
	Code       string
	FuncName   string
	Message    string
	Suggestion string
	Line       int
}

// Error returns a string with the error code prefix and message.
func (e ValidationError) Error() string {
	s := e.Code + ": " + e.Message
	if e.Suggestion != "" {
		s += " (suggestion: " + e.Suggestion + ")"
	}
	if e.Line > 0 {
		s += fmt.Sprintf(" (line %d)", e.Line)
	}
	return s
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
				Message:    "goroutines introduce non-deterministic scheduling across replays",
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
				Message:    "channel send operations are non-deterministic across replays; goroutine state is not replayed",
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
					Message:    "channel receive operations are non-deterministic across replays; goroutine state is not replayed",
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

// resolveNamedType unwraps pointer types and returns the underlying named type.
func resolveNamedType(t types.Type) *types.Named {
	if named, ok := t.(*types.Named); ok {
		return named
	}
	if ptr, ok := t.(*types.Pointer); ok {
		if named, ok := ptr.Elem().(*types.Named); ok {
			return named
		}
	}
	return nil
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
			Message:    "close() on channels signals goroutine state that does not exist during replay",
			Suggestion: "Channel operations are non-deterministic; use signals instead.",
			Line:       line,
		})
		return
	}

	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return
	}

	// Check for nested selectors like sync.Mutex.Lock() or sync.RWMutex.RLock().
	if innerSel, ok := sel.X.(*ast.SelectorExpr); ok {
		if innerPkgIdent, ok := innerSel.X.(*ast.Ident); ok {
			innerPkgPath := resolveImportPath(fd, innerPkgIdent)
			if innerPkgPath == "sync" {
				switch innerSel.Sel.Name {
				case "Mutex", "RWMutex", "WaitGroup", "Once", "Cond", "Pool", "Map":
					line := 0
					if fset != nil {
						line = fset.Position(call.Pos()).Line
					}
					cr.Errors[funcName] = append(cr.Errors[funcName], ValidationError{
						Code:       "E013",
						FuncName:   funcName,
						Message:    "sync." + innerSel.Sel.Name + " operations are non-deterministic across replays",
						Suggestion: "Workflow code is single-threaded by design. Remove the synchronization primitive -- it is not needed.",
						Line:       line,
					})
				}
			}
		}
	}

	pkgIdent, ok := sel.X.(*ast.Ident)
	if !ok {
		return
	}

	pkgPath := resolveImportPath(fd, pkgIdent)
	selName := sel.Sel.Name

	// Check for calls on sync-type variables (e.g., mu.Lock() where mu is sync.Mutex).
	if pkgPath == "" && fd.Pkg != nil && fd.Pkg.Info != nil {
		if tv, ok := fd.Pkg.Info.Types[pkgIdent]; ok {
			if named := resolveNamedType(tv.Type); named != nil {
				if named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "sync" {
					line := 0
					if fset != nil {
						line = fset.Position(call.Pos()).Line
					}
					cr.Errors[funcName] = append(cr.Errors[funcName], ValidationError{
						Code:       "E013",
						FuncName:   funcName,
						Message:    "sync." + named.Obj().Name() + " operations are non-deterministic across replays",
						Suggestion: "Workflow code is single-threaded by design. Remove the synchronization primitive -- it is not needed.",
						Line:       line,
					})
				}
			}
		}
	}

	var code, msg, suggestion string

	switch {
	case pkgPath == "time" && selName == "Now":
		code, msg, suggestion = "E003",
			"time.Now() returns wall-clock time which differs across replays, breaking determinism",
			"Use h.Now() for deterministic time."

	case pkgPath == "time" && selName == "Sleep":
		code, msg, suggestion = "E004",
			"time.Sleep() uses real wall-clock time; use h.DurableSleep() for replay-safe delays",
			"Use h.DurableSleep() instead."

	case pkgPath == "time" && (selName == "After" || selName == "NewTicker" || selName == "NewTimer"):
		code, msg, suggestion = "E014",
			"time.After/NewTicker/NewTimer create goroutines internally, which are non-deterministic",
			"Use h.DurableSleep() for deterministic delays."

	case pkgPath == "net/http" || strings.HasPrefix(pkgPath, "net/http/"):
		code, msg, suggestion = "E005",
			"direct net/http calls are non-deterministic (network failures, DNS, timeouts vary)",
			"Use h.DurableCall() with a service name instead."

	case pkgPath == "database/sql":
		code, msg, suggestion = "E006",
			"direct database/sql calls produce non-replayable side effects",
			"Use h.DurableCall() with a service name instead."

	case pkgPath == "math/rand":
		code, msg, suggestion = "E007",
			"math/rand default seed depends on wall-clock time, producing different results on replay",
			"Use h.Random() for deterministic randomness."

	case pkgPath == "math/rand/v2":
		code, msg, suggestion = "E018",
			"math/rand/v2 default seeding is time-based, producing different sequences on replay",
			"Use h.Random() for deterministic randomness."

	case pkgPath == "sync/atomic":
		code, msg, suggestion = "E013",
			"sync/atomic operations are non-deterministic across replays",
			"Workflow code is single-threaded by design. Use local variables instead of atomic operations."

	case pkgPath == "fmt" && (selName == "Print" || selName == "Printf" || selName == "Println" ||
		selName == "Fprint" || selName == "Fprintf" || selName == "Fprintln"):
		code, msg, suggestion = "E015",
			"fmt/log output goes to stdout/stderr which is not captured reliably during replay",
			"Use h.DurableLog() for deterministic logging."

	case pkgPath == "log" && (selName == "Print" || selName == "Printf" || selName == "Println" ||
		selName == "Fatal" || selName == "Fatalf" || selName == "Fatalln" ||
		selName == "Panic" || selName == "Panicf" || selName == "Panicln"):
		code, msg, suggestion = "E015",
			"fmt/log output goes to stdout/stderr which is not captured reliably during replay",
			"Use h.DurableLog() for deterministic logging."

	case pkgPath == "os" && (selName == "Getenv" || selName == "Environ"):
		code, msg, suggestion = "E016",
			"os."+selName+"() is not allowed in cleat functions",
			"Environment variables may differ across replays. Pass configuration via workflow input."

	case pkgPath == "os" && selName == "Exit":
		code, msg, suggestion = "E016",
			"os.Exit() is not allowed in cleat functions",
			"os.Exit terminates the WASM runtime; return an error instead."

	case pkgPath == "crypto/rand":
		code, msg, suggestion = "E017",
			"crypto/rand reads from OS entropy sources, producing non-deterministic values",
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
	// Only flag interface types that are NOT cleat.HostCalls.
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
		Message:    "calls through interfaces cannot be statically resolved, so the analyzer cannot verify the callee",
		Suggestion: "Use concrete types or refactor to avoid interface dispatch in cleat functions.",
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
		Message:    "function-value calls cannot be statically resolved; the analyzer cannot trace the call chain",
		Suggestion: "Replace with a direct function call or inline the logic.",
		Line:       line,
	})
}

// checkForbiddenPackageRef detects references to packages that are forbidden
// in cleat functions (os, reflect).
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
			"os package operations (files, env, processes) differ across replays and are not allowed",
			"Use h.DurableCall() with a service name instead of direct OS operations."
	case pkgPath == "reflect":
		code, msg, suggestion = "E011",
			"reflect results can differ across Go versions or build targets, breaking determinism",
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
			Code:       "W002",
			FuncName:   funcName,
			Message:    "floating-point value in control flow condition may cause non-deterministic replay",
			Suggestion: "Replace float comparisons with integer arithmetic, or compare using math.Float64bits() for exact bitwise equality.",
			Line:       line,
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
	// Fallback: match by explicit import name (unambiguous).
	for _, file := range fd.Pkg.Files {
		for _, imp := range file.Imports {
			importPath := strings.Trim(imp.Path.Value, `"`)
			if imp.Name != nil && imp.Name.Name == name {
				return importPath
			}
		}
	}
	// Fallback: match by last path component. Only return when exactly
	// one import matches, to avoid ambiguity (e.g., rand could be
	// crypto/rand or math/rand).
	var lastComponentMatch string
	matchCount := 0
	for _, file := range fd.Pkg.Files {
		for _, imp := range file.Imports {
			importPath := strings.Trim(imp.Path.Value, `"`)
			if analyzer.LastComponent(importPath) == name {
				lastComponentMatch = importPath
				matchCount++
			}
		}
	}
	if matchCount == 1 {
		return lastComponentMatch
	}
	return ""
}

// resolveCallFQName resolves a call expression to a fully-qualified function name
// using type information. Returns empty string if the callee cannot be resolved.
func resolveCallFQName(call *ast.CallExpr, info *types.Info) string {
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

// validateInitFunctions checks that init() functions in the target package
// do not call any function in the durable closure or durable leaves set.
// Durable calls must happen inside workflow entry points, not in init().
func validateInitFunctions(result *analyzer.AnalysisResult, cr *Result) {
	// Build the durable call set.
	durableSet := make(map[string]bool)
	for name := range cr.DurableLeaves {
		durableSet[name] = true
	}
	for name := range cr.DurableClosure {
		durableSet[name] = true
	}
	if len(durableSet) == 0 {
		return
	}

	for _, fd := range result.Funcs {
		if fd.Name != "init" || fd.RecvType != nil {
			continue
		}
		if fd.Ast.Body == nil {
			continue
		}

		var fset *token.FileSet
		if fd.Pkg != nil {
			fset = fd.Pkg.Fset
		}

		funcName := fd.FullyQualifiedName()
		ast.Inspect(fd.Ast.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			callee := resolveCallFQName(call, fd.Pkg.Info)
			if callee == "" {
				return true
			}
			if durableSet[callee] {
				line := 0
				if fset != nil {
					line = fset.Position(call.Pos()).Line
				}
				cr.Errors[funcName] = append(cr.Errors[funcName], ValidationError{
					Code:       "E020",
					FuncName:   funcName,
					Message:    "init() functions cannot make durable calls — durable calls must happen inside workflow entry points",
					Suggestion: "Move durable calls from init() into the workflow entry point function.",
					Line:       line,
				})
				return false
			}
			return true
		})
	}
}
