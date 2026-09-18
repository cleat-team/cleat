package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// cleat#1888. Every Go template produced a project that did not build, and
// every scaffold test passed, because the tests asserted that files existed
// and contained expected strings. None of them built the output.
//
// That gap hid three independent defects at once, which is the argument for
// this test existing rather than for any one of the fixes:
//
//   - workflow and fullstack pinned `github.com/cleat-team/cleat v0.0.0`, a
//     tag that has never existed;
//   - after that was corrected to a stamped version, the require still named
//     the PARENT module while the generated code imports
//     github.com/cleat-team/cleat/cleat -- a separate module (cleat/go.mod)
//     which has never been tagged at all, so no version string could work and
//     the build failed on a missing go.sum entry;
//   - and underneath both, workflow and fullstack declared `func main() {}`
//     plus a raw //go:wasmexport directive, which collide with the stub
//     `cleat build` generates ("other declaration of main", "symbol process
//     redeclared") and which Go's toolchain rejects for these parameter types.
//
// A reader could not have found any of them from the templates; only running
// the documented command does. So this runs the documented command.
//
// THIS REPLACES TestScaffoldedProjectsActuallyBuild, which cleat#1891 added
// and which ran `go vet` rather than `cleat build`. Its reasoning is worth
// repeating because the misstep is easy to make again: `go build` on a basic
// or agent scaffold fails at the LINK step ("function main is undeclared"),
// since those templates deliberately have no main, so it reached for a
// command that would pass. But the answer to that is `cleat build` -- which
// generates the missing main -- not a weaker command. Redefining the defect
// as "does the module graph resolve" rather than "does the documented command
// work" is what let it stay green over all three defects above.
//
// One observation from it is kept because it is load-bearing and was
// verified rather than assumed: `sdkReplaceDir` (wasm/build.go), which finds
// this repo's own cleat/ by walking up from a project directory, is called
// only by `cleat build`'s pipeline and never by `cleat init`. A scaffold
// created in t.TempDir() therefore resolves the SDK from the real module
// proxy exactly as an external user's would, nowhere near this checkout.
//
// NOT GUARDED ON NETWORK AVAILABILITY. `go mod tidy` in the scaffold needs the
// module proxy, and that is a real dependency of the thing under test rather
// than an optional resource: a scaffold that cannot resolve its own SDK is
// exactly the failure this test exists to catch. Skipping when the network is
// absent would make the test report clean in the one situation where it has
// measured nothing -- the shape scripts/check-skips.sh calls case (b).
func TestEveryGoTemplateScaffoldsIntoAProjectThatBuilds(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}

	// agent-python is deliberately absent: its documented first step is
	// `pip install -r requirements.txt` and its checker needs Python >= 3.10,
	// so building it here would test the environment, not the template.
	for _, tc := range []struct {
		template string
		artifact string
	}{
		{"basic", "hello.wasm"},
		{"agent", "agent_loop.wasm"},
		{"workflow", "process.wasm"},
		{"fullstack", "submit_order.wasm"},
	} {
		t.Run(tc.template, func(t *testing.T) {
			root := t.TempDir()
			name := "p_" + tc.template

			out, err := runCleatIn(t, root, "init", "--template", tc.template, name)
			if err != nil {
				t.Fatalf("cleat init --template %s failed: %v\n%s", tc.template, err, out)
			}

			proj := filepath.Join(root, name)
			out, err = runCleatIn(t, proj, "build", "-o", "./out", ".")
			if err != nil {
				t.Fatalf("a scaffolded %s project does not build.\n"+
					"This is the first command its own README tells a new user to run.\n%v\n%s",
					tc.template, err, out)
			}

			// An exit code of 0 is not the claim -- an artifact is. `cleat
			// build` has previously printed its analysis and emitted nothing.
			art := filepath.Join(proj, "out", tc.artifact)
			if _, statErr := os.Stat(art); statErr != nil {
				listing, _ := os.ReadDir(filepath.Join(proj, "out"))
				var names []string
				for _, e := range listing {
					names = append(names, e.Name())
				}
				t.Errorf("build succeeded but %s was not emitted; out/ contains %v", tc.artifact, names)
			}
		})
	}
}

func runCleatIn(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(cleatBinary, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
