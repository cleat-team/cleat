package closure

import (
	"go/token"
	"testing"

	"github.com/cleat-team/cleat/internal/analyzer"
	"github.com/cleat-team/cleat/internal/callgraph"
)

// ---------------------------------------------------------------------------
// VerifyThreading
// ---------------------------------------------------------------------------

func TestVerifyThreadingPassesForBasic(t *testing.T) {
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/cleat-team/cleat/testdata/basic", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}

	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("Build callgraph failed: %v", err)
	}

	cr := Compute(result, cg)

	threadingErrors := VerifyThreading(result, cg, cr)
	if len(threadingErrors) > 0 {
		for _, e := range threadingErrors {
			t.Errorf("unexpected threading error for %s: %s (chain: %v)",
				e.FuncName, e.Message, e.Chain)
		}
	}
}

func TestVerifyThreadingDetectsUnthreadedHelper(t *testing.T) {
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/cleat-team/cleat/testdata/errors", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}

	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("Build callgraph failed: %v", err)
	}

	cr := Compute(result, cg)

	threadingErrors := VerifyThreading(result, cg, cr)

	target := "github.com/cleat-team/cleat/testdata/errors.unthreadedHelper"
	found := false
	for _, e := range threadingErrors {
		if e.FuncName == target {
			found = true
			if e.Line <= 0 {
				t.Errorf("expected non-zero line number for %s", target)
			}
			if len(e.Chain) == 0 {
				t.Error("expected non-empty call chain")
			}
			break
		}
	}
	if !found {
		t.Errorf("expected threading error for %s, got %d error(s):", target, len(threadingErrors))
		for _, e := range threadingErrors {
			t.Logf("  %s: %s", e.FuncName, e.Message)
		}
	}
}

func TestVerifyThreadingDoesNotReportBadWorkflowAsError(t *testing.T) {
	// BadWorkflow has h cleat.HostCalls as a first parameter, so even
	// though it calls unthreadedHelper, it itself is properly threaded.
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/cleat-team/cleat/testdata/errors", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}

	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("Build callgraph failed: %v", err)
	}

	cr := Compute(result, cg)

	threadingErrors := VerifyThreading(result, cg, cr)

	badWorkflowName := "github.com/cleat-team/cleat/testdata/errors.BadWorkflow"
	for _, e := range threadingErrors {
		if e.FuncName == badWorkflowName {
			t.Errorf("BadWorkflow should not have a threading error (it has h param): %s", e.Message)
		}
	}
}

func TestVerifyThreadingDoesNotReportPureFunctions(t *testing.T) {
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/cleat-team/cleat/testdata/errors", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}

	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("Build callgraph failed: %v", err)
	}

	cr := Compute(result, cg)

	threadingErrors := VerifyThreading(result, cg, cr)

	pureName := "github.com/cleat-team/cleat/testdata/errors.pureHelper"
	for _, e := range threadingErrors {
		if e.FuncName == pureName {
			t.Errorf("pureHelper should not have a threading error (not in closure): %s", e.Message)
		}
	}
}

// ---------------------------------------------------------------------------
// findGlobalHostCalls
// ---------------------------------------------------------------------------

func TestFindGlobalHostCallsFindsVarHInAutothread(t *testing.T) {
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/cleat-team/cleat/testdata/autothread", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}

	obj := findGlobalHostCalls(result)
	if obj == nil {
		t.Fatal("expected to find global var h, got nil")
	}
	if obj.Name() != "h" {
		t.Errorf("expected name 'h', got %q", obj.Name())
	}
}

func TestFindGlobalHostCallsReturnsNilForBasic(t *testing.T) {
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/cleat-team/cleat/testdata/basic", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}

	obj := findGlobalHostCalls(result)
	if obj != nil {
		t.Errorf("expected nil, got global var named %q", obj.Name())
	}
}

func TestFindGlobalHostCallsReturnsNilForErrors(t *testing.T) {
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/cleat-team/cleat/testdata/errors", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}

	obj := findGlobalHostCalls(result)
	if obj != nil {
		t.Errorf("expected nil, got global var named %q", obj.Name())
	}
}

// ---------------------------------------------------------------------------
// VerifyThreading with autothread (global h pattern)
// ---------------------------------------------------------------------------

func TestVerifyThreadingAutothreadReportsPassThroughErrors(t *testing.T) {
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

	threadingErrors := VerifyThreading(result, cg, cr)

	// Functions that directly use the global var h (or have h as a param)
	// should NOT be reported.
	directlyThreaded := map[string]bool{
		"github.com/cleat-team/cleat/testdata/autothread.PlaceOrder":              true,
		"github.com/cleat-team/cleat/testdata/autothread.CancelOrder":             true,
		"github.com/cleat-team/cleat/testdata/autothread.checkItemAvailability":   true,
		"github.com/cleat-team/cleat/testdata/autothread.getDefaultPaymentMethod": true,
		"github.com/cleat-team/cleat/testdata/autothread.fulfillOrder":            true,
		"github.com/cleat-team/cleat/testdata/autothread.reserveInventory":        true,
		"github.com/cleat-team/cleat/testdata/autothread.chargeCustomer":          true,
		"github.com/cleat-team/cleat/testdata/autothread.releaseReservation":      true,
		"github.com/cleat-team/cleat/testdata/autothread.refundPayment":           true,
		"github.com/cleat-team/cleat/testdata/autothread.notifyCustomer":          true,
	}
	for _, e := range threadingErrors {
		if directlyThreaded[e.FuncName] {
			t.Errorf("%s should not be reported as unthreaded (directly uses global h or has h param): %s",
				e.FuncName, e.Message)
		}
	}

	// validateAndReserve and processPayment are pass-through functions in
	// the closure that don't reference the global var h directly, so they
	// are correctly reported as unthreaded BEFORE the transform runs. After
	// the transform they get h added as a parameter.
	expectedUnthreaded := map[string]bool{
		"github.com/cleat-team/cleat/testdata/autothread.validateAndReserve": true,
		"github.com/cleat-team/cleat/testdata/autothread.processPayment":     true,
	}

	for name := range expectedUnthreaded {
		found := false
		for _, e := range threadingErrors {
			if e.FuncName == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected threading error for pass-through function %s", name)
		}
	}
}
