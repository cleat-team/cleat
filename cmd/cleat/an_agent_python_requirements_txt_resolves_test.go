package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestAnAgentPythonRequirementsTxtResolves is the regression test for
// cleat#1971. requirements.txt pinned `cleat-sdk>=0.2.0`, which has never
// been published to PyPI, so `pip install -r requirements.txt` -- the FIRST
// command the template's own README tells a new user to run -- failed
// unconditionally, on every machine, for every user.
//
// Nothing caught it. agent-python is excluded from
// TestEveryGoTemplateScaffoldsIntoAProjectThatBuilds
// (a_scaffold_builds_test.go) because building it needs Python >= 3.10, and
// that exclusion took the package-resolution question down with it -- which
// the reason given for the exclusion never actually covered. A pip resolve
// failure is a property of the pinned package name, not of the build
// toolchain.
//
// So this checks resolution only, with `pip download --no-deps`, and
// deliberately does not attempt `cleat build` on the scaffold -- that stays
// out of scope for the same Python-version reason the sibling test states.
//
// NOT GUARDED ON NETWORK AVAILABILITY, for the same reason as the sibling
// test: resolving the template's own dependencies against a real index is
// the thing under test, and skipping when the network is absent would report
// clean in the one situation this test has measured nothing.
func TestAnAgentPythonRequirementsTxtResolves(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}

	pip := findPipOnPathOrSkip(t)

	root := t.TempDir()
	out, err := runCleatIn(t, root, "init", "--template", "agent-python", "p_agent_python")
	if err != nil {
		t.Fatalf("cleat init --template agent-python failed: %v\n%s", err, out)
	}

	reqs := filepath.Join(root, "p_agent_python", "requirements.txt")
	dl := t.TempDir()

	cmd := exec.Command(pip, "download", "--no-deps", "-r", reqs, "-d", dl)
	pipOut, pipErr := cmd.CombinedOutput()
	if pipErr == nil {
		return
	}

	// The one legitimate environmental precondition: the interpreter pip is
	// bound to is older than the SDK's own `requires-python` (pythonSDKMinVersion,
	// vet_python_hint.go) -- the exact reason a_scaffold_builds_test.go already
	// excludes this template from the full build. Anything else is a real
	// resolution failure and must fail the test, not skip it.
	if strings.Contains(string(pipOut), "requires a different Python") {
		t.Skipf("pip on PATH is bound to a Python older than the SDK needs (>= %s): %s\n%s",
			pythonSDKMinVersion, pythonVersionOnPath(), pipOut)
	}

	t.Fatalf("requirements.txt does not resolve -- this is the FIRST command "+
		"the template's own README tells a new user to run.\n%v\n%s", pipErr, pipOut)
}

// findPipOnPathOrSkip locates pip. Absence is a genuine environmental
// precondition (a toolchain that is not installed), not the defect under
// test, so it skips rather than fails.
func findPipOnPathOrSkip(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"pip3", "pip"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	t.Skip("neither pip3 nor pip is on PATH")
	return ""
}
