package transform

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/rcownie/durable/internal/analyzer"
	"github.com/rcownie/durable/internal/callgraph"
	"github.com/rcownie/durable/internal/closure"
)

func tfShortName(fqname string) string {
	for i := len(fqname) - 1; i >= 0; i-- {
		if fqname[i] == '.' {
			return fqname[i+1:]
		}
	}
	return fqname
}

func tfAutothreadFQ(name string) string {
	return "github.com/rcownie/durable/testdata/autothread." + name
}

func tfBasicFQ(name string) string {
	return "github.com/rcownie/durable/testdata/basic." + name
}

func tfBuildConfig(t *testing.T, pattern string) *Config {
	t.Helper()
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages(pattern, fset)
	if err != nil {
		t.Fatalf("LoadPackages(%q) failed: %v", pattern, err)
	}
	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("Build callgraph failed: %v", err)
	}
	cr := closure.Compute(result, cg)
	return &Config{Result: result, CallGraph: cg, Closure: cr}
}

func tfSyntaxCheck(t *testing.T, name, source string) {
	t.Helper()
	_, err := parser.ParseFile(token.NewFileSet(), "", source, parser.AllErrors)
	if err != nil {
		t.Errorf("%s: not valid Go:\n%v\n--- source ---\n%s", name, err, source)
	}
}

// ---- Transform: autothread testdata ----

func TestTransformAutothreadAddsH(t *testing.T) {
	cfg := tfBuildConfig(t, "github.com/rcownie/durable/testdata/autothread")
	tr, err := Transform(cfg)
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	expected := []string{
		tfAutothreadFQ("validateAndReserve"), tfAutothreadFQ("processPayment"),
		tfAutothreadFQ("checkItemAvailability"), tfAutothreadFQ("fulfillOrder"),
		tfAutothreadFQ("getDefaultPaymentMethod"), tfAutothreadFQ("reserveInventory"),
		tfAutothreadFQ("chargeCustomer"), tfAutothreadFQ("releaseReservation"),
		tfAutothreadFQ("refundPayment"), tfAutothreadFQ("notifyCustomer"),
	}
	for _, name := range expected {
		found := false
		for _, a := range tr.AddedH {
			if a == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %s in AddedH", tfShortName(name))
		}
	}
	if len(tr.AddedH) != len(expected) {
		t.Errorf("AddedH count: want %d, got %d", len(expected), len(tr.AddedH))
	}
}

func TestTransformAutothreadEntryPointsNotModified(t *testing.T) {
	cfg := tfBuildConfig(t, "github.com/rcownie/durable/testdata/autothread")
	tr, _ := Transform(cfg)
	for _, name := range []string{tfAutothreadFQ("PlaceOrder"), tfAutothreadFQ("CancelOrder")} {
		for _, a := range tr.AddedH {
			if a == name {
				t.Errorf("entry point %s should not be in AddedH", tfShortName(name))
			}
		}
	}
}

func TestTransformAutothreadOutputSyntaxValid(t *testing.T) {
	cfg := tfBuildConfig(t, "github.com/rcownie/durable/testdata/autothread")
	tr, err := Transform(cfg)
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	if len(tr.Files) == 0 {
		t.Error("expected at least one transformed file")
	}
	for name, content := range tr.Files {
		tfSyntaxCheck(t, name, string(content))
	}
}

func TestTransformAutothreadAllFilesContainHostCalls(t *testing.T) {
	cfg := tfBuildConfig(t, "github.com/rcownie/durable/testdata/autothread")
	tr, _ := Transform(cfg)
	for _, content := range tr.Files {
		if !strings.Contains(string(content), "HostCalls") {
			t.Error("transformed file should reference HostCalls")
		}
	}
}

func TestTransformBasicNoChanges(t *testing.T) {
	cfg := tfBuildConfig(t, "github.com/rcownie/durable/testdata/basic")
	tr, err := Transform(cfg)
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	if len(tr.AddedH) != 0 {
		t.Errorf("expected 0 AddedH for basic, got %v", tr.AddedH)
	}
}

func TestTransformEmptyClosure(t *testing.T) {
	cfg := tfBuildConfig(t, "github.com/rcownie/durable/testdata/basic")
	cfg.Closure = &closure.Result{
		DurableLeaves:  make(map[string]bool),
		DurableClosure: make(map[string]bool),
		Pure:           make(map[string]bool),
		Errors:         make(map[string][]closure.ValidationError),
		Warnings:       make(map[string][]closure.ValidationWarning),
	}
	tr, err := Transform(cfg)
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	if len(tr.AddedH) != 0 || len(tr.Files) != 0 {
		t.Error("expected empty result for empty closure")
	}
}

// ---- findGlobalH ----

func TestFindGlobalHAutothread(t *testing.T) {
	cfg := tfBuildConfig(t, "github.com/rcownie/durable/testdata/autothread")
	gh, users, file := findGlobalH(cfg.Result)
	if gh == nil {
		t.Fatal("expected global h in autothread — var h durable.HostCalls should be detected")
	}
	if file == nil {
		t.Fatal("expected file containing global h")
	}
	if len(users) == 0 {
		t.Fatal("expected non-empty global h users map")
	}
	// The transform correctly adds h to all closure functions (tested by
	// TestTransformAutothreadAddsH). Here we just verify findGlobalH works.
	for _, name := range []string{"validateAndReserve", "processPayment"} {
		if users[tfAutothreadFQ(name)] {
			t.Errorf("%s should NOT be a global h user (pass-through)", name)
		}
	}
}

func TestFindGlobalHBasicNil(t *testing.T) {
	cfg := tfBuildConfig(t, "github.com/rcownie/durable/testdata/basic")
	gh, users, file := findGlobalH(cfg.Result)
	if gh != nil || users != nil || file != nil {
		t.Error("expected nil for basic (no global h)")
	}
}

// ---- hasHostCallsParam ----

func TestHasHostCallsParamEntryPoint(t *testing.T) {
	cfg := tfBuildConfig(t, "github.com/rcownie/durable/testdata/basic")
	fd := cfg.Result.Funcs[tfBasicFQ("PlaceOrder")]
	if !hasHostCallsParam(fd) {
		t.Error("PlaceOrder should have HostCalls param")
	}
}

func TestHasHostCallsParamAutothreadLeaf(t *testing.T) {
	cfg := tfBuildConfig(t, "github.com/rcownie/durable/testdata/autothread")
	fd := cfg.Result.Funcs[tfAutothreadFQ("checkItemAvailability")]
	if hasHostCallsParam(fd) {
		t.Error("autothread leaf should NOT have HostCalls param (uses global h)")
	}
}
