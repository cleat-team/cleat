package wasm

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A checked-in prebuilt WASM fixture exports every entry point its source
// declares. cleat#1660.
//
// The AssemblyScript prebuilt was five days behind its source and missing an
// entry point the source declares -- `bind_multiple_params`, added by #1067 on
// 2026-09-09 against a binary last built on 2026-09-04. Nothing noticed,
// because nothing compares the two. Both readers of that binary
// (import_section_test.go here, TestRealFixturesRouteToWasmtime in
// cmd/cleat-worker) look at its import section and its language, neither of
// which the missing export changes.
//
// The fixture states its own intent. Directly above the entry point that was
// never compiled in, assembly/index.ts says:
//
//	This exists to be COMPILED. Until it was added, every @cleatEntry in the
//	repository took a single string parameter, so the transform's
//	multi-parameter binding branch was emitted by nothing and checked by
//	nothing -- and it was broken.
//
// So the artefact contradicted the source's stated purpose for it while every
// check that read the artefact passed. Regenerating the binary fixes that
// once; this test is what makes the next drift fail loudly instead.
//
// NO TOOLCHAIN. It compares a parse of the committed binary against a parse of
// the committed source, so it needs neither `asc` nor a JDK and runs on every
// job rather than skipping where the toolchain is absent -- which matters,
// because the toolchain being absent is exactly the condition under which a
// stale binary goes unnoticed.
//
// NOTE ON THE GO TEST CACHE: these fixtures live outside this package
// directory, and `go test` only invalidates its cache on files opened INSIDE
// it. A local `go test ./wasm/` can therefore report `(cached)` against an
// edited fixture. CI passes -count=1 to every matrix package, so CI is sound;
// locally, watch for the word `(cached)` where a duration should be.
func TestAPrebuiltExportsWhatItsSourceDeclares(t *testing.T) {
	for _, fx := range prebuiltFixtures() {
		t.Run(fx.name, func(t *testing.T) {
			declared, err := fx.declaredEntries()
			if err != nil {
				t.Fatalf("UNMEASURED: reading declared entry points: %v", err)
			}
			// FLOOR. An empty declared set makes the loop below vacuous, so a
			// scanner that silently stopped matching -- a renamed decorator, a
			// moved source file -- would report success. "0 problems" and
			// "0 examined" must not look alike.
			if len(declared) == 0 {
				t.Fatalf("UNMEASURED: no entry points found in %s. The scanner "+
					"found nothing to check, which is not the same as finding "+
					"nothing wrong.", fx.sourceGlob)
			}

			exported, err := wasmFuncExports(fx.wasmPath)
			if err != nil {
				t.Fatalf("UNMEASURED: reading the export section of %s: %v", fx.wasmPath, err)
			}
			if len(exported) == 0 {
				t.Fatalf("UNMEASURED: %s exports no functions at all", fx.wasmPath)
			}

			have := make(map[string]bool, len(exported))
			for _, e := range exported {
				have[e] = true
			}

			var missing []string
			for _, d := range declared {
				if !have[d.exportName] {
					missing = append(missing, fmt.Sprintf("%s (declared at %s)", d.exportName, d.where))
				}
			}
			if len(missing) > 0 {
				sort.Strings(missing)
				t.Errorf("%s is missing %d of %d entry points its source declares:\n  %s\n\n"+
					"exports present: %s\n\n"+
					"The binary is stale. Regenerate it -- see %s -- and commit the result. "+
					"Nothing rebuilds these automatically. cleat#1660.",
					fx.wasmPath, len(missing), len(declared), strings.Join(missing, "\n  "),
					strings.Join(exported, ", "), filepath.Join(filepath.Dir(fx.wasmPath), "README.md"))
			}
		})
	}
}

// TestTheEntryPointScannerIgnoresProseAboutEntryPoints is the known-positive
// for the scanner above, and it is not optional.
//
// assembly/index.ts mentions "@cleatEntry" twice in COMMENTS, including in the
// comment block introducing the very entry point that went missing. A scan
// that cannot tell a declaration from a sentence about one over-reports, sends
// the reader chasing an export that was never meant to exist, and -- worse --
// makes the floor assertion above satisfiable by prose alone. The Java half
// has the same hazard in its javadoc.
//
// Anchoring is the fix, and it is the portable one: a decorator or annotation
// that generates an export sits at the start of a declaration line, and a
// retraction or an explanation never can. This test asserts that property
// against sources built to break it rather than against the real fixtures,
// which happen to pass today.
func TestTheEntryPointScannerIgnoresProseAboutEntryPoints(t *testing.T) {
	t.Run("assemblyscript", func(t *testing.T) {
		const src = `
// Uses @cleatEntry decorator. The transformer generates the wrapper.
//
// @cleatEntry("Retracted")
// export function removed_entry(h: HostCalls): string { }
const doc: string = 'call @cleatEntry("InAString")';

@cleatEntry("IgnoredByTheTransform")
export function real_entry(h: HostCalls, note: string): string {
  return note;
}

@cleatEntry()
export function bare_decorator(h: HostCalls): string { return ""; }
`
		got := names(scanAssemblyScriptEntries(src, "synthetic.ts"))
		want := []string{"bare_decorator", "real_entry"}
		assertSet(t, got, want,
			"the AssemblyScript scanner must read declarations and not prose")
	})

	t.Run("java", func(t *testing.T) {
		const src = `
package com.cleat.example;
/**
 * Historical note: @CleatEntry(name = "Retracted") used to be declared here.
 */
public class W {
    @CleatEntry(name = "ExplicitName")
    public static String namedEntry(HostCalls h, String in) { return in; }

    // @CleatEntry(name = "CommentedOut")
    // public static String goneEntry(HostCalls h) { return ""; }

    @CleatEntry
    public static String defaultedEntry(HostCalls h) { return ""; }
}
`
		got := names(scanJavaEntries(src, "synthetic.java"))
		// "ExplicitName" comes from the annotation; "defaultedEntry" from the
		// method, because name() defaults to the empty string and the
		// processor falls back to the method name.
		want := []string{"ExplicitName", "defaultedEntry"}
		assertSet(t, got, want,
			"the Java scanner must read annotations and not javadoc")
	})
}

// TestTheTwoSDKsNameTheirExportsByOppositeRules pins the fact the comparison
// above depends on, because getting it wrong is silent in both directions.
//
// The declarations look alike and mean opposite things:
//
//	AssemblyScript  @cleatEntry("CallAllPlugins")            export is call_all_plugins
//	                export function call_all_plugins(...)    the string is INERT
//
//	Java            @CleatEntry(name = "CallAllPlugins")     export is CallAllPlugins
//	                public static String callAllPlugins(...) the METHOD name is unused
//
// In AssemblyScript the transform never reads the decorator's arguments at all
// -- it only tests that the decorator IS cleatEntry
// (packages/cleat-as/transform/index.js) and emits
// `export function ${funcName}`. Measured 2026-09-16: neither "CallAllPlugins"
// nor "BindMultipleParams" appears anywhere in the committed AssemblyScript
// binary, which has no custom sections at all.
//
// In Java the annotation's name() IS the export, falling back to the method
// name when empty (crates/cleat-java/src/main/java/cleat/CleatEntryProcessor.java,
// `exportName = annotation.name(); if (exportName.isEmpty()) exportName =
// method.getSimpleName()`).
//
// A checker that assumed either rule for both languages would pass on one
// fixture and be wrong about the other -- and wrong PERMISSIVELY, because a
// name it derives incorrectly is a name it then fails to find a mismatch for.
func TestTheTwoSDKsNameTheirExportsByOppositeRules(t *testing.T) {
	as := scanAssemblyScriptEntries(
		"@cleatEntry(\"DecoratorArgument\")\nexport function function_name(h: HostCalls): string {}\n",
		"x.ts")
	if len(as) != 1 || as[0].exportName != "function_name" {
		t.Errorf("AssemblyScript: export name must come from the FUNCTION, got %v", names(as))
	}

	java := scanJavaEntries(
		"@CleatEntry(name = \"AnnotationArgument\")\npublic static String methodName(HostCalls h) {}\n",
		"X.java")
	if len(java) != 1 || java[0].exportName != "AnnotationArgument" {
		t.Errorf("Java: export name must come from the ANNOTATION, got %v", names(java))
	}
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

type entryDecl struct {
	exportName string
	where      string
}

type prebuiltFixture struct {
	name       string
	wasmPath   string
	sourceGlob string
	scan       func(src, path string) []entryDecl
}

// prebuiltFixtures enumerates every checked-in prebuilt WASM fixture.
//
// Listed explicitly rather than discovered by globbing for prebuilt/*.wasm: a
// glob returns the empty set when the layout moves, and an empty set of
// fixtures is a test that passes having checked nothing. A new prebuilt that
// nobody adds here is a visible omission; a glob that silently stops matching
// is not.
func prebuiltFixtures() []prebuiltFixture {
	const td = "../tests/plugin-harness/testdata"
	return []prebuiltFixture{
		{
			name:       "assemblyscript",
			wasmPath:   td + "/asworkflow/prebuilt/workflow.wasm",
			sourceGlob: td + "/asworkflow/assembly/*.ts",
			scan:       scanAssemblyScriptEntries,
		},
		{
			name:       "java-teavm",
			wasmPath:   td + "/javaworkflow/prebuilt/workflow.wasm",
			sourceGlob: td + "/javaworkflow/src/main/java/com/cleat/example/*.java",
			scan:       scanJavaEntries,
		},
	}
}

func (f prebuiltFixture) declaredEntries() ([]entryDecl, error) {
	paths, err := filepath.Glob(f.sourceGlob)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no source files matched %s", f.sourceGlob)
	}
	var out []entryDecl
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		out = append(out, f.scan(string(b), p)...)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Source scanners
//
// Both anchor on the START of a line, so a decorator or annotation quoted in a
// comment, in a string, or in a javadoc block cannot be read as a declaration.
// See TestTheEntryPointScannerIgnoresProseAboutEntryPoints.
// ---------------------------------------------------------------------------

var (
	// @cleatEntry(...) on its own line, then `export function <name>`. The
	// decorator's argument is deliberately not captured: the transform does
	// not read it either.
	asEntryRe = regexp.MustCompile(
		`(?m)^[ \t]*@cleatEntry\b[^\n]*\n(?:[ \t]*\n)*[ \t]*export[ \t]+function[ \t]+([A-Za-z_$][\w$]*)`)

	// @CleatEntry, optionally with name = "...", then a `public static`
	// method. The return type is matched loosely because it is irrelevant
	// here and varies (String, long, void).
	javaEntryRe = regexp.MustCompile(
		`(?m)^[ \t]*@CleatEntry\b[ \t]*(?:\([ \t]*name[ \t]*=[ \t]*"([^"]*)"[ \t]*\))?[ \t]*\n` +
			`(?:[ \t]*\n)*[ \t]*public[ \t]+static[ \t]+[\w.<>\[\]]+[ \t]+([A-Za-z_$][\w$]*)[ \t]*\(`)
)

func scanAssemblyScriptEntries(src, path string) []entryDecl {
	var out []entryDecl
	for _, m := range asEntryRe.FindAllStringSubmatchIndex(src, -1) {
		name := src[m[2]:m[3]]
		out = append(out, entryDecl{exportName: name, where: where(src, path, m[0])})
	}
	return out
}

func scanJavaEntries(src, path string) []entryDecl {
	var out []entryDecl
	for _, m := range javaEntryRe.FindAllStringSubmatchIndex(src, -1) {
		name := ""
		if m[2] >= 0 {
			name = src[m[2]:m[3]]
		}
		if name == "" { // name() defaults to the method name
			name = src[m[4]:m[5]]
		}
		out = append(out, entryDecl{exportName: name, where: where(src, path, m[0])})
	}
	return out
}

func where(src, path string, off int) string {
	return fmt.Sprintf("%s:%d", path, strings.Count(src[:off], "\n")+1)
}

func names(ds []entryDecl) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.exportName)
	}
	sort.Strings(out)
	return out
}

func assertSet(t *testing.T, got, want []string, msg string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s\n  got:  %v\n  want: %v", msg, got, want)
	}
}

// ---------------------------------------------------------------------------
// The export section
// ---------------------------------------------------------------------------

// wasmFuncExports returns the names of every FUNCTION export, in module order.
//
// Kind 0 is a function; memory, table and global exports are skipped because
// an entry point is always a function and including them would let an
// unrelated global satisfy a missing entry point by name.
func wasmFuncExports(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) < 8 || string(b[:4]) != "\x00asm" {
		return nil, fmt.Errorf("not a WASM module")
	}
	var out []string
	off := 8
	for off < len(b) {
		id := b[off]
		off++
		size, n, err := readULEB128At(b, off)
		if err != nil {
			return nil, fmt.Errorf("section %d size: %w", id, err)
		}
		off += n
		end := off + int(size)
		if end > len(b) || end < off {
			return nil, fmt.Errorf("section %d overruns the module", id)
		}
		if id == 7 { // export section
			p := off
			count, n, err := readULEB128At(b, p)
			if err != nil {
				return nil, fmt.Errorf("export count: %w", err)
			}
			p += n
			for i := uint32(0); i < count; i++ {
				nameLen, n, err := readULEB128At(b, p)
				if err != nil {
					return nil, fmt.Errorf("export %d name length: %w", i, err)
				}
				p += n
				if p+int(nameLen) > end {
					return nil, fmt.Errorf("export %d name overruns the section", i)
				}
				name := string(b[p : p+int(nameLen)])
				p += int(nameLen)
				if p >= end {
					return nil, fmt.Errorf("export %d has no kind byte", i)
				}
				kind := b[p]
				p++
				_, n, err = readULEB128At(b, p) // the index, unused
				if err != nil {
					return nil, fmt.Errorf("export %d index: %w", i, err)
				}
				p += n
				if kind == 0 {
					out = append(out, name)
				}
			}
		}
		off = end
	}
	return out, nil
}

func readULEB128At(b []byte, off int) (uint32, int, error) {
	var result uint32
	var shift uint
	for n := 0; n < 5; n++ {
		if off+n >= len(b) {
			return 0, 0, fmt.Errorf("truncated LEB128")
		}
		v := b[off+n]
		result |= uint32(v&0x7f) << shift
		if v&0x80 == 0 {
			return result, n + 1, nil
		}
		shift += 7
	}
	return 0, 0, fmt.Errorf("LEB128 too long")
}
