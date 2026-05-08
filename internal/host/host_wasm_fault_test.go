package host

import (
	"context"
	"testing"
)

// ---------------------------------------------------------------------------
// Import resolution failure
// ---------------------------------------------------------------------------

// wasmWithMissingImport returns a WASM module that declares an import from
// "env" for a function the host does not provide.  This tests that the
// instantiation-time error path works (no panic).
func wasmWithMissingImport() []byte {
	bin := []byte{
		0x00, 0x61, 0x73, 0x6d, // magic
		0x01, 0x00, 0x00, 0x00, // version 1
	}
	// Type section: () -> ()
	bin = append(bin,
		0x01, 0x04, // Type, size 4
		0x01,       // 1 type
		0x60,       // functype marker
		0x00,       // 0 params
		0x00,       // 0 results
	)
	// Import section: import "env" "missing_func" (func () -> ())
	bin = append(bin,
		0x02,                     // Import section
		0x14,                     // size: 1+4+13+1+1 = 20 bytes
		0x01,                     // 1 import
		0x03, 0x65, 0x6e, 0x76,  // module "env"
		0x0c, 0x6d, 0x69, 0x73, 0x73, 0x69, 0x6e, 0x67,
		0x5f, 0x66, 0x75, 0x6e, 0x63, // field "missing_func"
		0x00,                     // import kind: func
		0x00,                     // type index 0
	)
	// Export section: empty
	bin = append(bin,
		0x07, 0x01, // Export, size 1
		0x00,       // 0 exports
	)
	return bin
}

func TestWasmMissingImportFailsAtInstantiate(t *testing.T) {
	// A module that imports a function the host does not provide must fail
	// at instantiation time, not panic.
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	wasmBytes := wasmWithMissingImport()

	// Compilation should succeed (imports are resolved at instantiation).
	compiled, err := rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		t.Fatalf("CompileModule should succeed even with unresolved imports: %v", err)
	}
	defer compiled.Close(ctx)

	// Instantiation should fail because "missing_func" is not provided.
	_, err = rt.InstantiateModule(ctx, compiled)
	if err == nil {
		t.Fatal("expected instantiation error for module with unresolved imports, got nil")
	}
	t.Logf("Got expected instantiation error: %v", err)
}
