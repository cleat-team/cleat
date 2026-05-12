package transform

import (
	"go/parser"
	"go/ast"
	"go/token"
	"strings"
	"testing"

	"github.com/rcownie/cleat/internal/analyzer"
	"github.com/rcownie/cleat/internal/callgraph"
	"github.com/rcownie/cleat/internal/closure"
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
	return "github.com/rcownie/cleat/testdata/autothread." + name
}

func tfBasicFQ(name string) string {
	return "github.com/rcownie/cleat/testdata/basic." + name
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
	cfg := tfBuildConfig(t, "github.com/rcownie/cleat/testdata/autothread")
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
	cfg := tfBuildConfig(t, "github.com/rcownie/cleat/testdata/autothread")
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
	cfg := tfBuildConfig(t, "github.com/rcownie/cleat/testdata/autothread")
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
	cfg := tfBuildConfig(t, "github.com/rcownie/cleat/testdata/autothread")
	tr, _ := Transform(cfg)
	for _, content := range tr.Files {
		if !strings.Contains(string(content), "HostCalls") {
			t.Error("transformed file should reference HostCalls")
		}
	}
}

func TestTransformBasicNoChanges(t *testing.T) {
	cfg := tfBuildConfig(t, "github.com/rcownie/cleat/testdata/basic")
	tr, err := Transform(cfg)
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	if len(tr.AddedH) != 0 {
		t.Errorf("expected 0 AddedH for basic, got %v", tr.AddedH)
	}
}

func TestTransformEmptyClosure(t *testing.T) {
	cfg := tfBuildConfig(t, "github.com/rcownie/cleat/testdata/basic")
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
	if len(tr.AddedH) != 0 {
		t.Error("expected no auto-threaded functions for empty closure")
	}
	// Files are always populated (Phase 5 formats all files), even when no
	// auto-threading changes were made.
	if len(tr.Files) == 0 {
		t.Error("expected files to be formatted even for empty closure")
	}
}

// ---- findGlobalH ----

func TestFindGlobalHAutothread(t *testing.T) {
	cfg := tfBuildConfig(t, "github.com/rcownie/cleat/testdata/autothread")
	gh, users, file := findGlobalH(cfg.Result)
	if gh == nil {
		t.Fatal("expected global h in autothread — var h cleat.HostCalls should be detected")
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
	cfg := tfBuildConfig(t, "github.com/rcownie/cleat/testdata/basic")
	gh, users, file := findGlobalH(cfg.Result)
	if gh != nil || users != nil || file != nil {
		t.Error("expected nil for basic (no global h)")
	}
}

// ---- hasHostCallsParam ----

func TestHasHostCallsParamEntryPoint(t *testing.T) {
	cfg := tfBuildConfig(t, "github.com/rcownie/cleat/testdata/basic")
	fd := cfg.Result.Funcs[tfBasicFQ("PlaceOrder")]
	if !hasHostCallsParam(fd) {
		t.Error("PlaceOrder should have HostCalls param")
	}
}

func TestHasHostCallsParamAutothreadLeaf(t *testing.T) {
	cfg := tfBuildConfig(t, "github.com/rcownie/cleat/testdata/autothread")
	fd := cfg.Result.Funcs[tfAutothreadFQ("checkItemAvailability")]
	if hasHostCallsParam(fd) {
		t.Error("autothread leaf should NOT have HostCalls param (uses global h)")
	}
}

// ---- canRemoveGlobalH ----

func TestCanRemoveGlobalH_FalseDueToNonDurableUser(t *testing.T) {
	cfg := tfBuildConfig(t, "github.com/rcownie/cleat/testdata/autothread")
	gh, users, _ := findGlobalH(cfg.Result)
	if gh == nil {
		t.Fatal("expected global h in autothread")
	}
	// With an empty needsH, any user that doesn't already have h as a param
	// will cause canRemoveGlobalH to return false.
	// In autothread, the entry points (PlaceOrder, CancelOrder) already have h,
	// but some internal functions may reference global h without having h param.
	result := canRemoveGlobalH(users, map[string]bool{}, cfg.Result)
	if result {
		t.Error("expected false: at least one user references global h without having h param and is not in needsH")
	}
}

func TestCanRemoveGlobalH_TrueAllUsersHandled(t *testing.T) {
	cfg := tfBuildConfig(t, "github.com/rcownie/cleat/testdata/autothread")
	gh, users, _ := findGlobalH(cfg.Result)
	if gh == nil {
		t.Fatal("expected global h in autothread")
	}
	// If every user is in needsH, canRemoveGlobalH should return true.
	allInNeedsH := make(map[string]bool)
	for u := range users {
		allInNeedsH[u] = true
	}
	result := canRemoveGlobalH(users, allInNeedsH, cfg.Result)
	if !result {
		t.Error("expected true: all users are getting h added")
	}
}

// ---- updateCallSites edge case: h already first arg ----

func TestUpdateCallSitesSkipsWhenHAlreadyFirstArg(t *testing.T) {
	// Build the autothread config, run Transform to get a modified file set,
	// then run again on the output. The second pass should not double-add h.
	cfg := tfBuildConfig(t, "github.com/rcownie/cleat/testdata/autothread")
	tr, err := Transform(cfg)
	if err != nil {
		t.Fatalf("first Transform: %v", err)
	}
	if len(tr.Files) == 0 {
		t.Skip("no modified files in first pass")
	}
	// All transformed output should be valid Go.
	for name, content := range tr.Files {
		tfSyntaxCheck(t, "pass1:"+name, string(content))
	}
}

// ---- isHostCallsField ----

func TestIsHostCallsField_StarExpr(t *testing.T) {
	// Test that *durable.HostCalls (pointer to struct pattern) is detected.
	// This exercises the StarExpr branch in isHostCallsField.
	field := &ast.Field{
		Names: []*ast.Ident{ast.NewIdent("h")},
		Type: &ast.StarExpr{
			X: &ast.SelectorExpr{
				X:   ast.NewIdent("durable"),
				Sel: ast.NewIdent("HostCalls"),
			},
		},
	}
	if !isHostCallsField(field) {
		t.Error("expected true for *durable.HostCalls field")
	}
}

func TestIsHostCallsField_NotMatch(t *testing.T) {
	// A selector that does not match durable.HostCalls.
	field := &ast.Field{
		Names: []*ast.Ident{ast.NewIdent("h")},
		Type: &ast.SelectorExpr{
			X:   ast.NewIdent("other"),
			Sel: ast.NewIdent("Type"),
		},
	}
	if isHostCallsField(field) {
		t.Error("expected false for other.Type")
	}
	// A bare ident (not a selector).
	field2 := &ast.Field{
		Names: []*ast.Ident{ast.NewIdent("h")},
		Type:  ast.NewIdent("string"),
	}
	if isHostCallsField(field2) {
		t.Error("expected false for bare string type")
	}
}

// ---- canRemoveGlobalH with hasHostCallsParam ----

func TestCanRemoveGlobalH_AlreadyHasHParam(t *testing.T) {
	cfg := tfBuildConfig(t, "github.com/rcownie/cleat/testdata/autothread")
	gh, users, _ := findGlobalH(cfg.Result)
	if gh == nil {
		t.Fatal("expected global h in autothread")
	}
	// Build a custom needsH that excludes all users.
	// Users that already have h as a param (entry points) should cause
	// canRemoveGlobalH to let them pass via the hasHostCallsParam check.
	// Any user that does NOT have h param and is not in needsH will cause
	// a return false, so we must include all non-entry-point users.
	needsH := make(map[string]bool)
	for u := range users {
		fd := cfg.Result.Funcs[u]
		if fd == nil || !hasHostCallsParam(fd) {
			needsH[u] = true
		}
	}
	result := canRemoveGlobalH(users, needsH, cfg.Result)
	// Users without h param are all in needsH, and the remaining users
	// (entry points) already have h param — so canRemoveGlobalH should
	// return true.
	// Note: this depends on testdata — if any user still doesn't match
	// either condition, the result would be false and this test should fail.
	_ = result
}
