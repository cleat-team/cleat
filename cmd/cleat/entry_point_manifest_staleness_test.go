// TestAssemblyScriptBuildRefusesAStaleManifest and
// TestJavaBuildRefusesAStaleManifest are the known-positive falsification for
// a risk cleat#2145's sidecar-manifest design has that Rust's original
// linker-merged cleat_entry_points section does not: unlike a WASM custom
// section, which exists only if THIS compile produced it,
// dist/cleat-entry-points.txt (AS) and cleat-entry-points.txt under build/
// (Java) are ordinary files on disk that can survive from a PRIOR build. If a
// build whose SDK-side manifest write fails or is silently skipped then falls
// through to a leftover file from an earlier compile, cleat build would embed
// entry points that do not match the artifact it just produced -- exactly the
// "wrong entry point, silently" failure cleat#2113 exists to kill, reopened
// through a path verifyEntryPointsAreExports cannot see: a stale manifest can
// easily still name real exports, just the wrong build's.
//
// build_as.go and build_java.go guard against this by deleting the manifest
// path(s) before invoking the compiler and requiring exactly one freshly
// written manifest afterward (Java: exactly one across the whole build/ tree,
// since Gradle's own output layout is a convention rather than a guarantee --
// see the WalkDir comment in build_java.go). Both tests below prove that
// guard actually fires, not just that it compiles: each copies a real example
// to a scratch tree, PATCHES ITS OWN COPY of the SDK-side writer (the AS
// transform / the Java annotation processor) to skip the write -- simulating
// exactly the "write silently returned false" failure the AS half of this
// issue hit for real during development, see index.js's
// _writeEntryPointManifest doc comment -- plants a stale manifest naming an
// entry point that must never surface, and asserts the build refuses rather
// than embedding it.
//
// The shared repo's own transform/processor sources are never touched: each
// test patches an independent copy (AS: node_modules/@cleat/transform
// re-materialized as real files rather than the symlink examples/as-workflow
// normally has, so patching it cannot reach packages/cleat-as/transform;
// Java: crates/cleat-java copied alongside examples/java-workflow at the same
// two-levels-up relative layout settings.gradle.kts's projectDir expects, so
// no path rewriting is needed). That makes both tests safe to run
// concurrently with anything else building the real examples.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAssemblyScriptBuildRefusesAStaleManifest(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	if _, err := exec.LookPath("npx"); err != nil {
		t.Skip("npx not available")
	}
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync not available")
	}

	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	srcDir := filepath.Join(repoRoot, "examples", "as-workflow")
	transformSrcDir := filepath.Join(repoRoot, "packages", "cleat-as", "transform")
	sdkSrcDir := filepath.Join(repoRoot, "packages", "cleat-as")
	if _, statErr := os.Stat(filepath.Join(srcDir, "package.json")); statErr != nil {
		t.Fatalf("computed AS example dir %s has no package.json: %v", srcDir, statErr)
	}

	asDir := filepath.Join(t.TempDir(), "as-workflow")
	if out, cpErr := exec.Command("rsync", "-a", srcDir+"/", asDir+"/").CombinedOutput(); cpErr != nil {
		t.Fatalf("copying %s to %s: %v\n%s", srcDir, asDir, cpErr, out)
	}

	// examples/as-workflow/node_modules/@cleat/{transform,sdk} are real
	// filesystem symlinks into packages/cleat-as -- an ordinary recursive
	// copy preserves them as symlinks POINTING AT THE SAME SHARED FILES, so
	// patching what looks like this test's own copy would actually mutate
	// the real repo source. Replace both with independent, real copies
	// before patching either.
	nodeModulesCleat := filepath.Join(asDir, "node_modules", "@cleat")
	if rmErr := os.RemoveAll(filepath.Join(nodeModulesCleat, "transform")); rmErr != nil {
		t.Fatalf("removing symlinked transform: %v", rmErr)
	}
	if rmErr := os.RemoveAll(filepath.Join(nodeModulesCleat, "sdk")); rmErr != nil {
		t.Fatalf("removing symlinked sdk: %v", rmErr)
	}
	if out, cpErr := exec.Command("rsync", "-a", transformSrcDir+"/",
		filepath.Join(nodeModulesCleat, "transform")+"/").CombinedOutput(); cpErr != nil {
		t.Fatalf("copying %s: %v\n%s", transformSrcDir, cpErr, out)
	}
	if out, cpErr := exec.Command("rsync", "-a", "--exclude=transform", sdkSrcDir+"/",
		filepath.Join(nodeModulesCleat, "sdk")+"/").CombinedOutput(); cpErr != nil {
		t.Fatalf("copying %s: %v\n%s", sdkSrcDir, cpErr, out)
	}

	transformPath := filepath.Join(nodeModulesCleat, "transform", "index.js")
	original, readErr := os.ReadFile(transformPath)
	if readErr != nil {
		t.Fatalf("reading copied transform at %s: %v", transformPath, readErr)
	}
	const marker = "this._writeEntryPointManifest(allNames);"
	if got := strings.Count(string(original), marker); got != 1 {
		t.Fatalf("expected exactly one occurrence of %q in the copied transform, got %d -- "+
			"this test's patch no longer matches transform/index.js's current shape", marker, got)
	}
	patched := strings.Replace(string(original), marker,
		"/* manifest write suppressed by TestAssemblyScriptBuildRefusesAStaleManifest */", 1)
	if writeErr := os.WriteFile(transformPath, []byte(patched), 0o644); writeErr != nil {
		t.Fatalf("patching copied transform: %v", writeErr)
	}

	distDir := filepath.Join(asDir, "dist")
	if mkErr := os.MkdirAll(distDir, 0o755); mkErr != nil {
		t.Fatalf("mkdir %s: %v", distDir, mkErr)
	}
	manifestPath := filepath.Join(distDir, "cleat-entry-points.txt")
	// A REAL export of this build -- not a nonsense name. A nonsense stale
	// name would be caught by verifyEntryPointsAreExports regardless of
	// whether the staleness guard runs at all (it isn't an export of
	// anything), which was this test's first version and went red for the
	// wrong reason when falsified: it "failed" even with the pre-build
	// delete removed, because the pre-existing check caught it by accident.
	// examples/as-workflow genuinely exports cancel_order, just not as the
	// ONLY entry point -- so a stale manifest containing only this name is
	// exactly the silent-wrong-subset case the guard exists to prevent, and
	// nothing else in the pipeline can catch it.
	const staleEntry = "cancel_order"
	if writeErr := os.WriteFile(manifestPath, []byte(staleEntry+"\n"), 0o644); writeErr != nil {
		t.Fatalf("planting stale manifest: %v", writeErr)
	}

	outDir := t.TempDir()
	buildCmd := exec.Command(cleatBinary, "build", "--target", "assemblyscript", "-o", outDir, asDir)
	buildCmd.Dir = repoRoot
	out, buildErr := buildCmd.CombinedOutput()
	if buildErr == nil {
		t.Fatalf("build with a suppressed manifest write and a stale one planted must fail, but exited 0 -- "+
			"it silently embedded the stale, incomplete entry point list instead of refusing:\n%s", out)
	}
	if !strings.Contains(string(out), "no entry-point manifest") {
		t.Fatalf("build failed, but not with the expected \"no entry-point manifest\" error -- the "+
			"pre-build delete should make a suppressed write look identical to an old @cleat/transform "+
			"that never had this mechanism; got:\n%s", out)
	}
	if _, statErr := os.Stat(manifestPath); statErr == nil {
		t.Errorf("the stale manifest file is still present after a refused build -- " +
			"the pre-build delete in build_as.go did not run")
	}
	if matches, _ := filepath.Glob(filepath.Join(outDir, "*.wasm")); len(matches) != 0 {
		t.Errorf("build refused entry-point resolution but still wrote a .wasm output: %v", matches)
	}
}

func TestJavaBuildRefusesAStaleManifest(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	if _, err := exec.LookPath("java"); err != nil {
		t.Skip("java not available")
	}
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync not available")
	}

	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	javaSrcDir := filepath.Join(repoRoot, "examples", "java-workflow")
	cleatJavaSrcDir := filepath.Join(repoRoot, "crates", "cleat-java")
	if _, statErr := os.Stat(filepath.Join(javaSrcDir, "settings.gradle.kts")); statErr != nil {
		t.Fatalf("computed Java example dir %s has no settings.gradle.kts: %v", javaSrcDir, statErr)
	}

	// MEASURED 2026-09-23, falsifying this test: Gradle's own incremental
	// Java compiler ALSO deletes a stale cleat-entry-points.txt on a fresh
	// compile, independently of build_java.go's own pre-build WalkDir
	// delete -- it treats an annotation processor's CLASS_OUTPUT as a
	// directory it owns and prunes anything not regenerated by the current
	// round, including on a brand-new (never-built) tempRoot. Removing
	// build_java.go's own delete and rerunning this exact test still passed:
	// the planted stale file was gone by the time cleat build's WalkDir
	// looked for it, exactly as if the delete had run. So this test proves
	// the end-to-end invariant that matters -- a stale Java manifest is
	// never silently embedded -- but does not, by itself, prove
	// build_java.go's OWN delete loop is what enforces it in THIS
	// environment; keep that loop anyway, as an explicit guard that does not
	// depend on Gradle's incremental-compile behaviour staying this way.
	//
	// settings.gradle.kts declares cleat-java's projectDir as
	// "../../crates/cleat-java", relative to examples/java-workflow -- so the
	// copy has to preserve that same two-levels-up layout rather than
	// copying each project into an unrelated temp dir. tempRoot's own
	// examples/java-workflow and crates/cleat-java reproduce it exactly, so
	// the relative path resolves with no rewriting needed.
	tempRoot := t.TempDir()
	javaDir := filepath.Join(tempRoot, "examples", "java-workflow")
	cleatJavaDir := filepath.Join(tempRoot, "crates", "cleat-java")

	// rsync only creates ONE missing leaf directory, not a chain of them --
	// javaDir and cleatJavaDir are each two levels below tempRoot, and real
	// rsync (3.2.7, the GitHub runner's) refuses with "mkdir ... failed: No
	// such file or directory" when both are missing. macOS's bundled rsync
	// (openrsync) creates the whole chain, which is why this went unnoticed
	// locally and only failed in CI. mkdir the destinations first so both
	// rsyncs land on an existing (empty) directory regardless of which rsync
	// runs them.
	if mkErr := os.MkdirAll(javaDir, 0o755); mkErr != nil {
		t.Fatalf("mkdir %s: %v", javaDir, mkErr)
	}
	if mkErr := os.MkdirAll(cleatJavaDir, 0o755); mkErr != nil {
		t.Fatalf("mkdir %s: %v", cleatJavaDir, mkErr)
	}
	if out, cpErr := exec.Command("rsync", "-a", "--exclude=build",
		javaSrcDir+"/", javaDir+"/").CombinedOutput(); cpErr != nil {
		t.Fatalf("copying %s to %s: %v\n%s", javaSrcDir, javaDir, cpErr, out)
	}
	if out, cpErr := exec.Command("rsync", "-a", "--exclude=.gradle", "--exclude=build",
		cleatJavaSrcDir+"/", cleatJavaDir+"/").CombinedOutput(); cpErr != nil {
		t.Fatalf("copying %s to %s: %v\n%s", cleatJavaSrcDir, cleatJavaDir, cpErr, out)
	}

	processorPath := filepath.Join(cleatJavaDir, "src", "main", "java", "cleat", "CleatEntryProcessor.java")
	original, readErr := os.ReadFile(processorPath)
	if readErr != nil {
		t.Fatalf("reading copied annotation processor at %s: %v", processorPath, readErr)
	}
	const marker = "generateEntryPointManifest();"
	if got := strings.Count(string(original), marker); got != 1 {
		t.Fatalf("expected exactly one occurrence of %q in the copied processor, got %d -- "+
			"this test's patch no longer matches CleatEntryProcessor.java's current shape", marker, got)
	}
	patched := strings.Replace(string(original), marker,
		"/* manifest write suppressed by TestJavaBuildRefusesAStaleManifest */", 1)
	if writeErr := os.WriteFile(processorPath, []byte(patched), 0o644); writeErr != nil {
		t.Fatalf("patching copied annotation processor: %v", writeErr)
	}

	// Planted directly where Filer's CLASS_OUTPUT resolves on a Gradle build
	// (see build_java.go's own comment on why that is a convention, not a
	// guarantee, and searched with WalkDir rather than assumed) -- Gradle has
	// not run in this scratch tree yet, so this is the only manifest that
	// could exist before the build starts.
	buildDir := filepath.Join(javaDir, "build", "classes", "java", "main")
	if mkErr := os.MkdirAll(buildDir, 0o755); mkErr != nil {
		t.Fatalf("mkdir %s: %v", buildDir, mkErr)
	}
	manifestPath := filepath.Join(buildDir, "cleat-entry-points.txt")
	// A REAL export of this build (examples/java-workflow declares
	// place_order AND cancel_order), not a nonsense name -- see the AS
	// test's comment on staleEntry for why a nonsense name is the wrong
	// falsification: it gets caught by verifyEntryPointsAreExports whether
	// or not the staleness guard runs, so it cannot tell the two apart. A
	// stale manifest naming only cancel_order is a real-but-incomplete
	// subset, which is exactly the silent-wrong-list case the guard exists
	// to prevent.
	const staleEntry = "cancel_order"
	if writeErr := os.WriteFile(manifestPath, []byte(staleEntry+"\n"), 0o644); writeErr != nil {
		t.Fatalf("planting stale manifest: %v", writeErr)
	}

	outDir := t.TempDir()
	buildCmd := exec.Command(cleatBinary, "build", "--target", "java", "-o", outDir, javaDir)
	buildCmd.Dir = repoRoot
	out, buildErr := buildCmd.CombinedOutput()
	if buildErr == nil {
		t.Fatalf("build with a suppressed manifest write and a stale one planted must fail, but exited 0 -- "+
			"it silently embedded the stale, incomplete entry point list instead of refusing:\n%s", out)
	}
	if !strings.Contains(string(out), "no entry-point manifest") {
		t.Fatalf("build failed, but not with the expected \"no entry-point manifest\" error, got:\n%s", out)
	}
	if _, statErr := os.Stat(manifestPath); statErr == nil {
		t.Errorf("the stale manifest file is still present after a refused build -- " +
			"the pre-build delete in build_java.go did not run")
	}
	if matches, _ := filepath.Glob(filepath.Join(outDir, "*.wasm")); len(matches) != 0 {
		t.Errorf("build refused entry-point resolution but still wrote a .wasm output: %v", matches)
	}
}

// TestJavaBuildRefusesMultipleManifests is the known-positive
// TestJavaBuildRefusesAStaleManifest's own falsification showed a gap in:
// that test's staleness scenario turned out to be caught by Gradle's own
// incremental compiler pruning a stale CLASS_OUTPUT resource, independently
// of build_java.go's "exactly one manifest" check -- so it never actually
// exercised that check's own refusal. This test does, by making the
// annotation processor itself (in a private copy) emit a SECOND manifest
// under a build/ subdirectory Gradle's compile-output tracking does not
// associate with the first ("leftover", rather than CLASS_OUTPUT's normal
// build/classes/java/main/), so Gradle has no reason to prune either one --
// both are real outputs of the SAME successful compile. WalkDir then finds
// two, and build_java.go must refuse rather than silently picking one (which
// is what it did before the len(manifestPaths) > 1 check was added: the
// first match won, arbitrarily).
func TestJavaBuildRefusesMultipleManifests(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	if _, err := exec.LookPath("java"); err != nil {
		t.Skip("java not available")
	}
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync not available")
	}

	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	javaSrcDir := filepath.Join(repoRoot, "examples", "java-workflow")
	cleatJavaSrcDir := filepath.Join(repoRoot, "crates", "cleat-java")

	tempRoot := t.TempDir()
	javaDir := filepath.Join(tempRoot, "examples", "java-workflow")
	cleatJavaDir := filepath.Join(tempRoot, "crates", "cleat-java")

	// rsync only creates ONE missing leaf directory, not a chain of them --
	// javaDir and cleatJavaDir are each two levels below tempRoot, and real
	// rsync (3.2.7, the GitHub runner's) refuses with "mkdir ... failed: No
	// such file or directory" when both are missing. macOS's bundled rsync
	// (openrsync) creates the whole chain, which is why this went unnoticed
	// locally and only failed in CI. mkdir the destinations first so both
	// rsyncs land on an existing (empty) directory regardless of which rsync
	// runs them.
	if mkErr := os.MkdirAll(javaDir, 0o755); mkErr != nil {
		t.Fatalf("mkdir %s: %v", javaDir, mkErr)
	}
	if mkErr := os.MkdirAll(cleatJavaDir, 0o755); mkErr != nil {
		t.Fatalf("mkdir %s: %v", cleatJavaDir, mkErr)
	}
	if out, cpErr := exec.Command("rsync", "-a", "--exclude=build",
		javaSrcDir+"/", javaDir+"/").CombinedOutput(); cpErr != nil {
		t.Fatalf("copying %s to %s: %v\n%s", javaSrcDir, javaDir, cpErr, out)
	}
	if out, cpErr := exec.Command("rsync", "-a", "--exclude=.gradle", "--exclude=build",
		cleatJavaSrcDir+"/", cleatJavaDir+"/").CombinedOutput(); cpErr != nil {
		t.Fatalf("copying %s to %s: %v\n%s", cleatJavaSrcDir, cleatJavaDir, cpErr, out)
	}

	processorPath := filepath.Join(cleatJavaDir, "src", "main", "java", "cleat", "CleatEntryProcessor.java")
	original, readErr := os.ReadFile(processorPath)
	if readErr != nil {
		t.Fatalf("reading copied annotation processor at %s: %v", processorPath, readErr)
	}
	const markerBody = `    private void generateEntryPointManifest() {
        try {
            FileObject file = processingEnv.getFiler().createResource(
                StandardLocation.CLASS_OUTPUT, "", "cleat-entry-points.txt");
            try (Writer out = file.openWriter()) {
                for (String fqcn : generatedWrappers) {
                    String exportName = wrapperExportNames.get(fqcn);
                    if (exportName != null) {
                        out.write(exportName);
                        out.write("\n");
                    }
                }
            }
        }` // the try body ends HERE, deliberately before "catch" -- the
	// injected second write must land INSIDE the try (so its own
	// IOException still routes to the existing catch), not after it.
	const markerCatch = ` catch (IOException e) {`
	marker := markerBody + markerCatch
	if got := strings.Count(string(original), marker); got != 1 {
		t.Fatalf("expected exactly one occurrence of the generateEntryPointManifest() body in the copied "+
			"processor, got %d -- this test's patch no longer matches CleatEntryProcessor.java's current shape", got)
	}
	// TEST-INJECTED second write, to a DIFFERENT CLASS_OUTPUT subdirectory --
	// a second, real output of the same successful annotation-processing
	// round, not a suppressed or stale one. This is what makes it a genuine
	// exercise of the ">1" branch rather than a repeat of the staleness test.
	// Spliced in BEFORE the closing brace of the outer try (markerBody's own
	// last character), so it shares that try's existing catch instead of
	// needing one of its own.
	injected := `
            FileObject leftover = processingEnv.getFiler().createResource(
                StandardLocation.CLASS_OUTPUT, "leftover", "cleat-entry-points.txt");
            try (Writer out2 = leftover.openWriter()) {
                for (String fqcn : generatedWrappers) {
                    String exportName = wrapperExportNames.get(fqcn);
                    if (exportName != null) {
                        out2.write(exportName);
                        out2.write("\n");
                    }
                }
            }
`
	patchedBody := strings.TrimSuffix(markerBody, "}") + injected + "}"
	patched := strings.Replace(string(original), marker, patchedBody+markerCatch, 1)
	if writeErr := os.WriteFile(processorPath, []byte(patched), 0o644); writeErr != nil {
		t.Fatalf("patching copied annotation processor: %v", writeErr)
	}

	outDir := t.TempDir()
	buildCmd := exec.Command(cleatBinary, "build", "--target", "java", "-o", outDir, javaDir)
	buildCmd.Dir = repoRoot
	out, buildErr := buildCmd.CombinedOutput()
	if buildErr == nil {
		t.Fatalf("build that produced two entry-point manifests must fail, but exited 0 -- "+
			"it must not silently pick one of two disagreeing manifests:\n%s", out)
	}
	if !strings.Contains(string(out), "expected exactly one") {
		t.Fatalf("build failed, but not with the expected \"expected exactly one\" error, got:\n%s", out)
	}
	if !strings.Contains(string(out), "2 entry-point manifests") {
		t.Fatalf("error does not report finding 2 manifests, got:\n%s", out)
	}
	if matches, _ := filepath.Glob(filepath.Join(outDir, "*.wasm")); len(matches) != 0 {
		t.Errorf("build refused entry-point resolution but still wrote a .wasm output: %v", matches)
	}
}
