package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// These run against the REAL checked-in examples, not synthetic fixtures --
// each one already exercises the exact case that would defeat a naive
// extractor: Rust's source mentions `#[cleat_entry]` inside doc comments
// describing the macro itself (must not be picked up), and both AS and Java
// give their entry an explicit display name that differs from the actual
// identifier the export must be named after (must NOT be picked up in
// AS's case, MUST be picked up in Java's -- the two SDKs disagree on which
// one wins, per each one's own codegen, and a test that used only one of
// them could not catch the extractor defaulting to the wrong rule).

func TestRustEntryPointNamesReadsTheRealExample(t *testing.T) {
	got := rustEntryPointNames("../../examples/rust-workflow/src")
	want := []string{
		"place_order", "cancel_order", "defer_order", "suspend_probe",
		"sleep_probe", "sleep_discard_probe", "retry_probe", "retry_long_probe",
	}
	assertSameNames(t, got, want)
}

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

// The risk this guards against isn't a `//` line comment mentioning the
// attribute (each commented line's own leading `//` already breaks the
// regex's required adjacency between #[cleat_entry] and `fn`, with or
// without stripping) -- it's a function commented out inside a `/* */`
// block, where nothing but the block delimiters separates them, and the
// delimiters are NOT part of what the regex matches against once stripped.
func TestRustEntryPointNamesIgnoresAFunctionCommentedOutInABlockComment(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "lib.rs", `
/*
#[cleat_entry]
fn commented_out(h: &HostCalls, input: Input) -> Result<String, String> { Ok(input) }
*/

#[cleat_entry]
fn real_one(h: &HostCalls, input: Input) -> Result<String, String> { Ok(input) }
`)
	got := rustEntryPointNames(dir)
	assertSameNames(t, got, []string{"real_one"})
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
