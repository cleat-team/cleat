package pluginharness

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestEverySDKImportIsAHostExport asserts that every host function an SDK
// DECLARES an import for is a name the engine actually exports.
//
// This exists because two SDKs declared one that is not. The engine exports
// cleat_schedule_invoke; the Rust SDK declared `pub fn schedule_invoke` inside
// its `#[link(wasm_import_module = "env")]` block with no #[link_name], and the
// Java SDK declared `@Import(module = "env", name = "schedule_invoke")`. Neither
// runtime defines that name.
//
// A wrong import name does not fail at the call. It fails INSTANTIATION, so it
// takes the whole module with it, and it is invisible until something
// references the binding -- guest toolchains drop imports nothing calls. Proven
// by execution 2026-09-05, by adding one arm to the Rust host-call fixture:
//
//	before: engine: host: ... wasm trap: host: instantiate: unknown import:
//	        `env::schedule_invoke` has not been defined
//	after:  status=ok detail=scheduled
//
// So any Rust or Java workflow that so much as referenced scheduleInvoke could
// not run, and every compile-time coverage number counted the binding as
// present. That is the same shape as the Java arity defect in #760 and as
// IMPROVEMENT-PLAN 3.55 -- a declaration that disagrees with the host, where
// the disagreement is fatal and silent.
//
// Anchored on DECLARATION SITES, per CLAUDE.md: the extern block, the @Import
// annotation, the @external decorator. Not on names appearing in source, which
// cannot tell a declaration from a comment about one.
func TestEverySDKImportIsAHostExport(t *testing.T) {
	root := findProjectRoot(t)
	exports := hostExportNames(t, root)
	if len(exports) < 40 {
		t.Fatalf("found only %d .Export( registrations in engine/imports.go; "+
			"expected at least 40. If registration moved behind a helper, this "+
			"test compares against a set it never found.", len(exports))
	}

	for _, sdk := range []struct {
		name  string
		floor int
		fn    func(*testing.T, string) map[string]string // import name -> where
	}{
		{"rust", 45, rustDeclaredImports},
		{"java", 45, javaDeclaredImports},
		{"assemblyscript", 45, asDeclaredImports},
	} {
		t.Run(sdk.name, func(t *testing.T) {
			declared := sdk.fn(t, root)
			// Input assertion. An extractor that silently finds nothing
			// reports perfect agreement, which is the failure this whole
			// file is about.
			if len(declared) < sdk.floor {
				t.Fatalf("%s: found only %d import declarations, expected at least %d.\n\n"+
					"The extractor anchors on the declaration site. If the SDK changed how "+
					"it spells one, this test quietly stops checking most of them.",
					sdk.name, len(declared), sdk.floor)
			}

			var bad []string
			for name, where := range declared {
				if !exports[name] {
					bad = append(bad, name+" ("+where+")")
				}
			}
			sort.Strings(bad)
			if len(bad) > 0 {
				t.Errorf("%s declares %d import(s) the engine does not export: %s\n\n"+
					"A guest that references one of these fails INSTANTIATION -- not the "+
					"call, the whole module -- and only once something references it, "+
					"because unused imports are dropped by the toolchain. Fix the name in "+
					"the SDK; do not add the export.",
					sdk.name, len(bad), strings.Join(bad, ", "))
			}
		})
	}
}

func hostExportNames(t *testing.T, root string) map[string]bool {
	t.Helper()
	src := readFileOrFatal(t, filepath.Join(root, "engine", "imports.go"))
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`\.Export\("([^"]+)"\)`).FindAllStringSubmatch(src, -1) {
		out[m[1]] = true
	}
	return out
}

// rustDeclaredImports reads the names inside the `extern "C"` block attributed
// with wasm_import_module = "env". A #[link_name] attribute overrides the Rust
// function name and IS the import name -- missing that is how this defect hid,
// since the two agree for every other declaration in the block.
func rustDeclaredImports(t *testing.T, root string) map[string]string {
	t.Helper()
	src := readFileOrFatal(t, filepath.Join(root, "crates", "cleat-sdk", "src", "host_calls.rs"))
	loc := regexp.MustCompile(`#\[link\(wasm_import_module = "env"\)\]\s*\n\s*extern "C" \{`).FindStringIndex(src)
	if loc == nil {
		t.Fatalf("no `#[link(wasm_import_module = \"env\")] extern \"C\"` block in host_calls.rs.\n\n" +
			"If the SDK changed how it declares imports, teach this extractor the new " +
			"spelling rather than letting it find nothing.")
	}
	depth, i := 1, loc[1]
	for depth > 0 && i < len(src) {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
		}
		i++
	}
	block := src[loc[1]:i]
	out := map[string]string{}
	re := regexp.MustCompile(`(?:#\[link_name = "([^"]+)"\]\s*\n\s*)?pub fn ([a-z_][a-z0-9_]*)\s*\(`)
	for _, m := range re.FindAllStringSubmatch(block, -1) {
		name := m[1]
		if name == "" {
			name = m[2]
		}
		out[name] = "crates/cleat-sdk/src/host_calls.rs"
	}
	return out
}

func javaDeclaredImports(t *testing.T, root string) map[string]string {
	t.Helper()
	p := filepath.Join(root, "crates", "cleat-java", "src", "main", "java", "cleat", "HostCalls.java")
	src := readFileOrFatal(t, p)
	out := map[string]string{}
	re := regexp.MustCompile(`@Import\(\s*module\s*=\s*"env"\s*,\s*name\s*=\s*"([^"]+)"\s*\)`)
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		out[m[1]] = "crates/cleat-java/.../HostCalls.java"
	}
	return out
}

func asDeclaredImports(t *testing.T, root string) map[string]string {
	t.Helper()
	p := filepath.Join(root, "packages", "cleat-as", "assembly", "host-calls.ts")
	src := readFileOrFatal(t, p)
	out := map[string]string{}
	re := regexp.MustCompile(`@external\("env",\s*"([^"]+)"\)`)
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		out[m[1]] = "packages/cleat-as/assembly/host-calls.ts"
	}
	return out
}

func readFileOrFatal(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}
