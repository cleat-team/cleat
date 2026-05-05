package closure

import (
	"go/token"
	"testing"

	"github.com/rcownie/durable/internal/analyzer"
	"github.com/rcownie/durable/internal/callgraph"
)

// basicFQ builds a fully-qualified name for a function in the
// testdata/basic package.
func basicFQ(name string) string {
	return "github.com/rcownie/durable/testdata/basic." + name
}

func TestComputeBasicIdentifiesDurableLeaves(t *testing.T) {
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/rcownie/durable/testdata/basic", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}

	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("Build callgraph failed: %v", err)
	}

	cr := Compute(result, cg)

	// All eight functions that call h.DurableCall directly.
	expectedLeaves := map[string]bool{
		basicFQ("checkItemAvailability"):   true,
		basicFQ("getDefaultPaymentMethod"): true,
		basicFQ("fulfillOrder"):            true,
		basicFQ("reserveInventory"):        true,
		basicFQ("chargeCustomer"):          true,
		basicFQ("releaseReservation"):      true,
		basicFQ("refundPayment"):           true,
		basicFQ("notifyCustomer"):          true,
	}

	for name := range expectedLeaves {
		if !cr.DurableLeaves[name] {
			t.Errorf("expected %s to be a durable leaf", name)
		}
	}

	// No unexpected leaves.
	for name := range cr.DurableLeaves {
		if !expectedLeaves[name] {
			t.Errorf("unexpected durable leaf: %s", name)
		}
	}

	if len(cr.DurableLeaves) != len(expectedLeaves) {
		t.Errorf("expected %d durable leaves, got %d", len(expectedLeaves), len(cr.DurableLeaves))
	}
}

func TestComputeBasicIdentifiesDurableClosure(t *testing.T) {
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/rcownie/durable/testdata/basic", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}

	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("Build callgraph failed: %v", err)
	}

	cr := Compute(result, cg)

	// Functions that transitively reach a durable leaf.
	expectedClosure := map[string]bool{
		basicFQ("PlaceOrder"):          true,
		basicFQ("CancelOrder"):         true,
		basicFQ("validateAndReserve"):  true,
		basicFQ("processPayment"):      true,
	}

	for name := range expectedClosure {
		if !cr.DurableClosure[name] {
			t.Errorf("expected %s to be in durable closure", name)
		}
	}

	// No unexpected closure entries.
	for name := range cr.DurableClosure {
		if !expectedClosure[name] {
			t.Errorf("unexpected durable closure: %s", name)
		}
	}

	if len(cr.DurableClosure) != len(expectedClosure) {
		t.Errorf("expected %d durable closure, got %d", len(expectedClosure), len(cr.DurableClosure))
	}
}

func TestComputeBasicCorrectlyTagsPureFunctions(t *testing.T) {
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/rcownie/durable/testdata/basic", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}

	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("Build callgraph failed: %v", err)
	}

	cr := Compute(result, cg)

	// Total functions = 12 (8 leaves + 4 closure), so pure should be empty.
	totalFuncs := len(result.Funcs)
	durableCount := len(cr.DurableLeaves) + len(cr.DurableClosure)
	pureCount := len(cr.Pure)

	if totalFuncs != 12 {
		t.Errorf("expected 12 functions, got %d", totalFuncs)
	}

	if durableCount+pureCount != totalFuncs {
		t.Errorf(
			"durable (%d) + pure (%d) = %d, expected %d",
			durableCount, pureCount, durableCount+pureCount, totalFuncs,
		)
	}
}

func TestComputeBasicTagsAreConsistentWithFuncDecl(t *testing.T) {
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/rcownie/durable/testdata/basic", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}

	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("Build callgraph failed: %v", err)
	}

	cr := Compute(result, cg)

	for name := range cr.DurableLeaves {
		fd := result.Funcs[name]
		if fd == nil {
			t.Errorf("leaf %s not found in Funcs", name)
			continue
		}
		if !fd.IsDurableLeaf {
			t.Errorf("leaf %s has IsDurableLeaf=false", name)
		}
		if fd.DurabilityTag != "DurableLeaf" {
			t.Errorf("leaf %s has DurabilityTag=%q", name, fd.DurabilityTag)
		}
	}

	for name := range cr.DurableClosure {
		fd := result.Funcs[name]
		if fd == nil {
			t.Errorf("closure %s not found in Funcs", name)
			continue
		}
		if !fd.InDurableClosure {
			t.Errorf("closure %s has InDurableClosure=false", name)
		}
		if fd.DurabilityTag != "DurableClosure" {
			t.Errorf("closure %s has DurabilityTag=%q", name, fd.DurabilityTag)
		}
	}

	for name := range cr.Pure {
		fd := result.Funcs[name]
		if fd == nil {
			continue
		}
		if fd.DurabilityTag != "Pure" {
			t.Errorf("pure %s has DurabilityTag=%q", name, fd.DurabilityTag)
		}
	}
}

func TestComputeErrorsDetectsGoroutine(t *testing.T) {
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/rcownie/durable/testdata/errors", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}

	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("Build callgraph failed: %v", err)
	}

	cr := Compute(result, cg)

	badName := "github.com/rcownie/durable/testdata/errors.BadWithGoroutine"

	errs := cr.Errors[badName]
	if len(errs) == 0 {
		t.Fatalf("expected validation errors for %s, got none", badName)
	}

	foundE001 := false
	for _, e := range errs {
		if e.Code == "E001" {
			foundE001 = true
			break
		}
	}
	if !foundE001 {
		t.Errorf("expected E001 (goroutine) error for %s, got codes: ", badName)
		for _, e := range errs {
			t.Logf("  %s: %s", e.Code, e.Message)
		}
	}
}

func TestComputeErrorsDetectsDurableLeaves(t *testing.T) {
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/rcownie/durable/testdata/errors", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}

	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("Build callgraph failed: %v", err)
	}

	cr := Compute(result, cg)

	expectedLeaves := map[string]bool{
		"github.com/rcownie/durable/testdata/errors.leafFunc":                   true,
		"github.com/rcownie/durable/testdata/errors.BadWithGoroutine":           true,
		"github.com/rcownie/durable/testdata/errors.BadWithInterfaceDispatch":   true,
		"github.com/rcownie/durable/testdata/errors.BadWithFuncValue":           true,
		"github.com/rcownie/durable/testdata/errors.BadWithFloatCondition":      true,
	}

	for name := range expectedLeaves {
		if !cr.DurableLeaves[name] {
			t.Errorf("expected %s to be a durable leaf", name)
		}
	}

	// No unexpected leaves.
	for name := range cr.DurableLeaves {
		if !expectedLeaves[name] {
			t.Errorf("unexpected durable leaf: %s", name)
		}
	}

	if len(cr.DurableLeaves) != len(expectedLeaves) {
		t.Errorf("expected %d durable leaves, got %d", len(expectedLeaves), len(cr.DurableLeaves))
	}
}

func TestComputeErrorsDetectsClosure(t *testing.T) {
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/rcownie/durable/testdata/errors", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}

	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("Build callgraph failed: %v", err)
	}

	cr := Compute(result, cg)

	expectedClosure := map[string]bool{
		"github.com/rcownie/durable/testdata/errors.BadWorkflow":      true,
		"github.com/rcownie/durable/testdata/errors.unthreadedHelper": true,
	}

	for name := range expectedClosure {
		if !cr.DurableClosure[name] {
			t.Errorf("expected %s to be in durable closure", name)
		}
	}
}

func TestComputeErrorsCorrectlyTagsPure(t *testing.T) {
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/rcownie/durable/testdata/errors", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}

	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("Build callgraph failed: %v", err)
	}

	cr := Compute(result, cg)

	pureName := "github.com/rcownie/durable/testdata/errors.pureHelper"
	if !cr.Pure[pureName] {
		t.Errorf("expected %s to be pure", pureName)
	}
}

func TestComputeErrorsDetectsInterfaceDispatch(t *testing.T) {
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/rcownie/durable/testdata/errors", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}

	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("Build callgraph failed: %v", err)
	}

	cr := Compute(result, cg)

	badName := "github.com/rcownie/durable/testdata/errors.BadWithInterfaceDispatch"

	warns := cr.Warnings[badName]
	if len(warns) == 0 {
		t.Fatalf("expected W003 warning for %s, got none", badName)
	}

	foundW003 := false
	for _, w := range warns {
		if w.Code == "W003" {
			foundW003 = true
			break
		}
	}
	if !foundW003 {
		t.Errorf("expected W003 (interface dispatch) warning for %s, got codes: ", badName)
		for _, w := range warns {
			t.Logf("  %s: %s", w.Code, w.Message)
		}
	}
}

func TestComputeErrorsDetectsFuncValueCall(t *testing.T) {
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/rcownie/durable/testdata/errors", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}

	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("Build callgraph failed: %v", err)
	}

	cr := Compute(result, cg)

	badName := "github.com/rcownie/durable/testdata/errors.BadWithFuncValue"

	warns := cr.Warnings[badName]
	if len(warns) == 0 {
		t.Fatalf("expected W004 warning for %s, got none", badName)
	}

	foundW004 := false
	for _, w := range warns {
		if w.Code == "W004" {
			foundW004 = true
			break
		}
	}
	if !foundW004 {
		t.Errorf("expected W004 (func value call) warning for %s, got codes: ", badName)
		for _, w := range warns {
			t.Logf("  %s: %s", w.Code, w.Message)
		}
	}
}

func TestComputeErrorsDetectsFloatInCondition(t *testing.T) {
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/rcownie/durable/testdata/errors", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}

	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("Build callgraph failed: %v", err)
	}

	cr := Compute(result, cg)

	badName := "github.com/rcownie/durable/testdata/errors.BadWithFloatCondition"

	warns := cr.Warnings[badName]
	if len(warns) == 0 {
		t.Fatalf("expected W002 warning for %s, got none", badName)
	}

	foundW002 := false
	for _, w := range warns {
		if w.Code == "W002" {
			foundW002 = true
			break
		}
	}
	if !foundW002 {
		t.Errorf("expected W002 (float in condition) warning for %s, got codes: ", badName)
		for _, w := range warns {
			t.Logf("  %s: %s", w.Code, w.Message)
		}
	}
}
