package closure

import (
	"go/ast"
	"go/token"
	"go/types"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/internal/analyzer"
	"github.com/cleat-team/cleat/internal/callgraph"
)

// ---------------------------------------------------------------------------
// ValidationError.Error() format
// ---------------------------------------------------------------------------

func TestValidationErrorFormat(t *testing.T) {
	t.Run("with suggestion and line", func(t *testing.T) {
		e := ValidationError{
			Code:       "E001",
			FuncName:   "pkg.Func",
			Message:    "goroutines are bad",
			Suggestion: "Use child workflows",
			Line:       42,
		}
		s := e.Error()
		if !strings.Contains(s, "E001") {
			t.Errorf("expected E001 in error string, got: %s", s)
		}
		if !strings.Contains(s, "goroutines are bad") {
			t.Errorf("expected message in error string, got: %s", s)
		}
		if !strings.Contains(s, "suggestion: Use child workflows") {
			t.Errorf("expected suggestion in error string, got: %s", s)
		}
		if !strings.Contains(s, "line 42") {
			t.Errorf("expected line number in error string, got: %s", s)
		}
	})

	t.Run("without suggestion", func(t *testing.T) {
		e := ValidationError{
			Code:     "E002",
			FuncName: "pkg.Func",
			Message:  "bad thing",
			Line:     0,
		}
		s := e.Error()
		if !strings.Contains(s, "E002") {
			t.Errorf("expected E002 in error string, got: %s", s)
		}
		if strings.Contains(s, "suggestion") {
			t.Errorf("did not expect suggestion in error string, got: %s", s)
		}
		if strings.Contains(s, "line") {
			t.Errorf("did not expect line number in error string, got: %s", s)
		}
	})
}

// ---------------------------------------------------------------------------
// NumErrors / NumWarnings
// ---------------------------------------------------------------------------

func TestNumErrorsAndWarnings(t *testing.T) {
	cr := &Result{
		Errors:   make(map[string][]ValidationError),
		Warnings: make(map[string][]ValidationWarning),
	}

	if n := cr.NumErrors(); n != 0 {
		t.Errorf("expected 0 errors, got %d", n)
	}
	if n := cr.NumWarnings(); n != 0 {
		t.Errorf("expected 0 warnings, got %d", n)
	}

	cr.Errors["f1"] = []ValidationError{
		{Code: "E001", Message: "err1"},
		{Code: "E002", Message: "err2"},
	}
	cr.Errors["f2"] = []ValidationError{
		{Code: "E003", Message: "err3"},
	}
	cr.Warnings["f1"] = []ValidationWarning{
		{Code: "W001", Message: "warn1"},
	}

	if n := cr.NumErrors(); n != 3 {
		t.Errorf("expected 3 errors, got %d", n)
	}
	if n := cr.NumWarnings(); n != 1 {
		t.Errorf("expected 1 warning, got %d", n)
	}
}

// ---------------------------------------------------------------------------
// hasHostCallsParam tested through testdata/basic
// ---------------------------------------------------------------------------

func TestHasHostCallsParam(t *testing.T) {
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/cleat-team/cleat/testdata/basic", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}

	// PlaceOrder has h cleat.HostCalls as first param.
	placeOrder := result.Funcs["github.com/cleat-team/cleat/testdata/basic.PlaceOrder"]
	if placeOrder == nil {
		t.Fatal("PlaceOrder not found in Funcs")
	}
	if !hasHostCallsParam(placeOrder) {
		t.Error("PlaceOrder should have HostCalls param")
	}

	// pure functions like notifyCustomer (wait, it's a leaf)...
	// Actually let's check a known pure function.
	// checkItemAvailability should have h as first param.
	checkItem := result.Funcs["github.com/cleat-team/cleat/testdata/basic.checkItemAvailability"]
	if checkItem == nil {
		t.Fatal("checkItemAvailability not found")
	}
	if !hasHostCallsParam(checkItem) {
		t.Error("checkItemAvailability should have HostCalls param")
	}
}

// ---------------------------------------------------------------------------
// resolveImportPath via testdata (exercises all branches)
// ---------------------------------------------------------------------------

func TestResolveImportPath(t *testing.T) {
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/cleat-team/cleat/testdata/basic", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}

	fd := result.Funcs["github.com/cleat-team/cleat/testdata/basic.PlaceOrder"]
	if fd == nil || fd.Pkg == nil {
		t.Fatal("PlaceOrder not found with package info")
	}

	// Test with valid fd and known import identifiers.
	// This exercises the fallback path (last component matching) in resolveImportPath.
	for _, file := range fd.Pkg.Files {
		for _, imp := range file.Imports {
			importPath := strings.Trim(imp.Path.Value, `"`)
			ident := &ast.Ident{Name: analyzer.LastComponent(importPath)}
			resolved := resolveImportPath(fd, ident)
			t.Logf("resolveImportPath(%s) = %s (expected %s)", ident.Name, resolved, importPath)
		}
	}
}

// ---------------------------------------------------------------------------
// resolveCallFQName edge cases
// ---------------------------------------------------------------------------

func TestResolveCallFQNameEdgeCases(t *testing.T) {
	// nil info returns empty.
	if got := resolveCallFQName(nil, nil); got != "" {
		t.Errorf("expected empty for nil, got %s", got)
	}

	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/cleat-team/cleat/testdata/basic", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}

	// nil info with nil call returns empty.
	if got := resolveCallFQName(nil, nil); got != "" {
		t.Errorf("expected empty for nil+info, got %s", got)
	}

	// CallExpr with an ident that doesn't resolve in info.
	if result.TargetPkg.Info != nil {
		identCall := &ast.CallExpr{Fun: &ast.Ident{Name: "nonExistentFunc"}}
		if got := resolveCallFQName(identCall, result.TargetPkg.Info); got != "" {
			t.Errorf("expected empty for unresolvable call, got %s", got)
		}
	}
}

// ---------------------------------------------------------------------------
// findCallChain edge cases
// ---------------------------------------------------------------------------

func TestFindCallChainEdgeCases(t *testing.T) {
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/cleat-team/cleat/testdata/basic", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}

	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("Build callgraph failed: %v", err)
	}

	// Target that does not exist in graph.
	chain := findCallChain("nonexistent", result.EntryPoints, cg)
	if chain != nil {
		t.Errorf("expected nil chain for non-existent function, got %v", chain)
	}

	// Target is an entry point itself.
	ep := "github.com/cleat-team/cleat/testdata/basic.PlaceOrder"
	chain = findCallChain(ep, result.EntryPoints, cg)
	if chain == nil {
		t.Fatal("expected chain for PlaceOrder")
	}
	if len(chain) < 1 {
		t.Errorf("expected at least 1 step in chain, got %d", len(chain))
	}
}

// ---------------------------------------------------------------------------
// dfsChain with visited detection (circular edge case)
// ---------------------------------------------------------------------------

func TestDfsChainVisited(t *testing.T) {
	// Create a minimal graph with a cycle.
	cg := &callgraph.Graph{
		Calls: map[string]map[string]bool{
			"a": {"b": true},
			"b": {"c": true},
			"c": {"a": true}, // cycle back to a
		},
	}

	// Target not reachable from "a" (but a->b->c->a is a cycle).
	chain := dfsChain("a", "d", cg, nil)
	if chain != nil {
		t.Errorf("expected nil for unreachable target with cycle, got %v", chain)
	}

	// Reachable target "c" from "a".
	chain = dfsChain("a", "c", cg, nil)
	if chain == nil {
		t.Fatal("expected chain from a to c")
	}
	if len(chain) < 2 {
		t.Errorf("expected at least 2 steps from a to c, got %d: %v", len(chain), chain)
	}
}

// ---------------------------------------------------------------------------
// VerifyThreading with empty durable set
// ---------------------------------------------------------------------------

func TestVerifyThreadingEmptyDurableSet(t *testing.T) {
	// Result with no functions in the durable set.
	result := &analyzer.AnalysisResult{
		Funcs:      make(map[string]*analyzer.FuncDecl),
		TargetPkg:  &analyzer.Package{Info: nil, Files: nil},
	}
	cg := &callgraph.Graph{
		Calls:    make(map[string]map[string]bool),
		CalledBy: make(map[string]map[string]bool),
	}
	cr := &Result{
		DurableLeaves:  make(map[string]bool),
		DurableClosure: make(map[string]bool),
		Pure:           make(map[string]bool),
		Errors:         make(map[string][]ValidationError),
		Warnings:       make(map[string][]ValidationWarning),
	}

	errors := VerifyThreading(result, cg, cr)
	if len(errors) != 0 {
		t.Errorf("expected 0 threading errors for empty durable set, got %d", len(errors))
	}
}

// ---------------------------------------------------------------------------
// Struct receiver with HostCalls field edge cases
// ---------------------------------------------------------------------------

func TestStructHasHostCallsField(t *testing.T) {
	// Non-pointer, non-named type returns false.
	if structHasHostCallsField(types.Typ[types.Int], nil) {
		t.Error("expected false for basic type")
	}
}

// ---------------------------------------------------------------------------
// Result tags consistency with FuncDecl across multiple packages
// ---------------------------------------------------------------------------

func TestResultTagsConsistency(t *testing.T) {
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/cleat-team/cleat/testdata/autothread", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}

	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("Build callgraph failed: %v", err)
	}

	cr := Compute(result, cg)

	// Every function should have a DurabilityTag set.
	for name, fd := range result.Funcs {
		if fd.DurabilityTag == "" {
			t.Errorf("function %s has empty DurabilityTag", name)
		}
		if fd.IsDurableLeaf && fd.DurabilityTag != "DurableLeaf" {
			t.Errorf("function %s has IsDurableLeaf=true but tag=%s", name, fd.DurabilityTag)
		}
		if fd.InDurableClosure && fd.DurabilityTag != "DurableClosure" {
			t.Errorf("function %s has InDurableClosure=true but tag=%s", name, fd.DurabilityTag)
		}
	}

	// Verify the Result matches the FuncDecl metadata.
	totalTagged := len(cr.DurableLeaves) + len(cr.DurableClosure) + len(cr.Pure)
	if totalTagged != len(result.Funcs) {
		t.Errorf("tagged count (%d) != func count (%d)", totalTagged, len(result.Funcs))
	}
}

// ---------------------------------------------------------------------------
// Error() method on ValidationWarning is not present, but ensure
// ValidationError.Error() includes all parts when all fields are set
// ---------------------------------------------------------------------------

func TestValidationErrorFullFormat(t *testing.T) {
	e := ValidationError{
		Code:       "E020",
		FuncName:   "mypkg.init",
		Message:    "init cannot make durable calls",
		Suggestion: "move to entry point",
		Line:       10,
	}
	s := e.Error()

	if !strings.HasPrefix(s, "E020: ") {
		t.Errorf("expected prefix 'E020: ', got: %s", s)
	}
	if !strings.Contains(s, "suggestion: move to entry point") {
		t.Errorf("expected suggestion in output")
	}
	if !strings.Contains(s, "line 10") {
		t.Errorf("expected line number in output")
	}
}

// ---------------------------------------------------------------------------
// Vet-checks: forbidden calls via Compute
// ---------------------------------------------------------------------------

func TestVetChecksForbiddenNow(t *testing.T) {
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/cleat-team/cleat/testdata/vet-checks/go/e003_time_now", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}
	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("Build callgraph: %v", err)
	}
	cr := Compute(result, cg)
	// Should have E003 (time.Now forbidden)
	if !hasErrorCode(cr, "E003") {
		t.Errorf("expected E003 (time.Now forbidden), got errors: %v", listErrorCodes(cr))
	}
}

func TestVetChecksForbiddenNetHTTP(t *testing.T) {
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/cleat-team/cleat/testdata/vet-checks/go/e005_net_http", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}
	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("Build callgraph: %v", err)
	}
	cr := Compute(result, cg)
	if !hasErrorCode(cr, "E005") {
		t.Errorf("expected E005 (net/http forbidden), got errors: %v", listErrorCodes(cr))
	}
}

func TestVetChecksForbiddenMathRand(t *testing.T) {
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/cleat-team/cleat/testdata/vet-checks/go/e007_math_rand", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}
	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("Build callgraph: %v", err)
	}
	cr := Compute(result, cg)
	if !hasErrorCode(cr, "E007") {
		t.Errorf("expected E007 (math/rand forbidden), got errors: %v", listErrorCodes(cr))
	}
}

func TestVetChecksForbiddenSyncMutex(t *testing.T) {
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/cleat-team/cleat/testdata/vet-checks/go/e013_sync_mutex", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}
	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("Build callgraph: %v", err)
	}
	cr := Compute(result, cg)
	if !hasErrorCode(cr, "E013") {
		t.Errorf("expected E013 (sync.Mutex forbidden), got errors: %v", listErrorCodes(cr))
	}
}

func TestVetChecksForbiddenFmtPrintln(t *testing.T) {
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/cleat-team/cleat/testdata/vet-checks/go/e015_fmt_println", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}
	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("Build callgraph: %v", err)
	}
	cr := Compute(result, cg)
	if !hasErrorCode(cr, "E015") {
		t.Errorf("expected E015 (fmt.Println forbidden), got errors: %v", listErrorCodes(cr))
	}
}

func TestVetChecksNoErrors(t *testing.T) {
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/cleat-team/cleat/testdata/vet-checks/go/no_errors", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}
	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("Build callgraph: %v", err)
	}
	cr := Compute(result, cg)
	if cr.NumErrors() != 0 {
		t.Errorf("expected 0 errors for clean code, got %d: %v", cr.NumErrors(), listErrorCodes(cr))
	}
}

// hasErrorCode checks if any function has an error with the given code.
func hasErrorCode(cr *Result, code string) bool {
	for _, errs := range cr.Errors {
		for _, e := range errs {
			if e.Code == code {
				return true
			}
		}
	}
	return false
}

// listErrorCodes returns a list of all error codes for debugging.
func listErrorCodes(cr *Result) []string {
	var codes []string
	for _, errs := range cr.Errors {
		for _, e := range errs {
			codes = append(codes, e.Code)
		}
	}
	return codes
}

// ---------------------------------------------------------------------------
// Vet-checks: goroutine detection via Compute (exercises validateConstructs)
// ---------------------------------------------------------------------------

func TestVetChecksGoroutine(t *testing.T) {
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/cleat-team/cleat/testdata/vet-checks/go/e001_goroutine", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}
	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("Build callgraph: %v", err)
	}
	cr := Compute(result, cg)
	if !hasErrorCode(cr, "E001") {
		t.Errorf("expected E001 (goroutine forbidden), got errors: %v", listErrorCodes(cr))
	}
}

// ---------------------------------------------------------------------------
// hasHostCallsParam edge cases
// ---------------------------------------------------------------------------

func TestHasHostCallsParamEdgeCases(t *testing.T) {
	// Nil fd.Type returns false.
	fd := &analyzer.FuncDecl{Type: nil}
	if hasHostCallsParam(fd) {
		t.Error("expected false for nil Type")
	}

	// Type with nil params returns false.
	params := types.NewTuple()
	// Can't easily create a types.Signature with nil params inline, skip.
	_ = params
}

// ---------------------------------------------------------------------------
// resolveNamedType edge cases
// ---------------------------------------------------------------------------

func TestResolveNamedTypeEdgeCases(t *testing.T) {
	// Non-named, non-pointer type returns nil.
	if got := resolveNamedType(types.Typ[types.Int]); got != nil {
		t.Errorf("expected nil for basic type, got %v", got)
	}

	// Pointer to non-named type returns nil.
	ptr := types.NewPointer(types.Typ[types.Int])
	if got := resolveNamedType(ptr); got != nil {
		t.Errorf("expected nil for pointer to basic type, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// structHasHostCallsField edge cases
// ---------------------------------------------------------------------------

func TestStructHasHostCallsFieldEdgeCases(t *testing.T) {
	// Non-pointer, non-named type returns false.
	if structHasHostCallsField(types.Typ[types.Int], nil) {
		t.Error("expected false for basic type")
	}

	// Pointer to non-named type returns false.
	ptr := types.NewPointer(types.Typ[types.Int])
	if structHasHostCallsField(ptr, nil) {
		t.Error("expected false for pointer to basic type")
	}

	// Pointer to named non-struct type returns false.
	named := types.NewNamed(types.NewTypeName(0, nil, "MyInt", types.Typ[types.Int]), types.Typ[types.Int], nil)
	ptr = types.NewPointer(named)
	if structHasHostCallsField(ptr, nil) {
		t.Error("expected false for pointer to named non-struct")
	}
}

// ---------------------------------------------------------------------------
// Vet-checks: channel operations (E002)
// ---------------------------------------------------------------------------

func TestVetChecksChannelOps(t *testing.T) {
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/cleat-team/cleat/testdata/vet-checks/go/e002_channel", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}
	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("Build callgraph: %v", err)
	}
	cr := Compute(result, cg)
	if !hasErrorCode(cr, "E002") {
		t.Errorf("expected E002 (channel forbidden), got errors: %v", listErrorCodes(cr))
	}
	if !hasErrorCode(cr, "E001") {
		t.Errorf("expected E001 (goroutine), got errors: %v", listErrorCodes(cr))
	}
}

// ---------------------------------------------------------------------------
// Vet-checks: os.Getenv + reflect (E010/E011)
// ---------------------------------------------------------------------------

func TestVetChecksForbiddenPackageRef(t *testing.T) {
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/cleat-team/cleat/testdata/vet-checks/go/e016_os_reflect", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}
	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("Build callgraph: %v", err)
	}
	cr := Compute(result, cg)
	if !hasErrorCode(cr, "E010") {
		t.Errorf("expected E010 (os package ref), got errors: %v", listErrorCodes(cr))
	}
	if !hasErrorCode(cr, "E011") {
		t.Errorf("expected E011 (reflect package ref), got errors: %v", listErrorCodes(cr))
	}
}

// ---------------------------------------------------------------------------
// Vet-checks: init function with durable call (E020)
// ---------------------------------------------------------------------------

func TestVetChecksInitFunction(t *testing.T) {
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/cleat-team/cleat/testdata/vet-checks/go/e009_init", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}
	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("Build callgraph: %v", err)
	}
	cr := Compute(result, cg)
	if !hasErrorCode(cr, "E020") {
		t.Errorf("expected E020 (init durable call), got errors: %v", listErrorCodes(cr))
	}
}
