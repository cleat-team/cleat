package wasm

import (
	"strings"
	"testing"
)

// ---- readLEB128U32 ----

func TestReadLEB128U32(t *testing.T) {
	tests := []struct {
		name     string
		input    []byte
		wantVal  uint32
		wantRead int
	}{
		{"zero", []byte{0x00}, 0, 1},
		{"one", []byte{0x01}, 1, 1},
		{"127 (max single byte)", []byte{0x7f}, 127, 1},
		{"128 (two bytes)", []byte{0x80, 0x01}, 128, 2},
		{"300", []byte{0xac, 0x02}, 300, 2},
		{"0xFFFF (three bytes)", []byte{0xff, 0xff, 0x03}, 0xFFFF, 3},
		{"max uint32 (five bytes)", []byte{0xff, 0xff, 0xff, 0xff, 0x0f}, 0xFFFFFFFF, 5},
		{"empty slice", nil, 0, 0},
		{"truncated single byte", []byte{0x80}, 0, 0},
		{"overflow (six continuation bytes)", []byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x00}, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotVal, gotRead := readLEB128U32(tt.input)
			if gotVal != tt.wantVal || gotRead != tt.wantRead {
				t.Errorf("readLEB128U32(%v) = (%d, %d), want (%d, %d)",
					tt.input, gotVal, gotRead, tt.wantVal, tt.wantRead)
			}
		})
	}
}

// ---- readName ----

func TestReadName(t *testing.T) {
	tests := []struct {
		name     string
		input    []byte
		wantStr  string
		wantRead int
	}{
		{"valid name", []byte{0x03, 'f', 'o', 'o'}, "foo", 4},
		{"empty name", []byte{0x00}, "", 1},
		{"single char", []byte{0x01, 'x'}, "x", 2},
		{"truncated length", []byte{0x80}, "", 0},
		{"length exceeds data", []byte{0x05, 'a', 'b'}, "", 0},
		{"empty data", nil, "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotStr, gotRead := readName(tt.input)
			if gotStr != tt.wantStr || gotRead != tt.wantRead {
				t.Errorf("readName(%v) = (%q, %d), want (%q, %d)",
					tt.input, gotStr, gotRead, tt.wantStr, tt.wantRead)
			}
		})
	}
}

// ---- normalizeImportName ----

func TestNormalizeImportName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"cleat_call_retry", "cleat_call"},
		{"cleat_call_heartbeat", "cleat_call"},
		{"cleat_fetch", "cleat_call"},
		{"cleat_child_workflow_with_options", "cleat_child_workflow"},
		{"cleat_child_workflow_in_schema", "cleat_child_workflow"},
		{"cleat_continue_as_new_versioned", "cleat_continue_as_new"},
		{"cleat_send_signal_and_wait", "cleat_await_signals"},
		{"cleat_reply_to_signal", "cleat_await_signals"},
		{"cleat_signal_workflow", "cleat_await_signals"},
		{"cleat_acquire_lock", "cleat_acquire_lock"},
		{"cleat_release_lock", "cleat_acquire_lock"},
		{"cleat_set_state", "set_query_state"},
		{"cleat_get_state", "set_query_state"},
		{"cleat_delete_state", "set_query_state"},
		{"cleat_incr_state", "set_query_state"},
		{"cleat_has_state", "set_query_state"},
		{"cleat_list_state", "set_query_state"},
		{"schedule_invoke", "cleat_sleep"},
		{"cleat_call", "cleat_call"},
		{"cleat_sleep", "cleat_sleep"},
		{"cleat_log", "cleat_log"},
		{"unknown_import", "unknown_import"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := normalizeImportName(tt.input)
			if got != tt.want {
				t.Errorf("normalizeImportName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// ---- IsCleatHostFunction ----

func TestIsCleatHostFunction(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"cleat_call", true},
		{"cleat_sleep", true},
		{"cleat_await_signals", true},
		{"set_query_state", true},
		{"set_something", true},
		{"plugin_foo", true},
		{"plugin_bar_baz", true},
		{"schedule_work", true},
		{"schedule_invoke", true},
		{"something_else", false},
		{"", false},
		{"CLEAT_call", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsCleatHostFunction(tt.name)
			if got != tt.want {
				t.Errorf("IsCleatHostFunction(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

// ---- WASM binary helpers ----

type importEntry struct {
	module []byte
	name   []byte
	kind   byte
	desc   []byte
}

// makeTestWasmSection builds a WASM section: sectionID + section content.
func makeTestWasmSection(id byte, content []byte) []byte {
	size := encodeULEB128(uint32(len(content)))
	out := []byte{id}
	out = append(out, size...)
	out = append(out, content...)
	return out
}

// makeTestWasmBinary builds a full WASM binary from a header and sections.
func makeTestWasmBinary(sections ...[]byte) []byte {
	out := make([]byte, 8)
	copy(out, wasmHeader())
	for _, s := range sections {
		out = append(out, s...)
	}
	return out
}

// makeImportSection creates a WASM import section from import entries.
func makeImportSection(imports ...importEntry) []byte {
	count := encodeULEB128(uint32(len(imports)))
	var content []byte
	content = append(content, count...)
	for _, imp := range imports {
		content = append(content, encodeULEB128(uint32(len(imp.module)))...)
		content = append(content, imp.module...)
		content = append(content, encodeULEB128(uint32(len(imp.name)))...)
		content = append(content, imp.name...)
		content = append(content, imp.kind)
		content = append(content, imp.desc...)
	}
	return makeTestWasmSection(2, content)
}

// makeMemorySection creates a WASM memory section with the given initial pages.
func makeMemorySection(initialPages uint32) []byte {
	content := encodeULEB128(1)
	content = append(content, encodeULEB128(0)...) // flags (no max)
	content = append(content, encodeULEB128(initialPages)...)
	return makeTestWasmSection(5, content)
}

// ---- ScanWasmImports ----

func TestScanWasmImports_ValidFunctionImport(t *testing.T) {
	sec := makeImportSection(importEntry{[]byte("env"), []byte("cleat_call"), 0, encodeULEB128(0)})
	wasm := makeTestWasmBinary(sec)

	imports, err := ScanWasmImports(wasm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(imports) != 1 {
		t.Fatalf("expected 1 import, got %d", len(imports))
	}
	if imports[0].Module != "env" {
		t.Errorf("expected module 'env', got %q", imports[0].Module)
	}
	if imports[0].Name != "cleat_call" {
		t.Errorf("expected name 'cleat_call', got %q", imports[0].Name)
	}
	if imports[0].Kind != 0 {
		t.Errorf("expected kind 0 (func), got %d", imports[0].Kind)
	}
}

func TestScanWasmImports_EmptyImportSection(t *testing.T) {
	sec := makeImportSection()
	wasm := makeTestWasmBinary(sec)

	imports, err := ScanWasmImports(wasm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(imports) != 0 {
		t.Errorf("expected 0 imports, got %d", len(imports))
	}
}

func TestScanWasmImports_NoImportSection(t *testing.T) {
	wasm := makeTestWasmBinary()

	imports, err := ScanWasmImports(wasm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if imports != nil && len(imports) != 0 {
		t.Errorf("expected empty imports, got %d", len(imports))
	}
}

func TestScanWasmImports_TableImport(t *testing.T) {
	sec := makeImportSection(importEntry{[]byte("env"), []byte("table_import"), 1, []byte{0x70, 0x00, 0x05}})
	wasm := makeTestWasmBinary(sec)

	imports, err := ScanWasmImports(wasm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(imports) != 1 {
		t.Fatalf("expected 1 import, got %d", len(imports))
	}
	if imports[0].Kind != 1 {
		t.Errorf("expected kind 1 (table), got %d", imports[0].Kind)
	}
}

func TestScanWasmImports_TableImportWithMax(t *testing.T) {
	// flags=0x01 (max present): initial=3, max=16
	desc := []byte{0x70, 0x01, 0x03, 0x10}
	sec := makeImportSection(importEntry{[]byte("env"), []byte("table_wmax"), 1, desc})
	wasm := makeTestWasmBinary(sec)

	imports, err := ScanWasmImports(wasm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(imports) != 1 {
		t.Fatalf("expected 1 import, got %d", len(imports))
	}
	if imports[0].Kind != 1 {
		t.Errorf("expected kind 1 (table), got %d", imports[0].Kind)
	}
}

func TestScanWasmImports_MemoryImport(t *testing.T) {
	desc := append(encodeULEB128(0), encodeULEB128(1)...)
	sec := makeImportSection(importEntry{[]byte("env"), []byte("memory"), 2, desc})
	wasm := makeTestWasmBinary(sec)

	imports, err := ScanWasmImports(wasm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(imports) != 1 {
		t.Fatalf("expected 1 import, got %d", len(imports))
	}
	if imports[0].Kind != 2 {
		t.Errorf("expected kind 2 (memory), got %d", imports[0].Kind)
	}
}

func TestScanWasmImports_GlobalImport(t *testing.T) {
	sec := makeImportSection(importEntry{[]byte("env"), []byte("global_var"), 3, []byte{0x7f, 0x00}})
	wasm := makeTestWasmBinary(sec)

	imports, err := ScanWasmImports(wasm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(imports) != 1 {
		t.Fatalf("expected 1 import, got %d", len(imports))
	}
	if imports[0].Kind != 3 {
		t.Errorf("expected kind 3 (global), got %d", imports[0].Kind)
	}
}

func TestScanWasmImports_MultipleImports(t *testing.T) {
	sec := makeImportSection(
		importEntry{[]byte("env"), []byte("cleat_call"), 0, encodeULEB128(0)},
		importEntry{[]byte("env"), []byte("global_var"), 3, []byte{0x7f, 0x00}},
	)
	wasm := makeTestWasmBinary(sec)

	imports, err := ScanWasmImports(wasm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(imports) != 2 {
		t.Fatalf("expected 2 imports, got %d", len(imports))
	}
}

func TestScanWasmImports_InvalidMagic(t *testing.T) {
	bad := []byte{0xde, 0xad, 0xbe, 0xef, 0x01, 0x00, 0x00, 0x00}
	_, err := ScanWasmImports(bad)
	if err == nil {
		t.Fatal("expected error for invalid magic")
	}
	if !strings.Contains(err.Error(), "magic") {
		t.Errorf("expected 'magic' in error, got: %v", err)
	}
}

func TestScanWasmImports_UnsupportedVersion(t *testing.T) {
	bad := []byte{0x00, 0x61, 0x73, 0x6d, 0x02, 0x00, 0x00, 0x00}
	_, err := ScanWasmImports(bad)
	if err == nil {
		t.Fatal("expected error for unsupported version")
	}
	if !strings.Contains(err.Error(), "version") {
		t.Errorf("expected 'version' in error, got: %v", err)
	}
}

func TestScanWasmImports_TooShort(t *testing.T) {
	_, err := ScanWasmImports([]byte{0x00, 0x61})
	if err == nil {
		t.Fatal("expected error for too-short binary")
	}
}

// ---- FindCleatOrphanedImports ----

func TestFindCleatOrphanedImports_None(t *testing.T) {
	sec := makeImportSection(importEntry{[]byte("env"), []byte("cleat_call"), 0, encodeULEB128(0)})
	wasm := makeTestWasmBinary(sec)

	orphans := FindCleatOrphanedImports(wasm, map[string]bool{"cleat_call": true})
	if len(orphans) != 0 {
		t.Errorf("expected no orphans, got: %v", orphans)
	}
}

func TestFindCleatOrphanedImports_OrphanFound(t *testing.T) {
	sec := makeImportSection(importEntry{[]byte("env"), []byte("cleat_sleep"), 0, encodeULEB128(0)})
	wasm := makeTestWasmBinary(sec)

	orphans := FindCleatOrphanedImports(wasm, map[string]bool{})
	if len(orphans) != 1 {
		t.Fatalf("expected 1 orphan, got %d", len(orphans))
	}
	if !strings.Contains(orphans[0], "cleat_sleep") {
		t.Errorf("orphan message should mention cleat_sleep: %s", orphans[0])
	}
}

func TestFindCleatOrphanedImports_NonEnvIgnored(t *testing.T) {
	sec := makeImportSection(importEntry{[]byte("wasi_snapshot_preview1"), []byte("args_get"), 0, encodeULEB128(0)})
	wasm := makeTestWasmBinary(sec)

	orphans := FindCleatOrphanedImports(wasm, map[string]bool{})
	if len(orphans) != 0 {
		t.Errorf("expected no orphans for non-env imports, got: %v", orphans)
	}
}

func TestFindCleatOrphanedImports_NonFunctionIgnored(t *testing.T) {
	desc := append(encodeULEB128(0), encodeULEB128(1)...)
	sec := makeImportSection(importEntry{[]byte("env"), []byte("memory"), 2, desc})
	wasm := makeTestWasmBinary(sec)

	orphans := FindCleatOrphanedImports(wasm, map[string]bool{})
	if len(orphans) != 0 {
		t.Errorf("expected no orphans for non-function imports, got: %v", orphans)
	}
}

func TestFindCleatOrphanedImports_NormalizedMatch(t *testing.T) {
	// cleat_call_retry should match "cleat_call" via normalization.
	sec := makeImportSection(importEntry{[]byte("env"), []byte("cleat_call_retry"), 0, encodeULEB128(0)})
	wasm := makeTestWasmBinary(sec)

	orphans := FindCleatOrphanedImports(wasm, map[string]bool{"cleat_call": true})
	if len(orphans) != 0 {
		t.Errorf("expected no orphans via normalization, got: %v", orphans)
	}
}

func TestFindCleatOrphanedImports_ErrorScanning(t *testing.T) {
	orphans := FindCleatOrphanedImports(nil, map[string]bool{})
	if len(orphans) != 1 {
		t.Fatalf("expected 1 entry for scan error, got %d", len(orphans))
	}
	if !strings.Contains(orphans[0], "error scanning") {
		t.Errorf("expected 'error scanning' in message: %s", orphans[0])
	}
}

func TestFindCleatOrphanedImports_NonCleatEnvFunc(t *testing.T) {
	sec := makeImportSection(importEntry{[]byte("env"), []byte("some_random_func"), 0, encodeULEB128(0)})
	wasm := makeTestWasmBinary(sec)

	orphans := FindCleatOrphanedImports(wasm, map[string]bool{})
	if len(orphans) != 0 {
		t.Errorf("expected no orphans for non-cleat env imports, got: %v", orphans)
	}
}
