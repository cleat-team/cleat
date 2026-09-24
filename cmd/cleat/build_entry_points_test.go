package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// These run against the REAL checked-in examples, not synthetic fixtures --
// each one already exercises the exact case that would defeat a naive
// extractor: both AS and Java give their entry an explicit display name that
// differs from the actual identifier the export must be named after (must
// NOT be picked up in AS's case, MUST be picked up in Java's -- the two SDKs
// disagree on which one wins, per each one's own codegen, and a test that
// used only one of them could not catch the extractor defaulting to the
// wrong rule).
//
// Rust has no extractor test here since cleat#2113: its entry points come
// from the cleat_entry_points WASM section the #[cleat_entry] macro itself
// emits, read via wasm.ReadEntryPointsSection and proven against the real
// example by TestRustExampleEntryPointResolutionLive
// (rust_entry_point_resolution_live_test.go), not by source scanning.

func TestJavaEntryPointNamesReadsTheRealExample(t *testing.T) {
	got := javaEntryPointNames("../../examples/java-workflow")
	// The annotation's name= attribute wins over the method's own name --
	// placeOrder/cancelOrder in source, place_order/cancel_order expected.
	want := []string{"place_order", "cancel_order"}
	assertSameNames(t, got, want)
}

func TestASEntryPointNamesReadsTheRealExample(t *testing.T) {
	got := asEntryPointNames("../../examples/as-workflow")
	// The decorator's string argument ("PlaceOrder") is a display name and
	// must NOT appear here -- the function identifier is the export name.
	want := []string{
		"place_order", "cancel_order", "defer_order", "defer_suspend",
		"spin_forever", "trap_after_defer", "defer_registers_defer",
		"defer_continues_as_new",
	}
	assertSameNames(t, got, want)
}

// TestJavaEntryPointNamesFallsBackToMethodNameWithoutAnExplicitOne is a
// synthetic case the real example doesn't cover: every entry in it gives an
// explicit name=, so a version of the extractor that only ever reads the
// annotation (and never falls back) would still pass the example test above.
func TestJavaEntryPointNamesFallsBackToMethodNameWithoutAnExplicitOne(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "Workflow.java", `
package com.example;
import cleat.CleatEntry;
class Workflow {
    @CleatEntry
    public static String runIt(HostCalls h, String input) { return input; }
}
`)
	got := javaEntryPointNames(dir)
	assertSameNames(t, got, []string{"runIt"})
}

// Same risk, Java's side: a block comment is the one case where nothing but
// the delimiters separates the annotation from a method, so it is the one
// case that actually exercises stripCLikeCommentsKeepStrings rather than the
// regex's own adjacency requirement.
func TestJavaEntryPointNamesIgnoresAMethodCommentedOutInABlockComment(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "Workflow.java", `
package com.example;
import cleat.CleatEntry;
class Workflow {
    /*
    @CleatEntry(name = "commented_out")
    public static String commentedOut(HostCalls h, String input) { return input; }
    */

    @CleatEntry(name = "real_one")
    public static String realOne(HostCalls h, String input) { return input; }
}
`)
	got := javaEntryPointNames(dir)
	assertSameNames(t, got, []string{"real_one"})
}

// AssemblyScript's side of the same risk.
func TestASEntryPointNamesIgnoresAFunctionCommentedOutInABlockComment(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, filepath.Join("assembly", "index.ts"), `
/*
@cleatEntry("CommentedOut")
export function commented_out(input: string): string { return input; }
*/

@cleatEntry("RealOne")
export function real_one(input: string): string { return input; }
`)
	got := asEntryPointNames(dir)
	assertSameNames(t, got, []string{"real_one"})
}

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

func assertSameNames(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func writeTestFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("creating test fixture dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writing test fixture: %v", err)
	}
}
