package main

import (
	"reflect"
	"strings"
	"testing"
)

// Java and AssemblyScript no longer predict entry points by scanning
// source: cleat#2145 moved both onto the same model cleat#2113 gave Rust --
// each SDK's own codegen states the list in the artifact it produces (a
// sidecar manifest for AS/Java, a linker-merged WASM section for Rust), and
// cleat build reads THAT, rather than reconstructing it with a regex. See
// build_as.go, build_java.go, build_rust.go and wasm.WriteEntryPointsSection
// / wasm.ReadEntryPointsSection. The live proof that each SDK's manifest
// actually survives compilation and names real exports is
// rust_entry_point_resolution_live_test.go,
// as_entry_point_resolution_live_test.go and
// java_entry_point_resolution_live_test.go -- not a source-scanning test
// here, because there is no longer any source-level prediction to test.

// syntheticWasmModule is a hand-built (not compiler-produced) minimal WASM
// module: two function exports, "foo" and "bar", plus a non-function export
// "mem" (kind 2) with no backing memory section -- wasmFuncExportNames only
// reads the export table's structure, so this is sufficient to test it
// without a real toolchain, and it deliberately includes a non-func export
// to prove kind filtering actually filters.
var syntheticWasmModule = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	0x01, 0x04, 0x01, 0x60, 0x00, 0x00,
	0x03, 0x03, 0x02, 0x00, 0x00,
	0x07, 0x13, 0x03,
	0x03, 0x66, 0x6f, 0x6f, 0x00, 0x00, // "foo", kind=func, idx=0
	0x03, 0x62, 0x61, 0x72, 0x00, 0x01, // "bar", kind=func, idx=1
	0x03, 0x6d, 0x65, 0x6d, 0x02, 0x00, // "mem", kind=memory, idx=0
	0x0a, 0x07, 0x02, 0x02, 0x00, 0x0b, 0x02, 0x00, 0x0b,
}

func TestWasmFuncExportNamesParsesExportSection(t *testing.T) {
	got := wasmFuncExportNames(syntheticWasmModule)
	want := map[string]bool{"foo": true, "bar": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v (the memory export \"mem\" must be excluded -- kind 2, not 0)", got, want)
	}
}

func TestWasmFuncExportNamesOnTooShortInput(t *testing.T) {
	got := wasmFuncExportNames([]byte{0x00, 0x61})
	if len(got) != 0 {
		t.Fatalf("got %v, want empty for a truncated header", got)
	}
}

func TestVerifyEntryPointsAreExportsAcceptsRealExports(t *testing.T) {
	if err := verifyEntryPointsAreExports("test", syntheticWasmModule, []string{"foo", "bar"}); err != nil {
		t.Fatalf("both names are real exports, want nil, got: %v", err)
	}
}

func TestVerifyEntryPointsAreExportsAcceptsEmptyEntryPoints(t *testing.T) {
	// Deliberately garbage bytes: the empty-list fast path must not even look.
	if err := verifyEntryPointsAreExports("test", []byte("not wasm at all"), nil); err != nil {
		t.Fatalf("no predicted entry points, want nil regardless of wasm bytes, got: %v", err)
	}
}

// This is the case the whole check exists for: a source-level extractor
// predicted a name ("baz") the compiler never exported. Falsified by
// commenting out the `if !exports[name]` check locally and confirming this
// goes red with no error at all before restoring it.
func TestVerifyEntryPointsAreExportsRejectsAPredictedNameNotExported(t *testing.T) {
	err := verifyEntryPointsAreExports("rust", syntheticWasmModule, []string{"foo", "baz"})
	if err == nil {
		t.Fatal("want an error naming \"baz\", got nil")
	}
	if !strings.Contains(err.Error(), "baz") {
		t.Errorf("error does not name the missing export \"baz\": %v", err)
	}
	if strings.Contains(err.Error(), "\"foo\"") {
		t.Errorf("error should not blame \"foo\", which IS a real export: %v", err)
	}
}
