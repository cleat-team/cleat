package wasm

import (
	"strings"
	"testing"
)

// makeWasmWithImports builds a minimal WASM binary with the given import entries.
// Each entry is a (module, name) pair imported as a function kind.
func makeWasmWithImports(imports []struct{ module, name string }) []byte {
	// Build import section payload
	content := encodeULEB128(uint32(len(imports))) // count
	for _, imp := range imports {
		// Module name
		content = append(content, encodeULEB128(uint32(len(imp.module)))...)
		content = append(content, []byte(imp.module)...)
		// Field name
		content = append(content, encodeULEB128(uint32(len(imp.name)))...)
		content = append(content, []byte(imp.name)...)
		// Kind: func
		content = append(content, 0x00)
		// Type index (always 0 for our test modules)
		content = append(content, encodeULEB128(0)...)
	}
	// Section: id=2, size, content
	size := encodeULEB128(uint32(len(content)))
	out := []byte{2}
	out = append(out, size...)
	out = append(out, content...)
	return memTestWasm(out)
}

func TestRewriteWitImports_EmptyBinary(t *testing.T) {
	_, err := RewriteWitImports([]byte{})
	if err == nil {
		t.Fatal("expected error for empty binary")
	}
}

func TestRewriteWitImports_BadMagic(t *testing.T) {
	_, err := RewriteWitImports([]byte{0xde, 0xad, 0xbe, 0xef, 0x01, 0x00, 0x00, 0x00})
	if err == nil {
		t.Fatal("expected error for bad magic")
	}
}

func TestRewriteWitImports_NoImportSection(t *testing.T) {
	// Valid header with no sections at all.
	wasm := memTestWasm()
	result, err := RewriteWitImports(wasm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Error("expected nil result (no WIT imports to rewrite)")
	}
}

func TestRewriteWitImports_WITStyleImports(t *testing.T) {
	imports := []struct{ module, name string }{
		{"cleat:host-calls/durable-call", "durable-call"},
		{"cleat:host-calls/durable-sleep", "durable-sleep"},
		{"cleat:host-calls/durable-version", "durable-version"},
	}
	wasm := makeWasmWithImports(imports)

	result, err := RewriteWitImports(wasm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result (WIT imports should be rewritten)")
	}

	// Verify imports were rewritten to "env" module.
	importNames, err := readImportModuleNames(result)
	if err != nil {
		t.Fatalf("readImportModuleNames failed: %v", err)
	}
	for i, mod := range importNames {
		if mod != "env" {
			t.Errorf("import %d: expected module 'env', got %q", i, mod)
		}
	}

	// Verify function names were rewritten to flat names by checking the binary content.
	resultStr := string(result)
	for _, fn := range []string{"cleat_call", "cleat_sleep", "cleat_version"} {
		if !strings.Contains(resultStr, fn) {
			t.Errorf("expected field name %q in rewritten binary", fn)
		}
	}
}

func TestRewriteWitImports_MixedImports(t *testing.T) {
	imports := []struct{ module, name string }{
		{"env", "memory"},
		{"cleat:host-calls/durable-lifecycle", "durable-defer"},
	}
	wasm := makeWasmWithImports(imports)

	result, err := RewriteWitImports(wasm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	// Verify the rewritten binary contains expected strings.
	resultStr := string(result)
	if !strings.Contains(resultStr, "cleat_defer") {
		t.Error("expected 'cleat_defer' field after rewrite")
	}
	if !strings.Contains(resultStr, "env") {
		t.Error("expected 'env' module after rewrite")
	}
	// The non-WIT "memory" import should now be under "env" too if it wasn't already,
	// but since our helper creates func kind not memory kind, all entries are treated
	// as func imports. Let's verify both entries are present.
	if !strings.Contains(resultStr, "memory") {
		t.Error("expected 'memory' field to be preserved")
	}
}

func TestRewriteWitImports_Idempotent(t *testing.T) {
	imports := []struct{ module, name string }{
		{"cleat:host-calls/durable-call", "durable-call"},
		{"cleat:host-calls/durable-sleep", "durable-sleep"},
	}
	wasm := makeWasmWithImports(imports)

	result1, err := RewriteWitImports(wasm)
	if err != nil {
		t.Fatalf("first rewrite failed: %v", err)
	}
	if result1 == nil {
		t.Fatal("expected non-nil result from first rewrite")
	}

	// Rewrite again.
	result2, err := RewriteWitImports(result1)
	if err != nil {
		t.Fatalf("second rewrite failed: %v", err)
	}

	// Second rewrite should return nil (no WIT imports left to rewrite)
	// since all imports are now under "env".
	if result2 != nil {
		t.Error("expected nil result from second rewrite (already idempotent)")
	}
}
