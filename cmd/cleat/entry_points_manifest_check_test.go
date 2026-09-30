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

// TestBuildIgnoresAnAbsentEntryPointsManifest is the negative control for the
// test above: entry_points: is optional, so a package with no cleat.yaml at
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
