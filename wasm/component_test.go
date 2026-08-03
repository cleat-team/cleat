package wasm

import (
	"os"
	"testing"
)

// handCraftedComponent returns a minimal valid component binary for testing.
//
// Structure:
//   - Header: magic + component layer
//   - Core module section: tiny WASM module (empty)
//   - Core instance section: Instantiate module 0 with no args
//   - Component export section: "run" func referencing index 0
func handCraftedComponent() []byte {
	var buf []byte

	// Magic + layer
	buf = append(buf, 0x00, 0x61, 0x73, 0x6d) // "\0asm"
	buf = append(buf, 0x0d, 0x00, 0x01, 0x00) // component layer

	// A tiny valid core WASM module that we'll embed.
	// It has no imports, no exports, just a single empty function body.
	// Magic + version (core wasm v1)
	coreModule := []byte{
		0x00, 0x61, 0x73, 0x6d, // magic
		0x01, 0x00, 0x00, 0x00, // version 1
		// Type section: one empty function type () -> ()
		0x01, 0x04, 0x01, 0x60, 0x00, 0x00,
		// Function section: one function of type 0
		0x03, 0x02, 0x01, 0x00,
		// Export section: export function 0 as "run"
		0x07, 0x05, 0x01, 0x03, 0x72, 0x75, 0x6e, 0x00, 0x00,
		// Code section: one empty body
		0x0a, 0x04, 0x01, 0x02, 0x00, 0x0b,
	}

	// Section 1: core module
	// LEB128 size of the module, then raw bytes
	modSize := uint32(len(coreModule))
	buf = append(buf, 0x01) // section ID
	buf = append(buf, encodeULEB128(modSize)...)
	buf = append(buf, coreModule...)

	// Section 2: core instance (Instantiate module 0, no args)
	buf = append(buf, 0x02) // section ID
	instPayload := []byte{
		0x01, // count: 1 instance
		0x00, // discriminator: Instantiate
		0x00, // module_index: 0
		0x00, // args: empty vec
	}
	buf = append(buf, encodeULEB128(uint32(len(instPayload)))...)
	buf = append(buf, instPayload...)

	// Section 11 (0x0b): component export
	buf = append(buf, 0x0b) // section ID
	exportPayload := []byte{
		0x01,       // count: 1 export
		0x00, 0x03, // name length: 3 (big-endian)
		0x72, 0x75, 0x6e, // "run"
		0x01, // sort: func
		0x00, // index: 0
		0x00, // no type reference
	}
	buf = append(buf, encodeULEB128(uint32(len(exportPayload)))...)
	buf = append(buf, exportPayload...)

	return buf
}

func TestComponentHandCrafted(t *testing.T) {
	data := handCraftedComponent()

	bundle, err := ParseComponentBundle(data)
	if err != nil {
		t.Fatalf("ParseComponentBundle failed: %v", err)
	}

	if len(bundle.Modules) != 1 {
		t.Errorf("expected 1 module, got %d", len(bundle.Modules))
	}

	if len(bundle.Instances) != 1 {
		t.Errorf("expected 1 instance, got %d", len(bundle.Instances))
	}

	inst := bundle.Instances[0]
	if inst.ModuleIndex != 0 {
		t.Errorf("expected ModuleIndex=0, got %d", inst.ModuleIndex)
	}

	exp, ok := bundle.Exports["run"]
	if !ok {
		t.Fatal("expected export 'run' to exist")
	}
	if exp.Name != "run" {
		t.Errorf("expected export name 'run', got %q", exp.Name)
	}
	if exp.Kind != 0x01 {
		t.Errorf("expected export kind 0x01 (func), got 0x%02x", exp.Kind)
	}
	if exp.ExportIndex != 0 {
		t.Errorf("expected ExportIndex 0, got %d", exp.ExportIndex)
	}
}

func TestComponentBadMagic(t *testing.T) {
	_, err := ParseComponentBundle([]byte{0x00, 0x00, 0x00, 0x00, 0x0d, 0x00, 0x01, 0x00})
	if err == nil {
		t.Fatal("expected error for bad magic")
	}
}

func TestComponentBadLayer(t *testing.T) {
	_, err := ParseComponentBundle([]byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00})
	if err == nil {
		t.Fatal("expected error for bad layer (core wasm v1)")
	}
}

func TestComponentTooShort(t *testing.T) {
	_, err := ParseComponentBundle([]byte{0x00, 0x61, 0x73})
	if err == nil {
		t.Fatal("expected error for too-short input")
	}
}

func TestComponentWithImports(t *testing.T) {
	var buf []byte

	// Magic + layer
	buf = append(buf, 0x00, 0x61, 0x73, 0x6d)
	buf = append(buf, 0x0d, 0x00, 0x01, 0x00)

	// A tiny core module (just header, no sections)
	coreMod := []byte{
		0x00, 0x61, 0x73, 0x6d,
		0x01, 0x00, 0x00, 0x00,
		// No sections — empty module
	}

	// Section 1: core module
	buf = append(buf, 0x01)
	buf = append(buf, encodeULEB128(uint32(len(coreMod)))...)
	buf = append(buf, coreMod...)

	// Section 10 (0x0a): component import — "wasi:cli/environment@0.2.0"
	buf = append(buf, 0x0a)
	importPayload := []byte{
		0x01,       // count: 1
		0x00, 0x1a, // name length: 26 (big-endian)
		0x77, 0x61, 0x73, 0x69, 0x3a, 0x63, 0x6c, 0x69, // "wasi:cli"
		0x2f,                                           // "/"
		0x65, 0x6e, 0x76, 0x69, 0x72, 0x6f, 0x6e, 0x6d, // "environm"
		0x65, 0x6e, 0x74, // "ent"
		0x40, 0x30, 0x2e, 0x32, 0x2e, 0x30, // "@0.2.0"
		0x05, // sort (instance)
		0x00, // type index: 0
	}
	buf = append(buf, encodeULEB128(uint32(len(importPayload)))...)
	buf = append(buf, importPayload...)

	// Section 11 (0x0b): component export — "run"
	buf = append(buf, 0x0b)
	exportPayload := []byte{
		0x01,       // count: 1
		0x00, 0x03, // name length: 3
		0x72, 0x75, 0x6e, // "run"
		0x01, // sort: func
		0x00, // index: 0
		0x00, // no type
	}
	buf = append(buf, encodeULEB128(uint32(len(exportPayload)))...)
	buf = append(buf, exportPayload...)

	bundle, err := ParseComponentBundle(buf)
	if err != nil {
		t.Fatalf("ParseComponentBundle failed: %v", err)
	}

	if len(bundle.ImportModules) != 1 {
		t.Fatalf("expected 1 import module, got %d", len(bundle.ImportModules))
	}
	if bundle.ImportModules[0] != "wasi:cli/environment@0.2.0" {
		t.Errorf("expected import 'wasi:cli/environment@0.2.0', got %q", bundle.ImportModules[0])
	}

	if exp, ok := bundle.Exports["run"]; !ok {
		t.Fatal("expected export 'run'")
	} else if exp.Kind != 0x01 {
		t.Errorf("expected kind 0x01, got 0x%02x", exp.Kind)
	}
}

// TestComponentPythonBinary tests parsing of the real componentize-py binary.
func TestComponentPythonBinary(t *testing.T) {
	data, err := os.ReadFile("/tmp/test_python.wasm")
	if err != nil {
		t.Skip("test_python.wasm not available:", err)
	}

	bundle, err := ParseComponentBundle(data)
	if err != nil {
		t.Fatalf("ParseComponentBundle failed: %v", err)
	}

	// Should have 14 core modules.
	if len(bundle.Modules) != 14 {
		t.Errorf("expected 14 modules, got %d", len(bundle.Modules))
	}

	// The first module (CPython) should be ~12MB.
	if len(bundle.Modules) > 0 && len(bundle.Modules[0]) < 10_000_000 {
		t.Errorf("expected first module to be ~12MB (CPython), got %d bytes", len(bundle.Modules[0]))
	}

	// Total module data should be >12MB.
	totalModBytes := 0
	for i, mod := range bundle.Modules {
		totalModBytes += len(mod)
		t.Logf("  module[%d]: %d bytes", i, len(mod))
	}
	t.Logf("  total module bytes: %d", totalModBytes)

	// Should have core instances.
	if len(bundle.Instances) == 0 {
		t.Fatal("expected at least one core instance")
	}

	// Count instantiate vs from-exports instances.
	instCount := 0
	fromCount := 0
	for _, inst := range bundle.Instances {
		if inst.ModuleIndex >= 0 {
			instCount++
		} else {
			fromCount++
		}
	}
	t.Logf("  instances: %d instantiate, %d from-exports (total %d)",
		instCount, fromCount, len(bundle.Instances))

	if instCount == 0 {
		t.Error("expected at least one Instantiate instance")
	}
	if fromCount == 0 {
		t.Error("expected at least one FromExports instance")
	}

	// Verify instantiate DAG: instance 0 should be Instantiate of module 12.
	if len(bundle.Instances) > 0 {
		inst0 := bundle.Instances[0]
		if inst0.ModuleIndex != 12 {
			t.Logf("  note: instance[0].ModuleIndex=%d (expected 12)", inst0.ModuleIndex)
		}
	}

	// Check for the "run" export.
	exp, ok := bundle.Exports["run"]
	if !ok {
		t.Fatal("expected export 'run'")
	}
	t.Logf("  export 'run': kind=0x%02x idx=%d", exp.Kind, exp.ExportIndex)

	// Should have component imports (WASI).
	if len(bundle.ImportModules) == 0 {
		t.Error("expected at least one component import")
	} else {
		t.Logf("  first import: %q", bundle.ImportModules[0])
		t.Logf("  total imports: %d", len(bundle.ImportModules))
	}
}

// ---- PatchEmptyImportModuleName tests ----

func TestPatchEmptyImportModuleName_NoImports(t *testing.T) {
	// WASM with no import section — should return original bytes unchanged.
	wasm := memTestWasm()
	result := PatchEmptyImportModuleName(wasm, "env")
	if len(result) != len(wasm) {
		t.Errorf("expected same length, got %d vs %d", len(result), len(wasm))
	}
	// Verify the returned binary is still valid WASM.
	if len(result) < 8 || string(result[0:4]) != "\x00asm" {
		t.Error("result is not valid WASM")
	}
}

func TestPatchEmptyImportModuleName_AllNamed(t *testing.T) {
	// All imports have non-empty module names — should return original unchanged.
	wasm := makeWasmWithImports([]struct{ module, name string }{
		{"env", "cleat_call"},
		{"wasi_snapshot_preview1", "proc_exit"},
	})
	result := PatchEmptyImportModuleName(wasm, "replacement")
	if len(result) != len(wasm) {
		t.Errorf("expected same length for all-named imports, got %d vs %d", len(result), len(wasm))
	}
	// Verify the import module names were preserved (not corrupted).
	names, err := readImportModuleNames(result)
	if err != nil {
		t.Fatalf("readImportModuleNames: %v", err)
	}
	if err != nil {
		t.Fatalf("readImportModuleNames: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("expected 2 imports, got %d", len(names))
	}
	if names[0] != "env" {
		t.Errorf("expected first import module 'env', got %q", names[0])
	}
	if names[1] != "wasi_snapshot_preview1" {
		t.Errorf("expected second import module 'wasi_snapshot_preview1', got %q", names[1])
	}
}

func TestPatchEmptyImportModuleName_EmptyNames(t *testing.T) {
	// Build a WASM binary with an import section containing an empty module name.
	content := encodeULEB128(1) // count: 1 import
	// Module name: length 0 (empty)
	content = append(content, encodeULEB128(0)...)
	// Field name: "durable-call"
	content = append(content, encodeULEB128(uint32(len("durable-call")))...)
	content = append(content, []byte("durable-call")...)
	// Kind: func
	content = append(content, 0x00)
	// Type index: 0
	content = append(content, encodeULEB128(0)...)

	size := encodeULEB128(uint32(len(content)))
	section := []byte{2}
	section = append(section, size...)
	section = append(section, content...)
	wasm := memTestWasm(section)

	result := PatchEmptyImportModuleName(wasm, "replacement")
	if len(result) == len(wasm) {
		t.Error("expected modified length for empty module name import")
	}

	// Verify the empty module name was replaced.
	modNames, err := readImportModuleNames(result)
	if err != nil {
		t.Fatalf("readImportModuleNames failed: %v", err)
	}
	if len(modNames) != 1 {
		t.Fatalf("expected 1 module name, got %d", len(modNames))
	}
	if modNames[0] != "replacement" {
		t.Errorf("expected module name 'replacement', got %q", modNames[0])
	}
}

// ---- parseCoreInstanceSection tests ----

func TestParseCoreInstanceSection_Truncated(t *testing.T) {
	// Build a component binary with a truncated core instance section payload.
	var buf []byte
	// Magic + component layer
	buf = append(buf, 0x00, 0x61, 0x73, 0x6d)
	buf = append(buf, 0x0d, 0x00, 0x01, 0x00)
	// Section 2: core instance — truncated payload (only count, no instance data)
	instPayload := []byte{
		0x01, // count: 1 instance
		// Missing discriminator and rest
	}
	buf = append(buf, 0x02)
	buf = append(buf, encodeULEB128(uint32(len(instPayload)))...)
	buf = append(buf, instPayload...)

	_, err := ParseComponentBundle(buf)
	if err == nil {
		t.Fatal("expected error for truncated core instance section")
	}
}

// ---- parseComponentExportSection tests ----

func TestParseComponentExportSection_Default(t *testing.T) {
	// Build a component binary with various export sorts to test default handling.
	var buf []byte
	// Magic + component layer
	buf = append(buf, 0x00, 0x61, 0x73, 0x6d)
	buf = append(buf, 0x0d, 0x00, 0x01, 0x00)

	// A tiny core module (needed for the bundle to parse).
	coreModule := []byte{
		0x00, 0x61, 0x73, 0x6d, // magic
		0x01, 0x00, 0x00, 0x00, // version 1
		// Type section: empty func type
		0x01, 0x04, 0x01, 0x60, 0x00, 0x00,
		// Function section: one function
		0x03, 0x02, 0x01, 0x00,
		// Export section: export function 0 as "run"
		0x07, 0x05, 0x01, 0x03, 0x72, 0x75, 0x6e, 0x00, 0x00,
		// Code section: empty body
		0x0a, 0x04, 0x01, 0x02, 0x00, 0x0b,
	}
	// Section 1: core module
	buf = append(buf, 0x01)
	buf = append(buf, encodeULEB128(uint32(len(coreModule)))...)
	buf = append(buf, coreModule...)

	// Section 2: core instance
	buf = append(buf, 0x02)
	instPayload := []byte{
		0x01, // count: 1
		0x00, // Instantiate
		0x00, // module index: 0
		0x00, // args: empty
	}
	buf = append(buf, encodeULEB128(uint32(len(instPayload)))...)
	buf = append(buf, instPayload...)

	// Section 11 (0x0b): component export — with a table export (sort=0x02, default case)
	buf = append(buf, 0x0b)
	exportPayload := []byte{
		0x02,       // count: 2 exports
		0x00, 0x03, // name length: 3
		0x72, 0x75, 0x6e, // "run"
		0x01,       // sort: func
		0x00,       // index: 0
		0x00,       // no type reference
		0x00, 0x05, // name length: 5
		0x74, 0x61, 0x62, 0x6c, 0x65, // "table"
		0x02, // sort: table (uses default case in parser)
		0x01, // index: 1
	}
	buf = append(buf, encodeULEB128(uint32(len(exportPayload)))...)
	buf = append(buf, exportPayload...)

	bundle, err := ParseComponentBundle(buf)
	if err != nil {
		t.Fatalf("ParseComponentBundle failed: %v", err)
	}

	// Verify both exports are present.
	exp, ok := bundle.Exports["run"]
	if !ok {
		t.Fatal("expected export 'run'")
	}
	if exp.Kind != 0x01 {
		t.Errorf("expected export kind 0x01, got 0x%02x", exp.Kind)
	}
	if exp.ExportIndex != 0 {
		t.Errorf("expected ExportIndex 0, got %d", exp.ExportIndex)
	}

	exp2, ok := bundle.Exports["table"]
	if !ok {
		t.Fatal("expected export 'table'")
	}
	if exp2.Kind != 0x02 {
		t.Errorf("expected export kind 0x02, got 0x%02x", exp2.Kind)
	}
	if exp2.ExportIndex != 1 {
		t.Errorf("expected ExportIndex 1, got %d", exp2.ExportIndex)
	}
}
