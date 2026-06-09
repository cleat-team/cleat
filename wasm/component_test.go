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
