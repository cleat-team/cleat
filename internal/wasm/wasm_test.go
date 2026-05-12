package wasm

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

func basicFQ(name string) string {
	return "github.com/rcownie/cleat/testdata/basic." + name
}

func loadBasic(t *testing.T) (*analyzer.AnalysisResult, *closure.Result) {
	t.Helper()
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages("github.com/rcownie/cleat/testdata/basic", fset)
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
	result, err := analyzer.LoadPackages("github.com/rcownie/cleat/testdata/errors", fset)
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
	if !usage.Used["cleat_call"] {
		t.Error("expected cleat_call to be used")
	}
	unused := []string{"cleat_sleep", "cleat_await_signals", "cleat_defer",
		"cleat_log", "cleat_poll_cancellation", "cleat_poll_signal",
		"cleat_continue_as_new", "cleat_child_workflow", "cleat_await_child",
		"cleat_version", "cleat_min_version", "set_query_state",
		"cleat_now", "cleat_random"}
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
	if !usage.Used["cleat_log"] {
		t.Error("expected cleat_log (leafFunc calls DurableLog)")
	}
	if !usage.Used["cleat_call"] {
		t.Error("expected cleat_call (BadWithGoroutine calls DurableCall)")
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
		"//go:wasmimport env cleat_call",
		"func cleatCallImport(",
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
	for _, name := range []string{"cleatSleepImport", "cleatAwaitSignalsImport",
		"cleatDeferImport", "cleatLogImport", "cleatNowImport", "cleatRandomImport"} {
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
	code := string(GenerateHostAdapter("basic", usage, "go"))
	for _, c := range []string{"//go:build wasip1", "package basic",
		`"fmt"`, `"unsafe"`, `"github.com/rcownie/cleat/cleat"`,
		"func makeHostCalls() cleat.HostCalls {",
		"DurableCall: func(service string, operation string, requestJSON string) (string, error)",
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
	code := string(GenerateHostAdapter("basic", usage, "go"))
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

func TestGenerateExportsErrorsZeroArgEntryPoints(t *testing.T) {
	result, cr := loadErrors(t)
	_ = cr
	code := string(GenerateExports("errors", result))
	// BadWithGoroutine, BadWithFuncValue, BadWithFloatCondition have no params
	// beyond h — their generated code should suppress args parsing.
	if !strings.Contains(code, "_ = argsPtr") {
		t.Error("expected args suppression for zero-arg entry points")
	}
	if !strings.Contains(code, "_ = argsLen") {
		t.Error("expected argsLen suppression for zero-arg entry points")
	}
	// void-return entry points should not have result variables.
	if !strings.Contains(code, "BadWithGoroutine(h)") {
		t.Error("expected void call for BadWithGoroutine")
	}
}

func TestGenerateExportsErrorsOnlyReturn(t *testing.T) {
	result, cr := loadErrors(t)
	_ = cr
	code := string(GenerateExports("errors", result))
	// BadWithInterfaceDispatch returns only error — no result marshal.
	if !strings.Contains(code, "__susResultErr") {
		t.Error("expected __susResultErr assignment")
	}
	if !strings.Contains(code, "BadWithInterfaceDispatch") {
		t.Error("expected BadWithInterfaceDispatch export")
	}
}

// ---- BuildOutputs ----

func TestBuildOutputsBasic(t *testing.T) {
	result, cr := loadBasic(t)
	usage := AnalyzeUsage(result, cr)
	of := BuildOutputs("basic", usage, result, "go")
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
		{"cleat_call", "cleatCall"},
		{"cleat_sleep", "cleatSleep"},
		{"cleat_await_signals", "cleatAwaitSignals"},
		{"set_query_state", "setQueryState"},
		{"cleat_now", "cleatNow"},
		{"cleat_child_workflow", "cleatChildWorkflow"},
		{"cleat_min_version", "cleatMinVersion"},
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
	usage := &UsageInfo{Used: map[string]bool{"cleat_call": true},
		Funcs: []HostFunction{{ImportName: "cleat_call", FieldName: "DurableCall"}}}
	if !needsFmt(usage) {
		t.Error("cleat_call adapter uses fmt.Sprintf")
	}
	usage2 := &UsageInfo{Used: map[string]bool{"cleat_sleep": true},
		Funcs: []HostFunction{{ImportName: "cleat_sleep", FieldName: "DurableSleep"}}}
	if needsFmt(usage2) {
		t.Error("cleat_sleep should not need fmt")
	}
}

func TestNeedsJSON(t *testing.T) {
	usage := &UsageInfo{Used: map[string]bool{"cleat_await_signals": true},
		Funcs: []HostFunction{{ImportName: "cleat_await_signals", FieldName: "DurableAwaitSignals"}}}
	if !needsJSON(usage) {
		t.Error("cleat_await_signals uses []string params")
	}
	usage2 := &UsageInfo{Used: map[string]bool{"cleat_call": true},
		Funcs: []HostFunction{{ImportName: "cleat_call", FieldName: "DurableCall"}}}
	if needsJSON(usage2) {
		t.Error("cleat_call should not need json")
	}
}

func TestNeedsUnsafe(t *testing.T) {
	usage := &UsageInfo{Used: map[string]bool{"cleat_call": true},
		Funcs: []HostFunction{{ImportName: "cleat_call", FieldName: "DurableCall"}}}
	if !needsUnsafe(usage) {
		t.Error("cleat_call uses unsafe.String")
	}
	usage2 := &UsageInfo{Used: map[string]bool{"cleat_log": true},
		Funcs: []HostFunction{{ImportName: "cleat_log", FieldName: "DurableLog"}}}
	if needsUnsafe(usage2) {
		t.Error("cleat_log should not need unsafe")
	}
}

func TestNeedsJSONWithPluginCallStreaming(t *testing.T) {
	usage := &UsageInfo{Used: map[string]bool{"cleat_plugin_call_streaming": true},
		Funcs: []HostFunction{{ImportName: "cleat_plugin_call_streaming", FieldName: "PluginCallStreaming"}}}
	if !needsJSON(usage) {
		t.Error("PluginCallStreaming result stmts contain json.Unmarshal")
	}
}

// ---- numOutBufs / outBufNames ----

func TestNumOutBufs(t *testing.T) {
	tests := map[string]int{
		"cleat_call": 1, "cleat_sleep": 0, "cleat_await_signals": 2,
		"cleat_defer": 1, "cleat_log": 0, "cleat_poll_cancellation": 1,
		"cleat_poll_signal": 1, "cleat_child_workflow": 1,
		"cleat_await_child": 1, "cleat_version": 0, "nonexistent": 0,
	}
	for name, want := range tests {
		if got := numOutBufs(name); got != want {
			t.Errorf("numOutBufs(%q)=%d, want %d", name, got, want)
		}
	}
}

func TestOutBufNames(t *testing.T) {
	if names := outBufNames("cleat_call"); len(names) != 1 || names[0] != "responseBuf" {
		t.Errorf("outBufNames(cleat_call)=%v", names)
	}
	if names := outBufNames("cleat_await_signals"); len(names) != 2 ||
		names[0] != "signalNameBuf" || names[1] != "payloadBuf" {
		t.Errorf("outBufNames(cleat_await_signals)=%v", names)
	}
	if names := outBufNames("cleat_sleep"); names != nil {
		t.Errorf("outBufNames(cleat_sleep) should be nil, got %v", names)
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
		if hf.ImportName == "" {
			continue // no WASM import needed (e.g., RunDetached is only tracked)
		}
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

// ---- Bit-packing round-trip tests ----
//
// These tests verify that the bit-packing formats used by the host runtime
// (internal/host/memory.go) and the extraction logic emitted by the code
// generator (adapter.go ResultStmts) are compatible. The packing logic is
// inlined to keep tests self-contained since packDurableCallResult and
// packSimpleResult are unexported in the host package.

// TestBitPackingDurableCall verifies the durable_call result format:
//
//	bits 40-63 = responseLen (24 bits)
//	bits 8-39  = callErrorCode (32 bits, though passed as byte)
//	bits 0-7   = errCode (8 bits)
//
// See packDurableCallResult in internal/host/memory.go and the DurableCall
// ResultStmts in adapter.go.
func TestBitPackingDurableCall(t *testing.T) {
	tests := []struct {
		responseLen   int
		callErrorCode byte
		errCode       byte
	}{
		{0, 0, 0},
		{1, 0, 0},
		{0xFFFFFF, 0, 0},       // max 24-bit value -- sets bit 63 when shifted
		{0, 0xFF, 0},           // max callErrorCode
		{0, 0, 0xFF},           // max errCode
		{0xABCDEF, 0x12, 0x34}, // all fields non-zero
		{0x800000, 0, 0},       // 0x800000 << 40 sets bit 63 -> negative int64
	}
	for _, tt := range tests {
		// Pack using the same formula as host.packDurableCallResult.
		packed := int64(uint64(tt.responseLen)<<40 | uint64(tt.callErrorCode)<<8 | uint64(tt.errCode))

		// Extract using the adapter.go DurableCall ResultStmts.
		responseLen := uint32(uint64(packed) >> 40)
		callErrorCode := byte((uint64(packed) >> 8) & 0xFF)
		errCode := byte(uint64(packed) & 0xFF)

		if responseLen != uint32(tt.responseLen) {
			t.Errorf("[%#x] responseLen = %d, want %d", uint64(packed), responseLen, tt.responseLen)
		}
		if callErrorCode != tt.callErrorCode {
			t.Errorf("[%#x] callErrorCode = %d, want %d", uint64(packed), callErrorCode, tt.callErrorCode)
		}
		if errCode != tt.errCode {
			t.Errorf("[%#x] errCode = %d, want %d", uint64(packed), errCode, tt.errCode)
		}

		// Verify that extracting without the uint64 cast would be wrong when bit 63 is set.
		if uint64(packed)&(1<<63) != 0 {
			badResponseLen := uint32(packed >> 40) // arithmetic shift -- sign extends!
			if badResponseLen == responseLen {
				t.Errorf("[%#x] expected arithmetic shift to produce wrong result, but got %d",
					uint64(packed), badResponseLen)
			}
		}
	}
}

// TestBitPackingAwaitSignals verifies the durable_await_signals result format:
//
//	bits 48-63 = signalNameLen (16 bits)
//	bits 32-47 = payloadLen (16 bits)
//	bits 16-31 = timedOut flag (non-zero == true)
//	bits 0-15  = errCode (16 bits)
//
// See the DurableAwaitSignals ResultStmts in adapter.go.
func TestBitPackingAwaitSignals(t *testing.T) {
	tests := []struct {
		signalNameLen uint32
		payloadLen    uint32
		timedOut      bool
		errCode       uint32
	}{
		{0, 0, false, 0},
		{1, 0, false, 0},
		{0xFFFF, 0, false, 0},   // max 16-bit signalNameLen
		{0, 0xFFFF, false, 0},    // max 16-bit payloadLen
		{0, 0, true, 0},          // timedOut set
		{0, 0, false, 0xFFFF},    // max 16-bit errCode
		{0xABCD, 0x1234, true, 0x5678}, // all fields non-zero
	}
	for _, tt := range tests {
		var timedOutVal uint32
		if tt.timedOut {
			timedOutVal = 1
		}
		packed := int64(uint64(tt.signalNameLen)<<48 |
			uint64(tt.payloadLen)<<32 |
			uint64(timedOutVal)<<16 |
			uint64(tt.errCode))

		// Extract using adapter.go DurableAwaitSignals ResultStmts.
		signalNameLen := uint32(uint64(packed) >> 48)
		payloadLen := uint32((uint64(packed) >> 32) & 0xFFFF)
		timedOut := uint32((uint64(packed) >> 16) & 0xFFFF) != 0
		errCode := uint32(uint64(packed) & 0xFFFF)

		if signalNameLen != tt.signalNameLen {
			t.Errorf("[%#x] signalNameLen = %d, want %d", uint64(packed), signalNameLen, tt.signalNameLen)
		}
		if payloadLen != tt.payloadLen {
			t.Errorf("[%#x] payloadLen = %d, want %d", uint64(packed), payloadLen, tt.payloadLen)
		}
		if timedOut != tt.timedOut {
			t.Errorf("[%#x] timedOut = %v, want %v", uint64(packed), timedOut, tt.timedOut)
		}
		if errCode != tt.errCode {
			t.Errorf("[%#x] errCode = %d, want %d", uint64(packed), errCode, tt.errCode)
		}
	}
}

// TestBitPackingSleep verifies the durable_sleep result format:
//
//	bit  56     = sleepStatus (1 byte)
//	bits 0-55   = durationMs (unused in extraction)
//
// See the DurableSleep ResultStmts in adapter.go.
func TestBitPackingSleep(t *testing.T) {
	tests := []struct {
		sleepStatus byte
	}{
		{0},   // normal return
		{1},   // suspend sentinel
		{0xFF}, // any non-zero value
	}
	for _, tt := range tests {
		packed := int64(uint64(tt.sleepStatus) << 56)

		// Extract using adapter.go DurableSleep ResultStmts.
		sleepStatus := byte(uint64(packed) >> 56)

		if sleepStatus != tt.sleepStatus {
			t.Errorf("[%#x] sleepStatus = %d, want %d", uint64(packed), sleepStatus, tt.sleepStatus)
		}
	}
}

// TestDecodeExportResult verifies the export result format used by
// writeJSONOut/writeErrorOut in exports.go and decodeExportResult in
// internal/host/memory.go:
//
//	bits 0-31  = errCode (0 = success)
//	bits 32-63 = actual output length
func TestDecodeExportResult(t *testing.T) {
	tests := []struct {
		errCode   uint32
		actualLen uint32
	}{
		{0, 0},
		{0, 1},
		{1, 0},
		{0xFFFFFFFF, 0},          // max errCode
		{0, 0xFFFFFFFF},          // max actualLen -- sets bit 63
		{0xDEAD, 0xBEEF},
	}
	for _, tt := range tests {
		// Pack as exports.go does: int64(actualLen)<<32 | errCode
		packed := uint64(tt.actualLen)<<32 | uint64(tt.errCode)

		// Extract using decodeExportResult logic.
		errCode := uint32(packed & 0xFFFFFFFF)
		actualLen := uint32(packed >> 32)

		if errCode != tt.errCode {
			t.Errorf("[%#x] errCode = %d (%#x), want %d (%#x)",
				packed, errCode, errCode, tt.errCode, tt.errCode)
		}
		if actualLen != tt.actualLen {
			t.Errorf("[%#x] actualLen = %d, want %d", packed, actualLen, tt.actualLen)
		}

		// Also test the host.decodeExportResult formula directly.
		rErrCode := uint32(packed & 0xFFFFFFFF)
		rActualLen := uint32(packed >> 32)
		if rErrCode != tt.errCode || rActualLen != tt.actualLen {
			t.Errorf("decodeExportResult formula mismatch for [%#x]", packed)
		}
	}
}

// TestSimpleResultPacking verifies the packSimpleResult format:
//
//	bits 32-63 = extra[0] (32 bits, optional)
//	bits 0-31  = errCode (32 bits)
//
// See packSimpleResult in internal/host/memory.go.
func TestSimpleResultPacking(t *testing.T) {
	tests := []struct {
		errCode byte
		extra   uint32
	}{
		{0, 0},
		{1, 0},
		{0xFF, 0},
		{0, 0xFFFFFFFF},
		{0xAB, 0xCDEF},
	}
	for _, tt := range tests {
		var v uint64
		if tt.extra != 0 || tt.errCode == 0 { // match packSimpleResult logic
			v = uint64(tt.extra) << 32
		}
		packed := int64(v | uint64(tt.errCode))

		// Decode with the uint64-based formula (as host does).
		decodedErr := uint32(uint64(packed) & 0xFFFFFFFF)
		decodedExtra := uint32(uint64(packed) >> 32)

		if decodedErr != uint32(tt.errCode) {
			t.Errorf("[%#x] errCode = %d, want %d", uint64(packed), decodedErr, tt.errCode)
		}
		if decodedExtra != tt.extra {
			t.Errorf("[%#x] extra = %d, want %d", uint64(packed), decodedExtra, tt.extra)
		}
	}
}

// ---- needsTime / needsUnsafe / needsJSON / outBufNames edge cases ----

func TestNeedsTimeTrue(t *testing.T) {
	usage := &UsageInfo{Used: map[string]bool{"cleat_call_heartbeat": true},
		Funcs: []HostFunction{{ImportName: "cleat_call_heartbeat", FieldName: "DurableCallWithHeartbeat"}}}
	if !needsTime(usage) {
		t.Error("DurableCallWithHeartbeat uses time.Duration, expected needsTime=true")
	}
}

func TestNeedsTimeFalse(t *testing.T) {
	// DurableSleepMs takes int64 (milliseconds), no time.Duration involved.
	usage := &UsageInfo{Used: map[string]bool{"cleat_sleep": true},
		Funcs: []HostFunction{{ImportName: "cleat_sleep", FieldName: "DurableSleepMs"}}}
	if needsTime(usage) {
		t.Error("DurableSleepMs should not need time")
	}
}

func TestNeedsFmtFalse(t *testing.T) {
	usage := &UsageInfo{Used: map[string]bool{"cleat_sleep": true},
		Funcs: []HostFunction{{ImportName: "cleat_sleep", FieldName: "DurableSleep"}}}
	if needsFmt(usage) {
		t.Error("cleat_sleep should not need fmt")
	}
}

func TestNeedsJSONFalse(t *testing.T) {
	usage := &UsageInfo{Used: map[string]bool{"cleat_call": true},
		Funcs: []HostFunction{{ImportName: "cleat_call", FieldName: "DurableCall"}}}
	if needsJSON(usage) {
		t.Error("cleat_call should not need json")
	}
}

func TestNeedsUnsafeFalse(t *testing.T) {
	usage := &UsageInfo{Used: map[string]bool{"cleat_log": true},
		Funcs: []HostFunction{{ImportName: "cleat_log", FieldName: "DurableLog"}}}
	if needsUnsafe(usage) {
		t.Error("cleat_log should not need unsafe")
	}
}

func TestOutBufNamesUnknownImport(t *testing.T) {
	if names := outBufNames("nonexistent_import"); names != nil {
		t.Errorf("expected nil for unknown import, got %v", names)
	}
}

// ---- generateExport coverage ----

func TestGenerateExportBasic(t *testing.T) {
	result, cr := loadBasic(t)
	_ = cr
	code := string(GenerateExports("basic", result))
	if !strings.Contains(code, "writeJSONOut") {
		t.Error("expected writeJSONOut function")
	}
	if !strings.Contains(code, "//go:wasmexport place_order") {
		t.Error("expected place_order export")
	}
	if !strings.Contains(code, "//go:wasmexport cancel_order") {
		t.Error("expected cancel_order export")
	}
}

// ---- needsFmt/needsUnsafe edge cases with unmapped FieldName ----

func TestNeedsFmtWithUnmappedField(t *testing.T) {
	usage := &UsageInfo{Used: map[string]bool{"cleat_unknown": true},
		Funcs: []HostFunction{{ImportName: "cleat_unknown", FieldName: "NonExistentField"}}}
	if needsFmt(usage) {
		t.Error("expected false for unmapped field")
	}
}

func TestNeedsUnsafeWithUnmappedField(t *testing.T) {
	usage := &UsageInfo{Used: map[string]bool{"cleat_unknown": true},
		Funcs: []HostFunction{{ImportName: "cleat_unknown", FieldName: "NonExistentField"}}}
	if needsUnsafe(usage) {
		t.Error("expected false for unmapped field")
	}
}

func TestNeedsJSONWithUnmappedField(t *testing.T) {
	usage := &UsageInfo{Used: map[string]bool{"cleat_unknown": true},
		Funcs: []HostFunction{{ImportName: "cleat_unknown", FieldName: "NonExistentField"}}}
	if needsJSON(usage) {
		t.Error("expected false for unmapped field")
	}
}

func TestNeedsTimeWithUnmappedField(t *testing.T) {
	usage := &UsageInfo{Used: map[string]bool{"cleat_unknown": true},
		Funcs: []HostFunction{{ImportName: "cleat_unknown", FieldName: "NonExistentField"}}}
	if needsTime(usage) {
		t.Error("expected false for unmapped field")
	}
}

func TestGenerateExportsResultOnlyReturn(t *testing.T) {
	fset := token.NewFileSet()
	result, err := analyzer.LoadPackages(
		"github.com/rcownie/cleat/testdata/vet-checks/go/e009_init", fset)
	if err != nil {
		t.Fatalf("LoadPackages(e009_init): %v", err)
	}
	code := string(GenerateExports("main", result))
	if !strings.Contains(code, "__r := Workflow(h)") {
		t.Error("expected result-only assignment for Workflow")
	}
	syntaxCheck(t, "GenerateExports(e009)", code)
}

func TestGenerateExportsNilEntryPoint(t *testing.T) {
	result := &analyzer.AnalysisResult{
		TargetPkg:   &analyzer.Package{Types: nil},
		EntryPoints: []string{"nonexistent"},
		Funcs:       map[string]*analyzer.FuncDecl{},
	}
	code := string(GenerateExports("main", result))
	if !strings.Contains(code, "package main") {
		t.Error("expected valid output even with nil entry point")
	}
}

func TestCollectHostCallsCallsNilBody(t *testing.T) {
	fd := &analyzer.FuncDecl{
		Name: "test",
		Ast:  &ast.FuncDecl{Body: nil},
	}
	info := &UsageInfo{}
	collectHostCallsCalls(fd, info)
	// Should not panic when Ast is nil.
}
