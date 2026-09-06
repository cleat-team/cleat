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
	if globalHObj != nil {
		// Declared here rather than above the branch. The empty map it used to
		// be initialised with outside was never read: the only reads are inside
		// this branch, and the first statement in it overwrote the value. So
		// the initialisation was doing nothing except making the variable look
		// like it had a meaning in the nil case, which it does not.
		usesGlobalH := findGlobalHUsers(result, globalHObj)
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

	// Phase 3b: Functions with a PARAMETER whose type carries a HostCalls
	// field.
	//
	// The same rule as phase 3, applied to parameters instead of receivers, and
	// symmetric with it rather than a widening: a function that is handed a
	// *TaskContext holding a cleat.HostCalls can reach the host exactly as a
	// method on that struct can.
	//
	// This is cleat/dagrun's designed shape, and its package doc says so:
	//
	//	// TaskContext.H is passed through to every user-written task body ...
	//	// That means TaskContext cannot be narrowed to a small interface
	//	// without breaking real callers (see examples/dag, which calls
	//	// ctx.H.DurableCall).
	//
	// Without this phase, `cleat vet` rejected the pattern a first-party cleat
	// SDK package documents itself as requiring, and examples/dag failed with
	// four errors telling its author to add a parameter it already effectively
	// has. IMPROVEMENT-PLAN 3.229.
	for name := range durableSet {
		if threaded[name] {
			continue
		}
		fd := result.Funcs[name]
		if fd == nil || fd.Type == nil {
			continue
		}
		params := fd.Type.Params()
		if params == nil {
			continue
		}
		for i := 0; i < params.Len(); i++ {
			if structHasHostCallsField(params.At(i).Type(), fd.Pkg) {
				threaded[name] = true
				break
			}
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

	errors = append(errors, verifyEntryPointResults(result)...)
	warnEntryPointTakesRawInput(result, cr)

	return errors
}

// warnEntryPointTakesRawInput warns about an entry point whose only
// parameter, after the HostCalls one, is a single string.
//
// Such a parameter receives the WHOLE input JSON, not the field matching its
// name. Every other shape binds by exact Go parameter name. That is deliberate
// -- something has to be able to carry an opaque payload -- so this is a
// warning and not an error, and the rule itself is unchanged.
//
// What it costs when unwanted is the reason for the warning. The rule is
// invisible at the call site, at build time and at deploy; it surfaces as a
// semantic failure in whatever the parameter was eventually used for, which is
// by construction somewhere else. Measured in cleat-team/cleat-ports:
//
//	func HandleLockTry(h cleat.HostCalls, key string) (string, error) {
//		acquired, err := h.AcquireLockMs("lock"+key, 120000)
//
// started with {"key": "lock-abc"} took the lock
// `lock-{"key":"lock-abc"}`, and every acquire failed with
// `cleat_acquire_lock: error 1`. Nothing in that message points at argument
// binding, and the natural reading is that locks are broken -- while a
// near-identical two-parameter workflow acquired the same key correctly in the
// same run. It has cost time three times: a detached-execution test, that lock
// test, and a cron target whose scheduled runs completed successfully having
// called the wrong service key. cleat#824.
func warnEntryPointTakesRawInput(result *analyzer.AnalysisResult, cr *Result) {
	if cr == nil || cr.Warnings == nil {
		return
	}
	for _, name := range result.EntryPoints {
		fd := result.Funcs[name]
		if fd == nil || fd.Type == nil {
			continue
		}
		params := fd.Type.Params()
		if params == nil {
			continue
		}
		// Skip a leading HostCalls parameter, which is not bound from the
		// input at all.
		start := 0
		if params.Len() > 0 && analyzer.IsHostCallsType(params.At(0).Type()) {
			start = 1
		}
		if params.Len()-start != 1 {
			continue
		}
		p := params.At(start)
		basic, ok := p.Type().Underlying().(*types.Basic)
		if !ok || basic.Kind() != types.String {
			continue
		}
		line := 0
		if fd.Pkg != nil && fd.Pkg.Fset != nil && fd.Ast != nil {
			line = fd.Pkg.Fset.Position(fd.Ast.Pos()).Line
		}
		cr.Warnings[name] = append(cr.Warnings[name], ValidationWarning{
			Code:     "W003",
			FuncName: name,
			Message: fmt.Sprintf(
				"%s is a workflow entry point whose only parameter is a single string, "+
					"so %q receives the ENTIRE input JSON rather than the field of that name. "+
					"Starting it with {%q: \"value\"} binds %s to the literal text {%q:\"value\"}.",
				analyzer.ShortName(name), p.Name(), p.Name(), p.Name(), p.Name()),
			Suggestion: "If that is what you want -- an opaque payload the workflow parses " +
				"itself -- nothing needs to change. If you meant to bind one field by name, " +
				fmt.Sprintf("add a second parameter or take a struct: func(h cleat.HostCalls, %s string, tag string). ", p.Name()) +
				"Struct parameters are unmarshalled from the input JSON and bind by field.",
			Line: line,
		})
	}
}

// verifyEntryPointResults rejects an entry point whose result value is not a
// string.
//
// THE STRING IS DELIBERATE, not a codegen limitation. A WASM entry point hands
// back bytes, and `string` is the one shape every language SDK expresses
// identically -- which is why the interfaces use it. GenerateExports therefore
// declares `var __r string` (wasm/exports.go) and emits `return []byte(__r)`,
// and supports exactly four signatures:
//
//	func(h cleat.HostCalls, ...) (string, error)
//	func(h cleat.HostCalls, ...) error
//	func(h cleat.HostCalls, ...) string
//	func(h cleat.HostCalls, ...)
//
// Anything else compiled until now, and then failed like this:
//
//	./gen_wasm_exports.go:340:28: cannot convert __r (variable of type
//	    *BookingResult) to type []byte
//
// -- a Go type error in GENERATED code, naming a variable the author never
// wrote and a file they did not create. `cleat vet` said OK on the same
// package. Three of the shipped examples are in that state
// (IMPROVEMENT-PLAN 3.228), which is how it went unnoticed: nothing in CI runs
// cleat build on a Go example.
//
// This rejects nothing that previously built. A non-string result already
// failed, later and less legibly.
//
// Note it is only the RESULT. Struct PARAMETERS are fine and common --
// examples/subscription takes a SubscriptionInput and builds -- because the
// generator unmarshals those from the args JSON.
func verifyEntryPointResults(result *analyzer.AnalysisResult) []ThreadingError {
	var errs []ThreadingError
	for _, name := range result.EntryPoints {
		fd := result.Funcs[name]
		if fd == nil || fd.Type == nil {
			continue
		}
		res := fd.Type.Results()
		if res == nil {
			continue
		}
		for i := 0; i < res.Len(); i++ {
			t := res.At(i).Type()
			if isErrorType(t) {
				continue
			}
			if basic, ok := t.Underlying().(*types.Basic); ok && basic.Kind() == types.String {
				continue
			}
			line := 0
			if fd.Pkg != nil && fd.Pkg.Fset != nil && fd.Ast != nil {
				line = fd.Pkg.Fset.Position(fd.Ast.Pos()).Line
			}
			errs = append(errs, ThreadingError{
				FuncName: name,
				Line:     line,
				Message: fmt.Sprintf(
					"%s is a workflow entry point returning %s, but an entry point's result must be a string. "+
						"A WASM entry point hands back bytes, and string is the shape every language SDK expresses "+
						"identically. Return (string, error) -- marshal the value yourself -- or error alone. "+
						"Struct parameters are fine; it is only the result.",
					analyzer.ShortName(name), t.String()),
			})
			break
		}
	}
	return errs
}

// isErrorType reports whether t is the builtin error interface.
func isErrorType(t types.Type) bool {
	named, ok := t.(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	return obj != nil && obj.Pkg() == nil && obj.Name() == "error"
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
