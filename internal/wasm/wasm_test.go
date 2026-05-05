package wasm

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/rcownie/durable/internal/analyzer"
	"github.com/rcownie/durable/internal/callgraph"
	"github.com/rcownie/durable/internal/closure"
)

func basicFQ(name string) string {
	return "github.com/rcownie/durable/testdata/basic." + name
}

func loadBasic(t *testing.T) (*analyzer.AnalysisResult, *closure.Result) {
	t.Helper()
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/rcownie/durable/testdata/basic", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}
	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("Build callgraph failed: %v", err)
	}
	cr := closure.Compute(result, cg)
	return result, cr
}

func loadErrors(t *testing.T) (*analyzer.AnalysisResult, *closure.Result) {
	t.Helper()
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/rcownie/durable/testdata/errors", fset)
	if err != nil {
		t.Fatalf("LoadPackages failed: %v", err)
	}
	cg, err := callgraph.Build(result)
	if err != nil {
		t.Fatalf("Build callgraph failed: %v", err)
	}
	cr := closure.Compute(result, cg)
	return result, cr
}

func syntaxCheck(t *testing.T, name, source string) {
	t.Helper()
	_, err := parser.ParseFile(token.NewFileSet(), "", source, parser.AllErrors)
	if err != nil {
		t.Errorf("%s: not valid Go:\n%v\n--- source ---\n%s", name, err, source)
	}
}

// ---- AnalyzeUsage ----

func TestAnalyzeUsageBasicDetectsDurableCall(t *testing.T) {
	result, cr := loadBasic(t)
	usage := AnalyzeUsage(result, cr)
	if !usage.Used["durable_call"] {
		t.Error("expected durable_call to be used")
	}
	unused := []string{"durable_sleep", "durable_await_signals", "durable_defer",
		"durable_log", "durable_poll_cancellation", "durable_poll_signal",
		"durable_continue_as_new", "durable_child_workflow", "durable_await_child",
		"durable_version", "durable_min_version", "set_query_state",
		"durable_now", "durable_random"}
	for _, name := range unused {
		if usage.Used[name] {
			t.Errorf("expected %q to not be used in basic testdata", name)
		}
	}
	if usage.Count() < 1 {
		t.Errorf("expected Count()>=1, got %d", usage.Count())
	}
}

func TestAnalyzeUsageErrorsDetectsMultiple(t *testing.T) {
	result, cr := loadErrors(t)
	usage := AnalyzeUsage(result, cr)
	if !usage.Used["durable_log"] {
		t.Error("expected durable_log (leafFunc calls DurableLog)")
	}
	if !usage.Used["durable_call"] {
		t.Error("expected durable_call (BadWithGoroutine calls DurableCall)")
	}
	if usage.Count() < 2 {
		t.Errorf("expected Count()>=2, got %d", usage.Count())
	}
}

func TestAnalyzeUsageWithEmptyClosure(t *testing.T) {
	result, _ := loadBasic(t)
	emptyCR := &closure.Result{
		DurableLeaves:  make(map[string]bool),
		DurableClosure: make(map[string]bool),
		Pure:           make(map[string]bool),
		Errors:         make(map[string][]closure.ValidationError),
		Warnings:       make(map[string][]closure.ValidationWarning),
	}
	usage := AnalyzeUsage(result, emptyCR)
	if usage.Count() != 0 {
		t.Errorf("expected 0 for empty closure, got %d", usage.Count())
	}
}

// ---- GenerateImports ----

func TestGenerateImportsBasic(t *testing.T) {
	result, cr := loadBasic(t)
	usage := AnalyzeUsage(result, cr)
	code := string(GenerateImports("basic", usage))
	checks := []string{
		"//go:build wasip1",
		"package basic",
		`import "unsafe"`,
		"//go:wasmimport env durable_call",
		"func durableCallImport(",
		"servicePtr unsafe.Pointer, serviceLen uint32",
		"operationPtr unsafe.Pointer, operationLen uint32",
		"requestJSONPtr unsafe.Pointer, requestJSONLen uint32",
		"outresponsePtr unsafe.Pointer, maxresponseLen uint32",
		") int64",
	}
	for _, c := range checks {
		if !strings.Contains(code, c) {
			t.Errorf("expected: %s", c)
		}
	}
}

func TestGenerateImportsNoUnused(t *testing.T) {
	result, cr := loadBasic(t)
	usage := AnalyzeUsage(result, cr)
	code := string(GenerateImports("basic", usage))
	for _, name := range []string{"durableSleepImport", "durableAwaitSignalsImport",
		"durableDeferImport", "durableLogImport", "durableNowImport", "durableRandomImport"} {
		if strings.Contains(code, name) {
			t.Errorf("unexpected import stub %q", name)
		}
	}
}

func TestGenerateImportsSyntaxValid(t *testing.T) {
	result, cr := loadBasic(t)
	usage := AnalyzeUsage(result, cr)
	syntaxCheck(t, "GenerateImports", string(GenerateImports("mypkg", usage)))
}

func TestGenerateImportsAllHostFunctions(t *testing.T) {
	usage := &UsageInfo{Used: make(map[string]bool), Funcs: hostFunctions}
	for _, hf := range hostFunctions {
		usage.Used[hf.ImportName] = true
	}
	code := string(GenerateImports("allimports", usage))
	for _, hf := range hostFunctions {
		if !strings.Contains(code, goName(hf.ImportName)+"Import") {
			t.Errorf("missing import stub for %s", hf.ImportName)
		}
	}
	syntaxCheck(t, "GenerateImports(all)", code)
}

// ---- GenerateMemory ----

func TestGenerateMemory(t *testing.T) {
	code := string(GenerateMemory("mypkg"))
	for _, c := range []string{"//go:build wasip1", "package mypkg",
		`import "unsafe"`, "func readString(", "func stringPtr("} {
		if !strings.Contains(code, c) {
			t.Errorf("expected: %s", c)
		}
	}
	syntaxCheck(t, "GenerateMemory", code)
}

// ---- GenerateHostAdapter ----

func TestGenerateHostAdapterBasic(t *testing.T) {
	result, cr := loadBasic(t)
	usage := AnalyzeUsage(result, cr)
	code := string(GenerateHostAdapter("basic", usage))
	for _, c := range []string{"//go:build wasip1", "package basic",
		`"fmt"`, `"unsafe"`, `"github.com/rcownie/durable/durable"`,
		"func makeHostCalls() durable.HostCalls {",
		"DurableCall: func(", "responseBuf := make([]byte, _durableOutBufSize)",
	} {
		if !strings.Contains(code, c) {
			t.Errorf("expected: %s", c)
		}
	}
	syntaxCheck(t, "GenerateHostAdapter", code)
}

func TestGenerateHostAdapterNoUnusedFields(t *testing.T) {
	result, cr := loadBasic(t)
	usage := AnalyzeUsage(result, cr)
	code := string(GenerateHostAdapter("basic", usage))
	if strings.Contains(code, "DurableSleep:") {
		t.Error("DurableSleep should not be generated for basic testdata")
	}
}

// ---- GenerateExports ----

func TestGenerateExportsBasic(t *testing.T) {
	result, cr := loadBasic(t)
	_ = cr
	code := string(GenerateExports("basic", result))
	for _, c := range []string{"//go:build wasip1", "package basic",
		"func writeJSONOut", "func writeErrorOut",
		"//go:wasmexport place_order",
		"//go:wasmexport cancel_order",
		"h := makeHostCalls()"} {
		if !strings.Contains(code, c) {
			t.Errorf("expected: %s", c)
		}
	}
}

func TestGenerateExportsPlaceOrderUnmarshalsArgs(t *testing.T) {
	result, cr := loadBasic(t)
	_ = cr
	code := string(GenerateExports("basic", result))
	if !strings.Contains(code, `UserID string `) || !strings.Contains(code, `userID`) {
		t.Error("expected UserID field in args struct")
	}
}

func TestGenerateExportsSyntaxValid(t *testing.T) {
	result, cr := loadBasic(t)
	_ = cr
	syntaxCheck(t, "GenerateExports", string(GenerateExports("basic", result)))
}

// ---- BuildOutputs ----

func TestBuildOutputsBasic(t *testing.T) {
	result, cr := loadBasic(t)
	usage := AnalyzeUsage(result, cr)
	of := BuildOutputs("basic", usage, result)
	if of.Imports == "" || of.Memory == "" || of.Adapter == "" || of.Exports == "" {
		t.Fatal("BuildOutputs returned empty fields")
	}
	syntaxCheck(t, "Imports", of.Imports)
	syntaxCheck(t, "Memory", of.Memory)
	syntaxCheck(t, "Adapter", of.Adapter)
	syntaxCheck(t, "Exports", of.Exports)
}

// ---- goName ----

func TestGoName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"durable_call", "durableCall"},
		{"durable_sleep", "durableSleep"},
		{"durable_await_signals", "durableAwaitSignals"},
		{"set_query_state", "setQueryState"},
		{"durable_now", "durableNow"},
		{"durable_child_workflow", "durableChildWorkflow"},
		{"durable_min_version", "durableMinVersion"},
	}
	for _, tt := range tests {
		if got := goName(tt.in); got != tt.want {
			t.Errorf("goName(%q)=%q, want %q", tt.in, got, tt.want)
		}
	}
}

// ---- ToSnakeCase ----

func TestToSnakeCase(t *testing.T) {
	tests := []struct{ in, want string }{
		{"PlaceOrder", "place_order"},
		{"CancelOrder", "cancel_order"},
		{"Now", "now"},
		{"NowMs", "now_ms"},
		{"ChildWorkflow", "child_workflow"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := ToSnakeCase(tt.in); got != tt.want {
			t.Errorf("ToSnakeCase(%q)=%q, want %q", tt.in, got, tt.want)
		}
	}
}

// ---- capitalize ----

func TestCapitalize(t *testing.T) {
	tests := []struct{ in, want string }{
		{"userID", "UserID"},
		{"orderID", "OrderID"},
		{"", ""},
		{"a", "A"},
	}
	for _, tt := range tests {
		if got := capitalize(tt.in); got != tt.want {
			t.Errorf("capitalize(%q)=%q, want %q", tt.in, got, tt.want)
		}
	}
}

// ---- needsFmt/needsJSON/needsUnsafe ----

func TestNeedsFmt(t *testing.T) {
	usage := &UsageInfo{Used: map[string]bool{"durable_call": true},
		Funcs: []HostFunction{{ImportName: "durable_call", FieldName: "DurableCall"}}}
	if !needsFmt(usage) {
		t.Error("durable_call adapter uses fmt.Sprintf")
	}
	usage2 := &UsageInfo{Used: map[string]bool{"durable_sleep": true},
		Funcs: []HostFunction{{ImportName: "durable_sleep", FieldName: "DurableSleep"}}}
	if needsFmt(usage2) {
		t.Error("durable_sleep should not need fmt")
	}
}

func TestNeedsJSON(t *testing.T) {
	usage := &UsageInfo{Used: map[string]bool{"durable_await_signals": true},
		Funcs: []HostFunction{{ImportName: "durable_await_signals", FieldName: "DurableAwaitSignals"}}}
	if !needsJSON(usage) {
		t.Error("durable_await_signals uses []string params")
	}
	usage2 := &UsageInfo{Used: map[string]bool{"durable_call": true},
		Funcs: []HostFunction{{ImportName: "durable_call", FieldName: "DurableCall"}}}
	if needsJSON(usage2) {
		t.Error("durable_call should not need json")
	}
}

func TestNeedsUnsafe(t *testing.T) {
	usage := &UsageInfo{Used: map[string]bool{"durable_call": true},
		Funcs: []HostFunction{{ImportName: "durable_call", FieldName: "DurableCall"}}}
	if !needsUnsafe(usage) {
		t.Error("durable_call uses unsafe.String")
	}
	usage2 := &UsageInfo{Used: map[string]bool{"durable_log": true},
		Funcs: []HostFunction{{ImportName: "durable_log", FieldName: "DurableLog"}}}
	if needsUnsafe(usage2) {
		t.Error("durable_log should not need unsafe")
	}
}

// ---- numOutBufs / outBufNames ----

func TestNumOutBufs(t *testing.T) {
	tests := map[string]int{
		"durable_call": 1, "durable_sleep": 0, "durable_await_signals": 2,
		"durable_defer": 1, "durable_log": 0, "durable_poll_cancellation": 1,
		"durable_poll_signal": 1, "durable_child_workflow": 1,
		"durable_await_child": 1, "durable_version": 0, "nonexistent": 0,
	}
	for name, want := range tests {
		if got := numOutBufs(name); got != want {
			t.Errorf("numOutBufs(%q)=%d, want %d", name, got, want)
		}
	}
}

func TestOutBufNames(t *testing.T) {
	if names := outBufNames("durable_call"); len(names) != 1 || names[0] != "responseBuf" {
		t.Errorf("outBufNames(durable_call)=%v", names)
	}
	if names := outBufNames("durable_await_signals"); len(names) != 2 ||
		names[0] != "signalNameBuf" || names[1] != "payloadBuf" {
		t.Errorf("outBufNames(durable_await_signals)=%v", names)
	}
	if names := outBufNames("durable_sleep"); names != nil {
		t.Errorf("outBufNames(durable_sleep) should be nil, got %v", names)
	}
}

// ---- importParamDecl ----

func TestImportParamDecl(t *testing.T) {
	tests := []struct {
		spec paramSpec
		want string
	}{
		{paramSpec{Name: "service", Kind: kindInString},
			"servicePtr unsafe.Pointer, serviceLen uint32"},
		{paramSpec{Name: "response", Kind: kindOutString},
			"outresponsePtr unsafe.Pointer, maxresponseLen uint32"},
		{paramSpec{Name: "durationMs", Kind: kindInt64}, "durationMs int64"},
	}
	for _, tt := range tests {
		if got := importParamDecl(tt.spec); got != tt.want {
			t.Errorf("importParamDecl(%+v)=%q, want %q", tt.spec, got, tt.want)
		}
	}
}

// ---- Consistency: importDefs <-> hostFunctions <-> adapterDefs ----

func TestImportDefsCoverage(t *testing.T) {
	for _, hf := range hostFunctions {
		if _, ok := importDefs[hf.ImportName]; !ok {
			t.Errorf("hostFunctions[%s].ImportName=%q has no importDef", hf.FieldName, hf.ImportName)
		}
	}
}

func TestAdapterDefsCoverage(t *testing.T) {
	for fieldName := range adapterDefs {
		found := false
		for _, hf := range hostFunctions {
			if hf.FieldName == fieldName {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("adapterDef %q has no matching HostFunction", fieldName)
		}
	}
}
