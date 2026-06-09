package engine

import (
	"context"
	"sync"
	"testing"
)

// minimalComponentWasm returns a minimal valid WASM Component Model binary for
// testing. It embeds a single core module (empty function body exported as
// "run") and a component export of sort=instance pointing to the first instance.
//
// The binary structure:
//   - Magic + component layer (8 bytes)
//   - Core module section: valid WASM v1 module with empty "run" export
//   - Core instance section: Instantiate module 0 with no args
//   - Component export section: "run" as instance export (sort=0x05)
//
// Component export sort is instance (0x05) rather than func (0x01) because
// the component parser sets InstanceIndex=-1 for func exports, which would
// cause a slice bounds panic in executeComponent Step 5.
func minimalComponentWasm() []byte {
	// Helper: LEB128 unsigned encoding (same algorithm as wasm/metadata.go).
	encodeULEB128 := func(v uint32) []byte {
		var buf [5]byte
		n := 0
		for {
			b := byte(v & 0x7f)
			v >>= 7
			if v != 0 {
				b |= 0x80
			}
			buf[n] = b
			n++
			if v == 0 {
				break
			}
		}
		return buf[:n]
	}

	var buf []byte

	// Magic + component model layer.
	buf = append(buf, 0x00, 0x61, 0x73, 0x6d) // "\0asm"
	buf = append(buf, 0x0d, 0x00, 0x01, 0x00) // component layer

	// A minimal valid core WASM module with an empty "run" export.
	// Magic + version + type section (() -> ()) + func section + export section + code section.
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

	// Section 0x01: core module.
	modSize := uint32(len(coreModule))
	buf = append(buf, 0x01)
	buf = append(buf, encodeULEB128(modSize)...)
	buf = append(buf, coreModule...)

	// Section 0x02: core instance (Instantiate module 0, no args).
	instPayload := []byte{
		0x01, // count: 1 instance
		0x00, // discriminator: Instantiate
		0x00, // module_index: 0
		0x00, // args: empty vec
	}
	buf = append(buf, 0x02)
	buf = append(buf, encodeULEB128(uint32(len(instPayload)))...)
	buf = append(buf, instPayload...)

	// Section 0x0b: component export — "run" as instance export (sort=0x05).
	// Using sort=instance (not func) so that InstanceIndex is set to 0,
	// allowing executeComponent Step 5 to find the instantiated module.
	buf = append(buf, 0x0b)
	exportPayload := []byte{
		0x01,       // count: 1 export
		0x00, 0x03, // name length: 3 (big-endian)
		0x72, 0x75, 0x6e, // "run"
		0x05, // sort: instance
		0x00, // index: 0
	}
	buf = append(buf, encodeULEB128(uint32(len(exportPayload)))...)
	buf = append(buf, exportPayload...)

	return buf
}

// TestComponentStdoutStderrRace verifies that concurrent Engine.Execute() calls
// with WASM Component Model binaries do not race on the Runtime's shared
// stdout/stderr buffers.
//
// Before the fix, executeComponent called e.rt.InstantiateModuleNamed() which
// resets and writes the shared Runtime.stdout/Runtime.stderr buffers. Concurrent
// component-model executions would race on those buffers.
//
// After the fix, executeComponent uses per-execution bytes.Buffer instances via
// instantiateModuleNamedWithWriters, matching the wazeroBackend.Execute() pattern.
func TestComponentStdoutStderrRace(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	eng := NewEngine(rt, nil)
	wasmBytes := minimalComponentWasm()

	const goroutines = 10
	const iterations = 10

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				// Execute returns an error because the empty function body
				// produces no results from CallExport, but that's expected.
				// The race condition is in the module instantiation step,
				// which occurs before CallExport and is exercised regardless.
				_, _, _, _, _, err := eng.Execute(ctx, wasmBytes, "run", nil)
				_ = err
			}
		}()
	}

	wg.Wait()
}
