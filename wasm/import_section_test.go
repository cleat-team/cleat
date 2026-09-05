package wasm

import (
	"os"
	"testing"
)

// TestReadImportSection_ParsesEveryImport is the regression test for a parser
// that stopped after the first import.
//
// readImportSection advanced past the import descriptor's *kind byte* and left
// its payload unread, so the next iteration read a type index as the following
// import's module-name length. Any module with two or more imports failed with
// "corrupt WASM import N".
//
// It was invisible because both callers read an error as "cannot tell":
// NeededEnvImports falls back to registering every host function, and
// detectLanguageFromImports returns "", which DetectLanguage turns into its
// "go" default. Nothing was wrong-looking; the AssemblyScript and TeaVM
// branches simply never ran.
//
// These fixtures are real toolchain output rather than hand-built modules, so
// the test fails if the descriptor encoding is mishandled for shapes a
// synthetic module would not produce.
func TestReadImportSection_ParsesEveryImport(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		wantCount int
		wantFirst wasmImport
		wantHas   wasmImport
	}{
		{
			name:      "assemblyscript",
			path:      "../tests/plugin-harness/testdata/asworkflow/prebuilt/workflow.wasm",
			wantCount: 3,
			wantFirst: wasmImport{module: "env", field: "abort"},
			wantHas:   wasmImport{module: "env", field: "plugin_call_streaming"},
		},
		{
			name:      "java-teavm",
			path:      "../tests/plugin-harness/testdata/javaworkflow/prebuilt/workflow.wasm",
			wantCount: 7,
			wantFirst: wasmImport{module: "teavm", field: "putwcharsOut"},
			wantHas:   wasmImport{module: "env", field: "plugin_call"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := os.ReadFile(tc.path)
			if err != nil {
				t.Fatalf("fixture %s is missing; it is checked in and this test "+
					"is meaningless without it: %v", tc.path, err)
			}

			imports, err := readImportSection(b)
			if err != nil {
				t.Fatalf("readImportSection: %v", err)
			}
			if len(imports) != tc.wantCount {
				t.Errorf("parsed %d imports, want %d: %v", len(imports), tc.wantCount, imports)
			}
			if len(imports) > 0 && imports[0] != tc.wantFirst {
				t.Errorf("first import = %v, want %v", imports[0], tc.wantFirst)
			}
			found := false
			for _, imp := range imports {
				if imp == tc.wantHas {
					found = true
				}
			}
			if !found {
				// A later import specifically: reaching it is what the old
				// parser could not do.
				t.Errorf("import %v not found; parsing stopped early: %v", tc.wantHas, imports)
			}
		})
	}
}

// TestDetectLanguage_IdentifiesNonGoGuests asserts the consequence rather than
// the mechanism. The AssemblyScript and TeaVM branches in
// detectLanguageFromImports have existed for a long time and returned nothing,
// because the parser they depend on failed before reaching them, and
// DetectLanguage's "go" default made that look like a positive answer.
func TestDetectLanguage_IdentifiesNonGoGuests(t *testing.T) {
	// Only fixtures that are actually tracked in git. examples/rust-workflow
	// builds under **/target/, which .gitignore excludes, so reading it here
	// passes on a developer machine that has run cargo and fails in CI. Rust
	// detection is covered synthetically below instead -- it keys off a
	// "/rustc/" substring rather than the import section, so it needs no
	// toolchain and no artifact.
	cases := []struct {
		name, path, want string
	}{
		{"assemblyscript", "../tests/plugin-harness/testdata/asworkflow/prebuilt/workflow.wasm", "assemblyscript"},
		{"java-teavm", "../tests/plugin-harness/testdata/javaworkflow/prebuilt/workflow.wasm", "java"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := os.ReadFile(tc.path)
			if err != nil {
				t.Fatalf("fixture %s is missing: %v", tc.path, err)
			}
			if got := DetectLanguage(b); got != tc.want {
				t.Errorf("DetectLanguage = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("rust", func(t *testing.T) {
		// A Rust cdylib embeds standard-library paths; detectLanguageFromImports
		// looks for "/rustc/" anywhere in the binary, before any import parsing.
		mod := append([]byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00},
			[]byte("/rustc/9fc2a81ee1a4c0a2f0d5b3c1e/library/std/src/lib.rs")...)
		if got := DetectLanguage(mod); got != "rust" {
			t.Errorf("DetectLanguage = %q, want %q", got, "rust")
		}
	})
}

// TestNeededEnvImports_NoLongerAlwaysNil covers the other caller. Its nil
// return means "register every host function", a safe fallback that was
// silently taken on every module with more than one import -- so the
// optimisation it exists for had never applied.
func TestNeededEnvImports_NoLongerAlwaysNil(t *testing.T) {
	b, err := os.ReadFile("../tests/plugin-harness/testdata/asworkflow/prebuilt/workflow.wasm")
	if err != nil {
		t.Fatalf("fixture missing: %v", err)
	}
	needed := NeededEnvImports(b)
	if needed == nil {
		t.Fatal("NeededEnvImports returned nil (the register-everything fallback), " +
			"meaning the import section still failed to parse")
	}
	for _, want := range []string{"abort", "plugin_call", "plugin_call_streaming"} {
		if !needed[want] {
			t.Errorf("env import %q missing from %v", want, needed)
		}
	}
}

// TestSkipImportDesc covers the three descriptor kinds the fixtures do not
// exercise. Only func imports occur in cleat's own guests, so table, memory
// and global are asserted directly rather than left to a future binary to
// discover -- guessing one of them wrong desynchronises the entire section
// rather than losing a single entry.
func TestSkipImportDesc(t *testing.T) {
	cases := []struct {
		name  string
		desc  []byte
		want  int
		fails bool
	}{
		{name: "func typeidx", desc: []byte{0x00, 0x07}, want: 2},
		{name: "func typeidx multibyte", desc: []byte{0x00, 0x80, 0x01}, want: 3},
		{name: "table reftype + limits min only", desc: []byte{0x01, 0x70, 0x00, 0x01}, want: 4},
		{name: "table reftype + limits min/max", desc: []byte{0x01, 0x70, 0x01, 0x01, 0x10}, want: 5},
		{name: "mem limits min only", desc: []byte{0x02, 0x00, 0x01}, want: 3},
		{name: "mem limits min/max", desc: []byte{0x02, 0x01, 0x01, 0x10}, want: 4},
		{name: "global valtype + mut", desc: []byte{0x03, 0x7f, 0x00}, want: 3},
		{name: "unknown kind", desc: []byte{0x09, 0x00}, fails: true},
		{name: "truncated func", desc: []byte{0x00}, fails: true},
		{name: "truncated limits", desc: []byte{0x02}, fails: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := skipImportDesc(tc.desc, 0, len(tc.desc))
			if tc.fails {
				if err == nil {
					t.Fatalf("expected error, got offset %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("offset = %d, want %d", got, tc.want)
			}
		})
	}
}
