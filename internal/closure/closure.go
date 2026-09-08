// Package closure computes the transitive closure of cleat functions,
// validates supported Go constructs, and verifies HostCalls threading.
package closure

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"sort"
	"strings"

	"github.com/cleat-team/cleat/internal/analyzer"
	"github.com/cleat-team/cleat/internal/callgraph"
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

	wasmCache := make(map[string]bool)

	// Validate supported constructs in every function the workflow executes.
	// Functions defined in files that would be excluded for the WASM target
	// (GOOS=wasip1 GOARCH=wasm) are skipped — they won't appear in the
	// compiled WASM module and may legitimately use non-deterministic
	// platform APIs.
	//
	// The set validated here is NOT the durable closure, and conflating the two
	// is cleat#949. Closure analysis asks "which functions reach a host call",
	// because that decides which host functions to import. Determinism asks
	// "what does this workflow EXECUTE", and replay re-runs the whole workflow
	// body -- it constrains the results of host calls, not local computation.
	// A helper that makes no host call is Pure by the first question and fully
	// in scope for the second.
	//
	// The two traversals run in opposite directions. DurableClosure is upward:
	// callers of anything durable. What determinism needs is downward: callees
	// of anything durable. nonDeterministicHelper in
	// testdata/vet-checks/go/helper_escape is Pure, is called by a durable
	// entry point, carries six violations across E001/E002/E013, and was
	// counted by the analyzer without being checked by it.
	//
	// Tagging is deliberately left alone. Pure still means "reaches no host
	// call", which is the right answer to the import question and what codegen
	// consumes; only the validation set is widened.
	for name := range durableReachable(result, cg, cr) {
		fd, ok := result.Funcs[name]
		if !ok {
			// Not a function this analysis loaded -- stdlib or a dependency.
			// Nothing to inspect, and its determinism is not ours to police
			// here; the forbidden-call checks above cover the ones that matter.
			continue
		}
		// Files excluded for the WASM target (GOOS=wasip1 GOARCH=wasm) do not
		// appear in the compiled module and may legitimately use
		// non-deterministic platform APIs.
		if !isWASEligible(fd, wasmCache) {
			continue
		}
		validateConstructs(fd, cr)
	}

	// Validate that init() functions do not call durable functions.
	validateInitFunctions(result, cr)

	return cr
}

// durableReachable returns every function the workflow can execute: the
// durable ones, plus everything reachable downward from them through the call
// graph.
//
// This is the determinism scope, and it is strictly larger than the durable
// closure. See the comment at its call site in Compute for why the two differ
// (cleat#949).
//
// Cycles are ordinary in workflow code -- mutual recursion between helpers --
// so this is a worklist with a visited set rather than recursion. A function
// name that is not in result.Funcs is still traversed FROM, because the call
// graph may know edges for a name whose declaration this analysis did not
// load; the caller decides what to do about a name it cannot inspect.
func durableReachable(result *analyzer.AnalysisResult, cg *callgraph.Graph, cr *Result) map[string]bool {
	seen := make(map[string]bool, len(result.Funcs))
	var queue []string

	push := func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		queue = append(queue, name)
	}

	for name := range cr.DurableLeaves {
		push(name)
	}
	for name := range cr.DurableClosure {
		push(name)
	}

	for len(queue) > 0 {
		cur := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		for callee := range cg.Calls[cur] {
			push(callee)
		}
	}
	return seen
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

	// PluginCaller methods are trusted boundaries — skip validation.
	if fd.RecvType != nil && analyzer.ImplementsPluginCaller(fd.RecvType) {
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
					if mapType, isMap := tv.Type.Underlying().(*types.Map); isMap {
						line := 0
						pos := ""
						if fset != nil {
							p := fset.Position(stmt.Pos())
							line = p.Line
							pos = p.String()
						}
						typeStr := fmt.Sprintf("map[%s]%s", mapType.Key(), mapType.Elem())
						cr.Errors[name] = append(cr.Errors[name], ValidationError{
							Code:       "E021",
							FuncName:   name,
							Message:    fmt.Sprintf("non-deterministic map iteration at %s: ranging over %s of type %s produces non-deterministic iteration order", pos, typeStr, typeStr),
							Suggestion: fmt.Sprintf("collect keys via slices.Sorted(maps.Keys(%s)), then iterate over sorted keys", typeStr),
							Line:       line,
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
	if analyzer.ImplementsPluginCaller(tv.Type) {
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
	if v, isVar := obj.(*types.Var); isVar {
		// Check if the variable's type implements PluginCaller.
		if analyzer.ImplementsPluginCaller(v.Type()) {
			return
		}
	} else {
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
// Floats in control flow conditions can cause non-deterministic replay because
// IEEE 754 compliance does not guarantee identical NaN payloads or denormal
// handling across all hardware. See docs/determinism.md for details.
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
			Suggestion: "Floating-point comparisons can produce different results across hardware or during replay due to NaN payload variation and denormal handling. Replace float comparisons with integer arithmetic, or compare exact bit patterns using math.Float64bits() / math.Float32bits(). See docs/determinism.md for details.",
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
			return analyzer.FuncFQName(fn)
		}
	case *ast.SelectorExpr:
		if sel, ok := info.Selections[fun]; ok {
			if fn, ok := sel.Obj().(*types.Func); ok {
				return analyzer.FuncFQName(fn)
			}
		}
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

// isWASEligible reports whether the function's source file would be
// compiled for the WASM target (GOOS=wasip1 GOARCH=wasm). Functions in
// files that are build-constrained out (e.g. *_linux.go, *_amd64.go, or
// files with //go:build constraints that exclude wasip1/wasm) are not
// eligible and should be skipped during validation.
//
// cache maps source file path -> eligibility, avoiding repeated I/O when
// multiple functions live in the same file.
func isWASEligible(fd *analyzer.FuncDecl, cache map[string]bool) bool {
	if fd.Pkg == nil || fd.Pkg.Fset == nil || fd.Ast == nil {
		return true // can't determine position; err on the side of validation
	}

	filename := fd.Pkg.Fset.Position(fd.Ast.Pos()).Filename
	if filename == "" || filename == "-" {
		return true
	}

	// Check cache.
	if eligible, ok := cache[filename]; ok {
		return eligible
	}

	// Quick check: filename suffix (no I/O needed).
	if analyzer.FilenameConstrainedOut(filename) {
		cache[filename] = false
		return false
	}

	// Read the file to check //go:build constraints.
	content, err := os.ReadFile(filename)
	if err != nil {
		// Can't read the file — let the validation proceed (safe side).
		cache[filename] = true
		return true
	}

	ok, err := analyzer.MatchWasmBuildConstraint(filename, content)
	if err != nil {
		// Parse error — let validation proceed.
		cache[filename] = true
		return true
	}

	cache[filename] = ok
	return ok
}

// ---------------------------------------------------------------------------
// Ordered access to diagnostics (cleat#965)
// ---------------------------------------------------------------------------

// Diagnostic is one finding paired with the function it was reported in.
//
// Errors and Warnings are keyed by function name, and Go randomises map
// iteration, so every consumer that ranged over them directly produced a
// different ordering on every run. Four output modes did: the human report, the
// GitHub Actions annotations, the JSON output, and the summary. `cleat vet` on
// an unchanged package could not be diffed against itself, which is the
// property a linter is most often asked for -- no golden file, no baseline, no
// before/after review of what an edit did to the diagnostics.
//
// It is also the tool whose job is rejecting non-determinism, and map iteration
// order is one of its own error codes (E021).
type Diagnostic struct {
	FuncName   string
	Code       string
	Message    string
	Suggestion string
	Line       int
}

// SortedErrors returns every validation error in a stable, total order.
//
// Use this rather than ranging over Errors. The maps stay exported because the
// analysis consumes them; ordering belongs to presentation.
func (cr *Result) SortedErrors() []Diagnostic {
	out := make([]Diagnostic, 0, len(cr.Errors))
	for funcName, errs := range cr.Errors {
		for _, e := range errs {
			out = append(out, Diagnostic{
				FuncName:   funcName,
				Code:       e.Code,
				Message:    e.Message,
				Suggestion: e.Suggestion,
				Line:       e.Line,
			})
		}
	}
	sortDiagnostics(out)
	return out
}

// SortedWarnings returns every validation warning in a stable, total order.
func (cr *Result) SortedWarnings() []Diagnostic {
	out := make([]Diagnostic, 0, len(cr.Warnings))
	for funcName, warns := range cr.Warnings {
		for _, w := range warns {
			out = append(out, Diagnostic{
				FuncName:   funcName,
				Code:       w.Code,
				Message:    w.Message,
				Suggestion: w.Suggestion,
				Line:       w.Line,
			})
		}
	}
	sortDiagnostics(out)
	return out
}

// sortDiagnostics orders by function, then line, then code, then message.
//
// All four keys are needed for a TOTAL order, which is the point: two findings
// that compare equal on every key would be free to swap between runs, and the
// ordering would be stable only by luck. One function can report the same code
// on the same line twice -- e013_sync_mutex does, for Lock and Unlock on one
// line -- so func+line+code alone is not enough.
func sortDiagnostics(d []Diagnostic) {
	sort.Slice(d, func(i, j int) bool {
		a, b := d[i], d[j]
		switch {
		case a.FuncName != b.FuncName:
			return a.FuncName < b.FuncName
		case a.Line != b.Line:
			return a.Line < b.Line
		case a.Code != b.Code:
			return a.Code < b.Code
		default:
			return a.Message < b.Message
		}
	})
}
