// Package analyzer loads Go packages, resolves types, and builds the
// internal representation used by the rest of the transformer pipeline.
package analyzer

import (
	"go/ast"
	"go/token"
	"go/types"
)

// Package represents a loaded Go package with its ASTs and type information.
type Package struct {
	Name  string
	Path  string
	Dir   string
	Files []*ast.File
	Fset  *token.FileSet
	Types *types.Package
	Info  *types.Info
}

// FuncDecl represents a function or method with its resolved type information.
type FuncDecl struct {
	Name     string           // simple name (e.g. "PlaceOrder")
	Pkg      *Package         // the package this function belongs to
	Ast      *ast.FuncDecl    // the AST node
	Type     *types.Signature // the type-checked signature
	RecvType types.Type       // non-nil for methods (the receiver type)

	// IsExported is true for capitalized function names.
	IsExported bool

	// IsEntryPoint is true if this function is a workflow entry point
	// (exported, first param is cleat.HostCalls, in root of target package).
	IsEntryPoint bool

	// IsDurableLeaf is true if this function directly calls a HostCalls method.
	IsDurableLeaf bool

	// InDurableClosure is true if this function transitively calls a cleat leaf.
	InDurableClosure bool

	// DurabilityTag is one of "DurableLeaf", "DurableClosure", or "Pure".
	DurabilityTag string

	// AutoThreaded is true if the transform added h cleat.HostCalls as
	// the first parameter to this function.
	AutoThreaded bool
}

// AnalysisResult holds the complete analysis of a workflow package.
type AnalysisResult struct {
	TargetPkg *Package
	UserPkgs  []*Package

	// ImportedPkgs are the non-stdlib packages the build depends on,
	// transitively, EXCLUDING TargetPkg.
	//
	// It exists for //cleat:require. That directive names host calls a package
	// makes on the caller's behalf, and collectRequirements read it from
	// TargetPkg.Files alone -- the workflow's own package. LoadPackages only
	// retains the packages that matched the build pattern, so a directive in
	// ANY imported package was silently ignored: cleat/dagrun's, and equally a
	// user's own helper package in their own module. The failure is silent and
	// late -- the module builds, deploys, and dies on the first call with "the
	// HostCalls runtime was not initialized" (cleat#1617).
	//
	// Non-stdlib rather than cleat-module-only, deliberately: a user's helper
	// package is not in cleat's module and has exactly the same problem. A
	// directive is opt-in, can only add imports that already exist in the
	// hostFunctions table, and there is no way for it to remove one.
	ImportedPkgs []*Package
	Funcs        map[string]*FuncDecl // keyed by fully-qualified name
	EntryPoints  []string             // fully-qualified names of entry points

	// Module information.
	ModulePath string // e.g., "github.com/cleat-team/cleat"
	ModuleDir  string // absolute path to module root (where go.mod lives)
	GoVersion  string // e.g., "1.26" from go.mod

	// Statistics
	NumFuncs          int
	NumExported       int
	NumDurableLeaves  int
	NumDurableClosure int
	NumPure           int
}

// FullyQualifiedName returns the fully-qualified name for a function,
// e.g. "workflows.PlaceOrder" or "(*workflows.OrderProcessor).Process".
func (f *FuncDecl) FullyQualifiedName() string {
	if f.RecvType != nil {
		return types.TypeString(f.RecvType, qualifier(f.Pkg)) + "." + f.Name
	}
	return f.Pkg.Path + "." + f.Name
}

func qualifier(pkg *Package) types.Qualifier {
	return func(other *types.Package) string {
		if other.Path() == pkg.Path {
			return ""
		}
		return other.Name()
	}
}

// ---- HostCalls type helpers ----

// IsHostCallsType reports whether t is cleat.HostCalls (interface) or
// *cleat.HostCalls (pointer to struct, for backward compatibility).
func IsHostCallsType(t types.Type) bool {
	// cleat.HostCalls as interface (passed by value).
	if named, ok := t.(*types.Named); ok {
		obj := named.Obj()
		if obj.Name() == "HostCalls" && obj.Pkg() != nil && obj.Pkg().Name() == "cleat" {
			return true
		}
	}
	// *cleat.HostCalls as pointer to struct (backward compatibility).
	if ptr, ok := t.(*types.Pointer); ok {
		if named, ok := ptr.Elem().(*types.Named); ok {
			obj := named.Obj()
			if obj.Name() == "HostCalls" && obj.Pkg() != nil && obj.Pkg().Name() == "cleat" {
				return true
			}
		}
	}
	return false
}

// HostCallsMethod reports whether the given selection is a method call
// on a HostCalls value.
func HostCallsMethod(sel *types.Selection) bool {
	if sel == nil {
		return false
	}
	return IsHostCallsType(sel.Recv())
}

// sdkDurableHelpers is the set of "Type.Method" names SDKDurableHelper
// accepts -- the keys the predicate below looks up.
// THE SET IS THE SOURCE OF TRUTH, and the predicate reads it. It used to be a
// switch inside the predicate, which made the accepted names unaddressable by
// name -- and they need to be addressable, because the imports these helpers
// require live in a SECOND hand list (wasm's sdkHelperImports) that must agree
// with this one. Neither direction of disagreement fails loudly: a name here
// with no row there contributes no import, and a row there with no name here is
// never consulted at all. In both cases the module builds, the call reaches a
// nil adapter, and it returns a zero value at run time. cleat#2627 was the
// first change that had to update both lists in lockstep, and only one of the
// two was covered by a test -- so TestEverySDKHelperRowIsAcceptedByTheGate
// (wasm) now checks them against each other through SDKDurableHelperName.
var sdkDurableHelpers = map[string]bool{
	"Saga.AddStepCall": true,

	// Selector.Select is the SECOND of these and it was not found the way
	// Saga.AddStepCall was. Saga.AddStepCall was reasoned about while the
	// feature was being written; this one shipped, and a cleat.Selector timer
	// fired instantly in every compiled workflow that did not independently
	// write h.DurableSleep -- for as long as the type has existed.
	//
	// Measured on 8ca97d46 through the ports harness, two guest packages that
	// differ by exactly one line:
	//
	//	no h.DurableSleep in the package: generation 1, 87ms, a 1500ms timer
	//	                                  fired, durable clock advanced 0ms
	//	one h.DurableSleep in the package: generation 2, the timer waited, the
	//	                                  durable clock advanced exactly 1500ms
	//
	// Same Selector, same deadline. The variable is whether the WORKFLOW's own
	// source happens to mention the host call the SDK makes on its behalf.
	"Selector.Select": true,

	// AddTimer is here for Now() and looks harmless next to Select. It is not:
	// an unwired Now() returns 0, so every deadline this Selector computes is
	// measured from the epoch and is already in the past. A Select that sleeps
	// correctly would then fire immediately anyway, for a second reason, and
	// fixing only Select would have looked like fixing nothing.
	"Selector.AddTimer": true,

	// Saga.Run and Saga.RunWithResult share one body -- Run delegates to
	// RunWithResult (cleat#2627) -- so both routes make the same host calls no
	// workflow wrote: LogKV, and SetQueryState, which is how a saga now
	// publishes its progress. The steps' calls are closures the workflow wrote,
	// so every other layer already sees them.
	//
	// BOTH KEYS ARE LOAD-BEARING and neither implies the other: the analyzer
	// sees the method the WORKFLOW writes, so saga.Run(h) consults one row and
	// saga.RunWithResult(h) the other.
	"Saga.Run":           true,
	"Saga.RunWithResult": true,
}

// SDKDurableHelper reports whether a selection is an SDK helper that makes a
// durable host call on the caller's behalf, rather than a HostCalls method the
// workflow wrote itself.
//
// Saga.AddStepCall is the first of these (cleat#1131). It takes a StepCall --
// data -- and builds the DurableCall closures inside the SDK, which is the
// point: it is the only form that can be PARAMETERISED without tripping E009
// or the durable-leaf check. But three separate layers decide what a workflow
// does by looking for HostCalls methods in workflow code, and none of them
// could see it:
//
//	callgraph.hasHostCallsCall  -> not a durable leaf
//	closure analysis            -> not in the durable closure, so...
//	wasm.collectHostCallsCalls  -> never scanned, so no cleat_call import
//
// The module then built clean, imported nothing, and would have failed at RUN
// time on its first step -- the cleat#1005 symptom reached from the opposite
// direction. One predicate, asked by all three, keeps them from disagreeing.
//
// Saga.AddStep needs no entry: the caller writes the closure and its
// h.DurableCall is visible to every layer already.
func SDKDurableHelper(sel *types.Selection) bool {
	if sel == nil {
		return false
	}
	t := sel.Recv()
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok || named.Obj() == nil || named.Obj().Pkg() == nil {
		return false
	}
	if named.Obj().Pkg().Name() != "cleat" {
		return false
	}
	return SDKDurableHelperName(named.Obj().Name() + "." + sel.Obj().Name())
}

// SDKDurableHelperName reports whether "Type.Method" names an SDK helper whose
// durable host calls the analyzer has to import on the workflow's behalf.
//
// It takes the key rather than a *types.Selection for one reason: the gate and
// the import table it decides whether to consult (wasm's sdkHelperImports) are
// TWO hand lists that have to agree, and agreeing is only checkable by name. A
// row whose key this refuses is never read, so it contributes no import, and
// the module builds and returns a zero value at run time. cleat#2627 was the
// first change that had to update both lists in lockstep.
func SDKDurableHelperName(key string) bool {
	return sdkDurableHelpers[key]
}

// PluginCallerMethod reports whether the given selection is a method call
// on a type that implements the cleat.PluginCaller marker interface.
func PluginCallerMethod(sel *types.Selection) bool {
	if sel == nil {
		return false
	}
	return ImplementsPluginCaller(sel.Recv())
}

// ImplementsPluginCaller reports whether the given type implements the
// cleat.PluginCaller marker interface (has a cleatPlugin() method with
// no parameters and no return values).
func ImplementsPluginCaller(t types.Type) bool {
	named := resolveNamedType(t)
	if named == nil {
		return false
	}
	// Check *T — the cleatPlugin method is defined with a pointer receiver.
	ptr := types.NewPointer(named)
	ms := types.NewMethodSet(ptr)
	for i := 0; i < ms.Len(); i++ {
		m := ms.At(i).Obj()
		if m.Name() == "cleatPlugin" {
			sig, ok := m.Type().(*types.Signature)
			return ok && sig.Params().Len() == 0 && sig.Results().Len() == 0
		}
	}
	// Also check value receiver methods (for completeness).
	msVal := types.NewMethodSet(named)
	for i := 0; i < msVal.Len(); i++ {
		m := msVal.At(i).Obj()
		if m.Name() == "cleatPlugin" {
			sig, ok := m.Type().(*types.Signature)
			return ok && sig.Params().Len() == 0 && sig.Results().Len() == 0
		}
	}
	return false
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

// FuncFQName returns the fully-qualified name of a *types.Func in the same
// format used as keys in AnalysisResult.Funcs. For generic instantiations,
// it uses Origin() to return the type-parameter form (e.g., "*Container[T].Process"
// instead of "(*Container[string]).Process").
func FuncFQName(fn *types.Func) string {
	// Use Origin() for generic instantiations to get the type-parameter form.
	if origin := fn.Origin(); origin != nil {
		fn = origin
	}
	sig := fn.Type().(*types.Signature)
	if sig.Recv() != nil {
		recvType := sig.Recv().Type()
		return types.TypeString(recvType, func(other *types.Package) string {
			if other.Path() == fn.Pkg().Path() {
				return ""
			}
			return other.Name()
		}) + "." + fn.Name()
	}
	return fn.Pkg().Path() + "." + fn.Name()
}

// ShortName returns the short function name from a fully-qualified name.
// "pkg.Func" → "Func", "(*pkg.Type).Method" → "Method".
func ShortName(fqname string) string {
	// Method name like "(*pkg.Type).Method"
	for i := len(fqname) - 1; i >= 0; i-- {
		if fqname[i] == '.' {
			return fqname[i+1:]
		}
	}
	return fqname
}

// LastComponent returns the last component of a path (after the last '/').
func LastComponent(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[i+1:]
		}
	}
	return path
}

// ContainsNode reports whether the AST subtree root contains the target node.
func ContainsNode(root, target ast.Node) bool {
	found := false
	ast.Inspect(root, func(n ast.Node) bool {
		if n == target {
			found = true
			return false
		}
		return !found
	})
	return found
}

// FindEnclosingFuncName returns the simple name of the function containing
// the given AST node, or "".
func FindEnclosingFuncName(files []*ast.File, target ast.Node) string {
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if ContainsNode(fn.Body, target) {
				return fn.Name.Name
			}
		}
	}
	return ""
}
