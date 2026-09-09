package wasm

import (
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/internal/analyzer"
)

// TestEveryAdapterDefCompiles emits an adapter containing EVERY adapterDefs
// entry and compiles it for wasip1.
//
// Nothing did this. The generator's own tests assert on the emitted TEXT --
// `strings.Contains(code, "func parseChildResultArray")` and friends -- and
// the emitted file is only ever really checked by building a guest, which
// happens in integration jobs that build one fixture using whichever calls
// that fixture happens to make. So a def can be emitted, wrong, and invisible
// until someone writes a workflow that uses it.
//
// Two failures on 2026-09-05, hours apart, both in generated code that no unit
// test compiled:
//
//   - #780: a comment mentioning strings.Index made patchAdapterImports inject
//     an unused import, and every guest build failed with
//     `"strings" imported and not used`.
//   - #786: a wrapper row invented a closure field carrying a parameter the
//     inner import's body never reads, and eleven CI jobs failed with
//     `declared and not used: heartbeatIntervalMs`.
//
// Both are one-line compile errors that a unit test can catch in seconds, and
// both cost a full CI round instead. #786's own unit test passed throughout:
// it asserted that each wrapper ends up with the right IMPORT, which was true
// -- the FIELD was the problem. A guard measuring the legible half again.
//
// Covering every def rather than a fixture's subset is the point: the failure
// mode is one field in isolation, so a fixture that does not use that field
// cannot see it.
func TestEveryAdapterDefCompiles(t *testing.T) {
	// No -short skip. This repo removed those on purpose: "-short is a mode
	// someone opts into, not a precondition, and a harness that measures what
	// runs must not learn to stop running"
	// (tests/plugin-harness/hostcall_go_test.go). It costs about half a second.
	//
	// No toolchain skip either, for the same reason the harness gives: `go
	// test` is what is executing this, so `go` is on PATH by construction and a
	// skip here would be a decision not to run dressed as an environmental
	// fact.
	if _, err := exec.LookPath("go"); err != nil {
		t.Fatalf("no go toolchain on PATH, yet `go test` is executing this: %v", err)
	}

	repoRoot, err := FindRepoRoot(mustCwd(t))
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}

	// A real analysis of a real (call-free) workflow: GenerateExports builds
	// its dispatch from actual entry points, and an exports file with none
	// declares a HostCalls value nothing uses -- a compile error that would be
	// the harness's fault rather than the generator's.
	srcDir := filepath.Join(repoRoot, "wasm", "testdata", "emptyworkflow")
	analysis, err := analyzer.LoadPackages(srcDir, token.NewFileSet())
	if err != nil {
		t.Fatalf("analysing %s: %v", srcDir, err)
	}

	usage := usageCoveringEveryAdapterDef(t)
	outDir := t.TempDir()

	cfg := &BuildConfig{
		SrcDir:      srcDir,
		OutDir:      outDir,
		PkgName:     "main",
		ModulePath:  RootModulePath,
		ProjectRoot: repoRoot,
		GoVersion:   "1.25",
		Outputs:     BuildOutputs("main", usage, analysis, "go"),
		WASMOutput:  "probe.wasm",
		Target:      "go",
	}
	if err := PrepareBuildDir(cfg); err != nil {
		t.Fatalf("PrepareBuildDir: %v", err)
	}

	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = outDir
	tidy.Env = append(os.Environ(), "GOOS="+runtime.GOOS, "GOARCH="+runtime.GOARCH)
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy in the generated build dir: %v\n%s", err, out)
	}

	build := exec.Command("go", "build", "./...")
	build.Dir = outDir
	build.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("the generated adapter does not compile.\n%s\n\n"+
			"Every adapterDefs entry is emitted here, so this is a defect in one "+
			"of them -- most likely a closure parameter the body never reads, or a package referenced "+
			"without an import. The error above names the file and line in %s.", out, outDir)
	}
}

// usageCoveringEveryAdapterDef builds a UsageInfo naming every field the
// generator can emit, paired with the import that implements it.
func usageCoveringEveryAdapterDef(t *testing.T) *UsageInfo {
	t.Helper()

	// The import each field is implemented by, taken from the real table.
	fieldImport := map[string]string{}
	for _, hf := range hostFunctions {
		if _, seen := fieldImport[hf.FieldName]; !seen {
			fieldImport[hf.FieldName] = hf.ImportName
		}
	}

	u := &UsageInfo{Used: map[string]bool{}, Children: map[string]bool{}}
	var missing []string
	seenImport := map[string]bool{}

	add := func(field string) {
		if imp, ok := fieldImport[field]; ok {
			u.Used[imp] = true
			if !seenImport[field] {
				seenImport[field] = true
				u.Funcs = append(u.Funcs, HostFunction{ImportName: imp, FieldName: field})
			}
			return
		}
		// Reachable through compositeRequires instead: the wrapper's imports are
		// wired and no field is emitted for it, so there is nothing for this test
		// to compile -- but it is not unreachable.
		if imps, ok := compositeRequires[field]; ok {
			for _, imp := range imps {
				u.Used[imp] = true
			}
			return
		}
		missing = append(missing, field)
	}

	for field := range adapterDefs {
		add(field)
	}

	// A def named by NEITHER table is unreachable: nothing can request it, so no
	// build can emit it and this test cannot compile it either.
	//
	// Both tables have to be consulted, and the distinction is #786's:
	// hostFunctions is bidirectional -- a row requests an import AND emits a
	// field named FieldName implemented by that import's body -- while
	// compositeRequires only requests imports, for SDK-level wrappers that must
	// NOT get a field of their own. Checking hostFunctions alone reported
	// DurableCallTypedWithOptions as unreachable when it is reachable through
	// the second table. IMPROVEMENT-PLAN 3.225.
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("adapter definitions named by neither hostFunctions nor compositeRequires, "+
			"so no build can emit them: %s\n"+
			"Either name it in one of those tables (if a guest is supposed to be able to call it) "+
			"or delete the definition (if it is dead). A public method in this state compiles to "+
			"nothing and the build says OK -- see IMPROVEMENT-PLAN 3.224.",
			strings.Join(missing, ", "))
	}

	sort.Slice(u.Funcs, func(i, j int) bool { return u.Funcs[i].ImportName < u.Funcs[j].ImportName })
	return u
}

func mustCwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return wd
}
