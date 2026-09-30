// TestBuildRefusesAnEntryPointsManifestMismatch is cleat#2698's known-positive:
// before checkEntryPointsAgainstManifest (cmd/cleat/main.go) existed,
// cleat.yaml's entry_points: was never parsed by anything, in either of the
// two incompatible shapes the tree carried (a list of strings, and a list of
// {name, function} maps written only by the agent scaffold and its
// generator) -- so a manifest that disagreed with the actual build was
// indistinguishable from one that agreed. This proves the check actually
// fires on a real disagreement, not just that it stays quiet on the
// scaffolds' own generated manifests (TestEveryGoTemplateScaffoldsIntoAProjectThatBuilds
// covers that side).
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildRefusesAnEntryPointsManifestMismatch(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}

	root := t.TempDir()
	name := "p_mismatch"
	out, err := runCleatIn(t, root, "init", "--template", "basic", name)
	if err != nil {
		t.Fatalf("cleat init --template basic failed: %v\n%s", err, out)
	}
	proj := filepath.Join(root, name)
	resolveScaffoldAgainstThisCheckout(t, proj)

	// scaffoldBasic's own entry point is Hello (main.go), so writeYAML wrote
	// "entry_points: [hello]". Corrupt it to a name the build cannot
	// possibly produce -- the actual mismatch this check exists to catch,
	// not a name that just happens to sort differently.
	yamlPath := filepath.Join(proj, "cleat.yaml")
	original, readErr := os.ReadFile(yamlPath)
	if readErr != nil {
		t.Fatalf("reading scaffolded cleat.yaml: %v", readErr)
	}
	if !strings.Contains(string(original), "- hello") {
		t.Fatalf("scaffolded cleat.yaml does not contain the expected 'hello' entry point -- "+
			"this test's assumption about writeYAML's output no longer holds:\n%s", original)
	}
	corrupted := strings.Replace(string(original), "- hello", "- goodbye", 1)
	if writeErr := os.WriteFile(yamlPath, []byte(corrupted), 0o644); writeErr != nil {
		t.Fatalf("writing corrupted cleat.yaml: %v", writeErr)
	}

	out, buildErr := runCleatIn(t, proj, "build", "-o", "./out", ".")
	if buildErr == nil {
		t.Fatalf("build with a cleat.yaml entry_points: that disagrees with the actual code must "+
			"fail, but exited 0 -- it silently ignored the manifest:\n%s", out)
	}
	if !strings.Contains(out, "does not match what cleat build") {
		t.Fatalf("build failed, but not with the expected entry_points mismatch error, got:\n%s", out)
	}
	if !strings.Contains(out, "goodbye") || !strings.Contains(out, "hello") {
		t.Errorf("error message should name both the declared and the actual entry point, got:\n%s", out)
	}
	if matches, _ := filepath.Glob(filepath.Join(proj, "out", "*.wasm")); len(matches) != 0 {
		t.Errorf("build refused the manifest check but still wrote a .wasm output: %v", matches)
	}
}

// TestBuildRefusesTheOldMapShapedManifest is cleat-review's cleat#2812 R1
// finding, turned into a known-positive: workflowManifestEntryPoints's first
// version unmarshalled entry_points: straight into []string and returned nil
// on any decode error, which reads the OLD {name, function} map shape --
// exactly what every PRE-#2812 `cleat init --template agent` project's
// cleat.yaml carries, with the wrong name (agent/AgentLoop, which never
// matches what cleat build actually finds for scaffoldBasic's Hello) -- as
// "absent" and skips it silently. This is the manifest MOST likely to be
// wrong, and it is the one TestEveryGoTemplateScaffoldsIntoAProjectThatBuilds
// cannot catch, because that test only ever sees a FRESHLY generated,
// already-correct manifest -- it was never going to exercise a stale one
// written before this PR existed.
func TestBuildRefusesTheOldMapShapedManifest(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}

	root := t.TempDir()
	name := "p_oldshape"
	out, err := runCleatIn(t, root, "init", "--template", "basic", name)
	if err != nil {
		t.Fatalf("cleat init --template basic failed: %v\n%s", err, out)
	}
	proj := filepath.Join(root, name)
	resolveScaffoldAgainstThisCheckout(t, proj)

	// A real pre-#2812 agent scaffold's cleat.yaml, transplanted onto
	// scaffoldBasic's Hello entry point -- the map form always named
	// agent/AgentLoop regardless of which scaffold called writeYAML, so this
	// is exactly what a `basic` project generated before this PR would have
	// carried.
	yamlPath := filepath.Join(proj, "cleat.yaml")
	oldShape := "name: \"p_oldshape\"\nlanguage: go\nentry_points:\n  - name: agent\n    function: AgentLoop\n"
	if writeErr := os.WriteFile(yamlPath, []byte(oldShape), 0o644); writeErr != nil {
		t.Fatalf("writing old-shape cleat.yaml: %v", writeErr)
	}

	out, buildErr := runCleatIn(t, proj, "build", "-o", "./out", ".")
	if buildErr == nil {
		t.Fatalf("build with an old {name, function}-shaped entry_points: must fail, but exited 0 -- "+
			"it silently treated the malformed manifest as absent:\n%s", out)
	}
	if !strings.Contains(out, "is not a list of strings") {
		t.Fatalf("build failed, but not with the expected malformed-manifest error, got:\n%s", out)
	}
	if matches, _ := filepath.Glob(filepath.Join(proj, "out", "*.wasm")); len(matches) != 0 {
		t.Errorf("build refused the manifest check but still wrote a .wasm output: %v", matches)
	}
}

// TestBuildIgnoresAnAbsentEntryPointsManifest is the negative control for the
// tests above: entry_points: is optional, so a package with no cleat.yaml at
// all -- not a corrupted one, an absent one -- must build exactly as it did
// before checkEntryPointsAgainstManifest existed.
func TestBuildIgnoresAnAbsentEntryPointsManifest(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}

	root := t.TempDir()
	name := "p_nomanifest"
	out, err := runCleatIn(t, root, "init", "--template", "basic", name)
	if err != nil {
		t.Fatalf("cleat init --template basic failed: %v\n%s", err, out)
	}
	proj := filepath.Join(root, name)
	resolveScaffoldAgainstThisCheckout(t, proj)

	if rmErr := os.Remove(filepath.Join(proj, "cleat.yaml")); rmErr != nil {
		t.Fatalf("removing scaffolded cleat.yaml: %v", rmErr)
	}

	out, buildErr := runCleatIn(t, proj, "build", "-o", "./out", ".")
	if buildErr != nil {
		t.Fatalf("build with no cleat.yaml at all must still succeed (entry_points: is optional): %v\n%s",
			buildErr, out)
	}
	if matches, _ := filepath.Glob(filepath.Join(proj, "out", "*.wasm")); len(matches) != 1 {
		t.Errorf("expected exactly one .wasm output, got %v", matches)
	}
}
