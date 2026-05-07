package callgraph

import (
	"go/ast"
	"go/token"
	"strings"
	"testing"

	"github.com/rcownie/cleat/internal/analyzer"
)

func cgBasicFQ(name string) string {
	return "github.com/rcownie/cleat/testdata/basic." + name
}

func cgLoadBasic(t *testing.T) (*analyzer.AnalysisResult, *Graph) {
	t.Helper()
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/rcownie/cleat/testdata/basic", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}
	g, err := Build(result)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}
	return result, g
}

// ---- Build: edges ----

func TestBuildBasicHasEdges(t *testing.T) {
	_, g := cgLoadBasic(t)
	tests := []struct{ caller, callee string }{
		{cgBasicFQ("PlaceOrder"), cgBasicFQ("validateAndReserve")},
		{cgBasicFQ("PlaceOrder"), cgBasicFQ("processPayment")},
		{cgBasicFQ("PlaceOrder"), cgBasicFQ("fulfillOrder")},
		{cgBasicFQ("PlaceOrder"), cgBasicFQ("releaseReservation")},
		{cgBasicFQ("PlaceOrder"), cgBasicFQ("refundPayment")},
		{cgBasicFQ("PlaceOrder"), cgBasicFQ("notifyCustomer")},
		{cgBasicFQ("CancelOrder"), cgBasicFQ("refundPayment")},
		{cgBasicFQ("CancelOrder"), cgBasicFQ("releaseReservation")},
		{cgBasicFQ("validateAndReserve"), cgBasicFQ("checkItemAvailability")},
		{cgBasicFQ("validateAndReserve"), cgBasicFQ("reserveInventory")},
		{cgBasicFQ("processPayment"), cgBasicFQ("getDefaultPaymentMethod")},
		{cgBasicFQ("processPayment"), cgBasicFQ("chargeCustomer")},
	}
	for _, tt := range tests {
		if !g.Calls[tt.caller][tt.callee] {
			t.Errorf("expected edge %s -> %s", analyzer.ShortName(tt.caller), analyzer.ShortName(tt.callee))
		}
	}
}

func TestBuildCalledByConsistency(t *testing.T) {
	_, g := cgLoadBasic(t)
	for caller, callees := range g.Calls {
		for callee := range callees {
			if !g.CalledBy[callee][caller] {
				t.Errorf("CalledBy missing edge %s <- %s", analyzer.ShortName(callee), analyzer.ShortName(caller))
			}
		}
	}
}

func TestBuildAllFunctionsInitialized(t *testing.T) {
	result, g := cgLoadBasic(t)
	for fqname := range result.Funcs {
		if _, ok := g.Calls[fqname]; !ok {
			t.Errorf("no Calls entry for %s", analyzer.ShortName(fqname))
		}
	}
	if len(g.Calls) != len(result.Funcs) {
		t.Errorf("Calls entries=%d, Funcs=%d", len(g.Calls), len(result.Funcs))
	}
}

// ---- DurableLeaves ----

func TestBuildDurableLeavesBasic(t *testing.T) {
	_, g := cgLoadBasic(t)
	expected := map[string]bool{
		cgBasicFQ("checkItemAvailability"):   true,
		cgBasicFQ("getDefaultPaymentMethod"): true,
		cgBasicFQ("fulfillOrder"):            true,
		cgBasicFQ("reserveInventory"):        true,
		cgBasicFQ("chargeCustomer"):          true,
		cgBasicFQ("releaseReservation"):      true,
		cgBasicFQ("refundPayment"):           true,
		cgBasicFQ("notifyCustomer"):          true,
	}
	for name := range expected {
		if !g.DurableLeaves[name] {
			t.Errorf("expected leaf: %s", analyzer.ShortName(name))
		}
	}
	nonLeaves := []string{cgBasicFQ("PlaceOrder"), cgBasicFQ("CancelOrder"),
		cgBasicFQ("validateAndReserve"), cgBasicFQ("processPayment")}
	for _, name := range nonLeaves {
		if g.DurableLeaves[name] {
			t.Errorf("expected NOT leaf: %s", analyzer.ShortName(name))
		}
	}
}

func TestBuildDurableLeavesSetsFuncDeclFlags(t *testing.T) {
	result, g := cgLoadBasic(t)
	for name := range g.DurableLeaves {
		fd := result.Funcs[name]
		if fd == nil {
			t.Errorf("leaf %s not in Funcs", analyzer.ShortName(name))
			continue
		}
		if !fd.IsDurableLeaf || fd.DurabilityTag != "DurableLeaf" {
			t.Errorf("leaf %s: IsDurableLeaf=%v Tag=%q", analyzer.ShortName(name), fd.IsDurableLeaf, fd.DurabilityTag)
		}
	}
}

// ---- NumEdges ----

func TestNumEdges(t *testing.T) {
	_, g := cgLoadBasic(t)
	if g.NumEdges() <= 0 {
		t.Errorf("expected positive NumEdges, got %d", g.NumEdges())
	}
	count := 0
	for _, callees := range g.Calls {
		count += len(callees)
	}
	if g.NumEdges() != count {
		t.Errorf("NumEdges()=%d, manual sum=%d", g.NumEdges(), count)
	}
}

// ---- String ----

func TestGraphString(t *testing.T) {
	_, g := cgLoadBasic(t)
	s := g.String()
	for _, want := range []string{"Call graph", "edges", "cleat leaves"} {
		if !strings.Contains(s, want) {
			t.Errorf("String() should contain %q, got: %s", want, s)
		}
	}
}

func TestGraphStringEmpty(t *testing.T) {
	g := &Graph{
		Calls:         make(map[string]map[string]bool),
		CalledBy:      make(map[string]map[string]bool),
		DurableLeaves: make(map[string]bool),
	}
	if s := g.String(); s != "Call graph: 0 functions, 0 edges, 0 cleat leaves" {
		t.Errorf("got %q", s)
	}
}

// ---- hasHostCallsCall ----

func TestHasHostCallsCallOnLeaf(t *testing.T) {
	result, _ := cgLoadBasic(t)
	fd := result.Funcs[cgBasicFQ("checkItemAvailability")]
	if !hasHostCallsCall(fd) {
		t.Error("checkItemAvailability should have HostCalls call")
	}
}

func TestHasHostCallsCallOnNonLeaf(t *testing.T) {
	result, _ := cgLoadBasic(t)
	fd := result.Funcs[cgBasicFQ("PlaceOrder")]
	if hasHostCallsCall(fd) {
		t.Error("PlaceOrder should NOT have direct HostCalls call")
	}
}

func TestHasHostCallsCallWithNilInfo(t *testing.T) {
	result, _ := cgLoadBasic(t)
	for _, fd := range result.Funcs {
		if fd.Pkg != nil {
			saved := fd.Pkg.Info
			fd.Pkg.Info = nil
			if hasHostCallsCall(fd) {
				t.Errorf("hasHostCallsCall should be false with nil Info for %s", fd.Name)
			}
			fd.Pkg.Info = saved
		}
	}
}

// ---- resolveCallee ----

func TestResolveCalleeReturnsEmptyForNilInfo(t *testing.T) {
	result, _ := cgLoadBasic(t)
	fd := result.Funcs[cgBasicFQ("checkItemAvailability")]
	var callExpr *ast.CallExpr
	ast.Inspect(fd.Ast.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			callExpr = call
			return false
		}
		return true
	})
	if callExpr != nil {
		if got := resolveCallee(callExpr, nil); got != "" {
			t.Errorf("expected empty for nil info, got %q", got)
		}
	}
}
