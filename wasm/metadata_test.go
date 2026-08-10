package wasm

import (
	"testing"
)

func TestWriteMetadata_InvalidWasmBytes(t *testing.T) {
	meta := &Metadata{
		WorkflowName:         "test",
		WorkflowVersion:      1,
		ABIVersion:           1,
		MinCompatibleVersion: 1,
	}
	// Too-short bytes should cause writeCustomSection to absorb the error
	// and append the section to the original bytes.
	result, err := WriteMetadata(nil, meta)
	if err != nil {
		t.Fatalf("WriteMetadata with nil bytes: %v", err)
	}
	if len(result) == 0 {
		t.Error("expected non-empty result even with nil input")
	}

	result2, err := WriteMetadata([]byte("short"), meta)
	if err != nil {
		t.Fatalf("WriteMetadata with short bytes: %v", err)
	}
	if len(result2) == 0 {
		t.Error("expected non-empty result even with short input")
	}
}

func TestWriteMetadata_InvalidMeta(t *testing.T) {
	// Valid WASM header: magic (4) + version (4) = 8 bytes.
	wasmHeader := []byte{
		0x00, 0x61, 0x73, 0x6d, // magic
		0x01, 0x00, 0x00, 0x00, // version
	}
	meta := &Metadata{
		WorkflowName:         "test-workflow",
		WorkflowVersion:      1,
		ABIVersion:           1,
		MinCompatibleVersion: 1,
	}
	result, err := WriteMetadata(wasmHeader, meta)
	if err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}
	if len(result) <= len(wasmHeader) {
		t.Error("expected metadata section appended")
	}

	// Round-trip: read back.
	readMeta, err := ReadMetadata(result)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if readMeta.WorkflowName != "test-workflow" {
		t.Errorf("expected name test-workflow, got %s", readMeta.WorkflowName)
	}
	if readMeta.WorkflowVersion != 1 {
		t.Errorf("expected version 1, got %d", readMeta.WorkflowVersion)
	}
}

func TestWriteMetadata_ReplaceExistingSection(t *testing.T) {
	wasmHeader := []byte{
		0x00, 0x61, 0x73, 0x6d,
		0x01, 0x00, 0x00, 0x00,
	}
	meta1 := &Metadata{
		WorkflowName:         "v1",
		WorkflowVersion:      1,
		ABIVersion:           1,
		MinCompatibleVersion: 1,
	}
	wasm1, err := WriteMetadata(wasmHeader, meta1)
	if err != nil {
		t.Fatalf("WriteMetadata v1: %v", err)
	}

	meta2 := &Metadata{
		WorkflowName:         "v2",
		WorkflowVersion:      2,
		ABIVersion:           1,
		MinCompatibleVersion: 1,
	}
	wasm2, err := WriteMetadata(wasm1, meta2)
	if err != nil {
		t.Fatalf("WriteMetadata v2: %v", err)
	}

	readMeta, err := ReadMetadata(wasm2)
	if err != nil {
		t.Fatalf("ReadMetadata after replace: %v", err)
	}
	if readMeta.WorkflowName != "v2" {
		t.Errorf("expected v2, got %s", readMeta.WorkflowName)
	}
}

func TestStripCustomSection_NotFound(t *testing.T) {
	wasmHeader := []byte{
		0x00, 0x61, 0x73, 0x6d,
		0x01, 0x00, 0x00, 0x00,
	}
	_, err := stripCustomSection(wasmHeader, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent section")
	}
}

func TestValidate_Errors(t *testing.T) {
	tests := []struct {
		name   string
		meta   Metadata
		errMsg string
	}{
		{"empty name", Metadata{}, "workflow_name is empty"},
		{"zero version", Metadata{WorkflowName: "x"}, "workflow_version must be positive"},
		{"zero abi", Metadata{WorkflowName: "x", WorkflowVersion: 1}, "abi_version must be positive"},
		{"zero min", Metadata{WorkflowName: "x", WorkflowVersion: 1, ABIVersion: 1}, "min_compatible_version must be positive"},
		{"min > abi", Metadata{WorkflowName: "x", WorkflowVersion: 1, ABIVersion: 1, MinCompatibleVersion: 2}, "min_compatible_version (2) exceeds abi_version (1)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.meta.Validate()
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestReadMetadata_InvalidWasm(t *testing.T) {
	_, err := ReadMetadata([]byte{0, 1, 2})
	if err == nil {
		t.Error("expected error for invalid wasm bytes")
	}
	_, err = ReadMetadata(nil)
	if err == nil {
		t.Error("expected error for nil wasm bytes")
	}
}

func TestDecodeULEB128_Truncated(t *testing.T) {
	_, n := decodeULEB128(nil)
	if n != 0 {
		t.Errorf("expected 0 for nil input, got %d", n)
	}
	_, n = decodeULEB128([]byte{0x80})
	if n != 0 {
		t.Errorf("expected 0 for truncated input, got %d", n)
	}
}

func TestReadCustomSection_CorruptNameLen(t *testing.T) {
	// WASM header + custom section where the name length ULEB128 has the
	// continuation bit set on the last byte (truncated). This exercises the
	// nn <= 0 error path in readCustomSection (line ~107).
	wasm := []byte{
		0x00, 0x61, 0x73, 0x6d, // magic
		0x01, 0x00, 0x00, 0x00, // version
		0x00, // section ID = custom
		0x01, // section size = 1
		0x80, // truncated name length (continuation bit, no more bytes)
	}
	_, err := ReadMetadata(wasm)
	if err == nil {
		t.Error("expected error for corrupt name length in readCustomSection")
	}
}

func TestStripCustomSection_CorruptNameLen(t *testing.T) {
	wasm := []byte{
		0x00, 0x61, 0x73, 0x6d,
		0x01, 0x00, 0x00, 0x00,
		0x00, // section ID = custom
		0x01, // section size = 1
		0x80, // truncated name length ULEB128
	}
	_, err := stripCustomSection(wasm, "cleat.metadata")
	if err == nil {
		t.Error("expected error for corrupt name length in stripCustomSection")
	}
}

func TestStripCustomSection_NameOverflow(t *testing.T) {
	// Custom section where the declared name length exceeds the remaining bytes.
	wasm := []byte{
		0x00, 0x61, 0x73, 0x6d,
		0x01, 0x00, 0x00, 0x00,
		0x00, // section ID = custom
		0x02, // section size = 2
		0x05, // name length = 5 (but only 1 byte follows)
		0x00, // partial name byte
	}
	_, err := stripCustomSection(wasm, "cleat.metadata")
	if err == nil {
		t.Error("expected error for name overflow in stripCustomSection")
	}
}

func TestReadCustomSection_NameOverflow(t *testing.T) {
	wasm := []byte{
		0x00, 0x61, 0x73, 0x6d,
		0x01, 0x00, 0x00, 0x00,
		0x00, // section ID = custom
		0x02, // section size = 2
		0x05, // name length = 5 (but only 1 byte follows)
		0x00, // partial name byte
	}
	_, err := ReadMetadata(wasm)
	if err == nil {
		t.Error("expected error for name overflow in readCustomSection")
	}
}

func TestEffectivePolicy_Defaults(t *testing.T) {
	tests := []struct {
		name     string
		meta     Metadata
		expected string
	}{
		{
			name:     "empty policy, no child versions -> latest",
			meta:     Metadata{ChildBindingPolicy: ""},
			expected: "latest",
		},
		{
			name:     "empty policy, with child versions -> frozen",
			meta:     Metadata{ChildVersions: map[string]int{"child1": 1}},
			expected: "frozen",
		},
		{
			name:     "explicit frozen",
			meta:     Metadata{ChildBindingPolicy: "frozen"},
			expected: "frozen",
		},
		{
			name:     "explicit stable",
			meta:     Metadata{ChildBindingPolicy: "stable"},
			expected: "stable",
		},
		{
			name:     "explicit latest",
			meta:     Metadata{ChildBindingPolicy: "latest"},
			expected: "latest",
		},
		{
			name:     "explicit tag:canary",
			meta:     Metadata{ChildBindingPolicy: "tag:canary"},
			expected: "tag:canary",
		},
		{
			name:     "empty policy with child versions still wins over explicit empty",
			meta:     Metadata{ChildVersions: map[string]int{"c": 2}},
			expected: "frozen",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.meta.EffectivePolicy()
			if got != tc.expected {
				t.Errorf("EffectivePolicy() = %q, want %q", got, tc.expected)
			}
		})
	}
}

// ---- DetectLanguage tests ----

func TestDetectLanguage_Go(t *testing.T) {
	// Build a WASM binary with wasi_snapshot_preview1 import (Go compiles with WASI).
	imports := []struct{ module, name string }{
		{"wasi_snapshot_preview1", "proc_exit"},
	}
	wasm := makeWasmWithImports(imports)
	lang := DetectLanguage(wasm)
	if lang != "go" {
		t.Errorf("expected 'go', got %q", lang)
	}
}

func TestDetectLanguage_Unknown(t *testing.T) {
	// Build a WASM binary with imports that don't match any known pattern.
	imports := []struct{ module, name string }{
		{"custom_module", "custom_func"},
	}
	wasm := makeWasmWithImports(imports)
	lang := DetectLanguage(wasm)
	// Default should be "go" when no language-specific imports are found.
	if lang != "go" {
		t.Errorf("expected default 'go', got %q", lang)
	}
}

func TestDetectLanguage_InvalidWasm(t *testing.T) {
	lang := DetectLanguage(nil)
	if lang != "go" {
		t.Errorf("expected default 'go' for nil, got %q", lang)
	}
	lang = DetectLanguage([]byte{0x00, 0x61})
	if lang != "go" {
		t.Errorf("expected default 'go' for short binary, got %q", lang)
	}
}

// ---- ReadImportModuleNames tests ----

func TestReadImportModuleNames(t *testing.T) {
	imports := []struct{ module, name string }{
		{"env", "cleat_call"},
		{"wasi_snapshot_preview1", "proc_exit"},
	}
	wasm := makeWasmWithImports(imports)

	modNames, err := readImportModuleNames(wasm)
	if err != nil {
		t.Fatalf("readImportModuleNames failed: %v", err)
	}
	if len(modNames) != 2 {
		t.Fatalf("expected 2 module names, got %d", len(modNames))
	}
	if modNames[0] != "env" {
		t.Errorf("expected module[0]='env', got %q", modNames[0])
	}
	if modNames[1] != "wasi_snapshot_preview1" {
		t.Errorf("expected module[1]='wasi_snapshot_preview1', got %q", modNames[1])
	}
}

func TestReadImportModuleNames_NoImportSection(t *testing.T) {
	wasm := memTestWasm()
	_, err := readImportModuleNames(wasm)
	if err == nil {
		t.Fatal("expected error for binary with no import section")
	}
}

func TestReadImportModuleNames_TooShort(t *testing.T) {
	_, err := readImportModuleNames([]byte{0x00, 0x61})
	if err == nil {
		t.Fatal("expected error for too-short binary")
	}
}

func TestDetectLanguage_Python(t *testing.T) {
	// Python component binaries have component model header.
	wasm := []byte{
		0x00, 0x61, 0x73, 0x6d, // magic
		0x0d, 0x00, 0x01, 0x00, // component layer
	}
	lang := DetectLanguage(wasm)
	if lang != "python" {
		t.Errorf("expected 'python', got %q", lang)
	}
}

func TestDetectLanguage_Python_WithCleatImports(t *testing.T) {
	// Core WASM with cleat: imports but no component header.
	imports := []struct{ module, name string }{
		{"cleat:host-calls/durable-call", "durable-call"},
	}
	wasm := makeWasmWithImports(imports)
	lang := DetectLanguage(wasm)
	if lang != "python" {
		t.Errorf("expected 'python' for WIT-style imports, got %q", lang)
	}
}

func TestDetectLanguage_Java(t *testing.T) {
	// TeaVM-compiled Java modules import from "teavm".
	imports := []struct{ module, name string }{
		{"teavm", "someFunc"},
	}
	wasm := makeWasmWithImports(imports)
	lang := DetectLanguage(wasm)
	if lang != "java" {
		t.Errorf("expected 'java', got %q", lang)
	}
}

func TestDetectLanguage_AssemblyScript(t *testing.T) {
	// AssemblyScript modules import env.abort.
	imports := []struct{ module, name string }{
		{"env", "abort"},
	}
	wasm := makeWasmWithImports(imports)
	lang := DetectLanguage(wasm)
	if lang != "assemblyscript" {
		t.Errorf("expected 'assemblyscript', got %q", lang)
	}
}

func TestHasWasiImports(t *testing.T) {
	imports := []struct{ module, name string }{
		{"wasi_snapshot_preview1", "proc_exit"},
	}
	wasm := makeWasmWithImports(imports)
	if !HasWasiImports(wasm) {
		t.Error("expected HasWasiImports to be true")
	}

	noWasi := memTestWasm()
	if HasWasiImports(noWasi) {
		t.Error("expected HasWasiImports to be false")
	}
}

func TestHasImport(t *testing.T) {
	imports := []struct{ module, name string }{
		{"env", "cleat_call"},
	}
	wasm := makeWasmWithImports(imports)
	if !HasImport(wasm, "env", "cleat_call") {
		t.Error("expected HasImport(env, cleat_call) to be true")
	}
	if HasImport(wasm, "env", "nonexistent") {
		t.Error("expected HasImport(env, nonexistent) to be false")
	}
}
