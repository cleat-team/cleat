package analyzer

// cleat#3803, cleat#3805. Which host calls does a function from ANOTHER package make on the workflow's behalf?
//
// The build finds a workflow's host calls by looking in the workflow's own source for a method on HostCalls.
// A call that sits inside an SDK (or third-party) function the workflow merely invokes is invisible to that:
// the module builds, links no import for it, and fails at run time with "the HostCalls runtime was not
// initialized" -- or, when the function that holds `h` is itself not seen as using host calls, with a nil
// dereference, because nothing threaded `h` into it.
//
// This used to be answered by two hand lists keyed by "Type.Method" (sdkDurableHelpers here and wasm's
// sdkHelperImports), kept in step by a test. A list of names has three holes and all three were hit: it names
// only METHODS (a package-level function such as cleat.NewPromiseTyped has no key at all), it names only
// receivers in a package called "cleat", and a method on a GENERIC type never reached it -- the scan that
// kept the lists honest could not read a receiver of the form Promise[T]. Each hole built clean.
//
// This reads the function's body instead. For any function declared in a dependency package it computes the
// HostCalls methods that function can reach, following calls to other dependency functions, so there is no
// name to forget to add and no receiver shape to forget to handle.

import (
	"go/ast"
	"go/token"
	"go/types"
	"sort"
	"sync"
)

// DepSummary is what one dependency function can reach.
type DepSummary struct {
	// Methods are the names of the HostCalls methods reachable from the function, sorted.
	Methods []string
	// PluginCall is true when it reaches a method of a PluginCaller type, which is a trusted boundary that
	// makes plugin_call on the caller's behalf.
	PluginCall bool
	// Opaque lists the places the walk met a call it cannot follow while holding the host-call runtime, so
	// that "reaches no host call" would be a guess rather than a finding.
	Opaque []OpaqueCall
}

// Empty reports whether the function reaches nothing at all, opaque places included.
func (s *DepSummary) Empty() bool {
	return s == nil || (len(s.Methods) == 0 && !s.PluginCall && len(s.Opaque) == 0)
}

// OpaqueCall is a call inside a dependency that hands the host-call runtime to code the build cannot read.
type OpaqueCall struct {
	Pos    string // file:line:col of the call, inside the dependency
	Callee string // the call as written
	Why    string
}

// depDirect is what one function's own body shows, before following any call out of it.
type depDirect struct {
	methods    map[string]bool
	pluginCall bool
	callees    map[string]bool
	opaque     []OpaqueCall
}

type depDecl struct {
	decl *ast.FuncDecl
	pkg  *Package
}

// depIndex is built once per AnalysisResult, on first use.
type depIndex struct {
	once    sync.Once
	mu      sync.Mutex
	decls   map[string]*depDecl
	direct  map[string]*depDirect
	summary map[string]*DepSummary
}

// DepHostCalls returns what fn reaches, or nil when fn is not declared in a dependency package the build
// has source for (the target module's own functions are read directly, and the standard library makes no
// host call).
//
// A generic function or method is read through its origin (FuncFQName resolves it), so an instantiation such as
// NewPromiseTyped[string] and a method on Promise[string] resolve to the one declaration.
func (r *AnalysisResult) DepHostCalls(fn *types.Func) *DepSummary {
	if r == nil || fn == nil || fn.Pkg() == nil {
		return nil
	}
	r.dep.once.Do(r.buildDepIndex)

	key := FuncFQName(fn)
	r.dep.mu.Lock()
	defer r.dep.mu.Unlock()
	if s, ok := r.dep.summary[key]; ok {
		return s
	}
	if r.dep.decls[key] == nil {
		return nil
	}

	// Union over everything reachable. A per-node memo would be wrong under recursion (a partial result
	// cached inside a cycle), and the dependency call graph is small enough that a walk per queried function
	// is cheap.
	methods := map[string]bool{}
	var opaque []OpaqueCall
	plugin := false
	seen := map[string]bool{key: true}
	queue := []string{key}
	for len(queue) > 0 {
		k := queue[0]
		queue = queue[1:]
		d := r.dep.directOf(k)
		if d == nil {
			continue
		}
		for m := range d.methods {
			methods[m] = true
		}
		plugin = plugin || d.pluginCall
		opaque = append(opaque, d.opaque...)
		for c := range d.callees {
			if !seen[c] && r.dep.decls[c] != nil {
				seen[c] = true
				queue = append(queue, c)
			}
		}
	}

	s := &DepSummary{PluginCall: plugin, Opaque: opaque}
	for m := range methods {
		s.Methods = append(s.Methods, m)
	}
	sort.Strings(s.Methods)
	sort.Slice(s.Opaque, func(i, j int) bool { return s.Opaque[i].Pos < s.Opaque[j].Pos })
	if s.Empty() {
		s = nil
	}
	r.dep.summary[key] = s
	return s
}

func (r *AnalysisResult) buildDepIndex() {
	r.dep.decls = map[string]*depDecl{}
	r.dep.direct = map[string]*depDirect{}
	r.dep.summary = map[string]*DepSummary{}
	for _, pkg := range r.ImportedPkgs {
		if pkg == nil || pkg.Info == nil {
			continue
		}
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				obj, ok := pkg.Info.Defs[fd.Name].(*types.Func)
				if !ok {
					continue
				}
				r.dep.decls[FuncFQName(obj)] = &depDecl{decl: fd, pkg: pkg}
			}
		}
	}
}

// directOf reads one function's own body. The caller holds mu.
func (ix *depIndex) directOf(key string) *depDirect {
	if d, ok := ix.direct[key]; ok {
		return d
	}
	decl := ix.decls[key]
	if decl == nil {
		return nil
	}
	d := &depDirect{methods: map[string]bool{}, callees: map[string]bool{}}
	info := decl.pkg.Info
	note := func(fn *types.Func) {
		if fn == nil || fn.Pkg() == nil {
			return // a universe method such as error.Error
		}
		d.callees[FuncFQName(fn)] = true
	}
	ast.Inspect(decl.decl.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if sel, ok := info.Selections[x]; ok {
				switch {
				case HostCallsMethod(sel):
					d.methods[x.Sel.Name] = true
				case PluginCallerMethod(sel):
					d.pluginCall = true
				default:
					if fn, ok := sel.Obj().(*types.Func); ok {
						note(fn)
					}
				}
			} else if fn, ok := info.Uses[x.Sel].(*types.Func); ok {
				note(fn) // a package-qualified function, pkg.Func
			}
		case *ast.Ident:
			if fn, ok := info.Uses[x].(*types.Func); ok {
				note(fn)
			}
		case *ast.CallExpr:
			if o := opaqueCall(x, decl.pkg); o != nil {
				d.opaque = append(d.opaque, *o)
			}
		}
		return true
	})
	ix.direct[key] = d
	return d
}

// opaqueCall reports a call through an interface method, other than HostCalls and PluginCaller, that is handed
// a HostCalls: the implementation that runs is not knowable, so what it does with the runtime is not either.
//
// A call through a FUNCTION VALUE is deliberately not flagged. The common shape is a callback the workflow
// wrote (Saga.AddStep's closure takes the HostCalls it is given), and the workflow's own body is read in full.
func opaqueCall(call *ast.CallExpr, pkg *Package) *OpaqueCall {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || pkg.Info == nil {
		return nil
	}
	s, ok := pkg.Info.Selections[sel]
	if !ok || HostCallsMethod(s) || PluginCallerMethod(s) {
		return nil
	}
	if _, isIface := s.Recv().Underlying().(*types.Interface); !isIface {
		return nil
	}
	for _, arg := range call.Args {
		if tv, ok := pkg.Info.Types[arg]; ok && IsHostCallsType(tv.Type) {
			return &OpaqueCall{
				Pos:    pkg.Fset.Position(call.Pos()).String(),
				Callee: types.ExprString(call.Fun),
				Why:    "it calls a method on an interface and passes it the host-call runtime, so which code runs is not known",
			}
		}
	}
	return nil
}

// DepRef is one reference, in a function of the target module, to a dependency function that reaches host calls.
type DepRef struct {
	Fn      *types.Func
	Pos     token.Pos
	Summary *DepSummary
}

// DepReferences lists the references in fd's body to dependency functions whose summary is not empty.
//
// A REFERENCE, not only a call: `f := cleat.NewPromiseTyped[string]` makes the same host call later, and a
// scan that required call.Fun would miss it. Over-reporting costs an import the workflow did not need;
// under-reporting costs a run that fails.
func (r *AnalysisResult) DepReferences(fd *FuncDecl) []DepRef {
	if r == nil || fd == nil || fd.Ast == nil || fd.Ast.Body == nil || fd.Pkg == nil || fd.Pkg.Info == nil {
		return nil
	}
	info := fd.Pkg.Info
	var refs []DepRef
	add := func(fn *types.Func, pos token.Pos) {
		if s := r.DepHostCalls(fn); s != nil {
			refs = append(refs, DepRef{Fn: fn, Pos: pos, Summary: s})
		}
	}
	ast.Inspect(fd.Ast.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if sel, ok := info.Selections[x]; ok {
				if HostCallsMethod(sel) || PluginCallerMethod(sel) {
					return true // read by the ordinary scan
				}
				if fn, ok := sel.Obj().(*types.Func); ok {
					add(fn, x.Pos())
				}
			} else if fn, ok := info.Uses[x.Sel].(*types.Func); ok {
				add(fn, x.Pos())
			}
		case *ast.Ident:
			if fn, ok := info.Uses[x].(*types.Func); ok {
				add(fn, x.Pos())
			}
		}
		return true
	})
	return refs
}
