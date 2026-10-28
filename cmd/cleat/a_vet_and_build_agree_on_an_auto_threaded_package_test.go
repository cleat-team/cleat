package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// cleat#3524. `cleat build` filtered the threading errors the transform fixes
// (IMPROVEMENT-PLAN 3.229) only in its plain path, so on a package that
// auto-threads, `cleat vet` and `cleat build --json` exited 1 on an error that
// `cleat build` accepts. Measured on testdata/autothread before the change:
// build exit 0, vet exit 1, build --json exit 1.
//
// A pre-push step that disagrees with the build it stands in for is worse than
// none, so these pin that the three answer the same, and that the filter did not
// widen into swallowing a real error.
func TestVetAndBuildAgreeOnAnAutoThreadedPackage(t *testing.T) {
	needBinary(t)
	src := filepath.Join("..", "..", "testdata", "autothread")

	for _, c := range []struct {
		name string
		args []string
	}{
		{"cleat vet", []string{"vet", src}},
		{"cleat build", []string{"build", "-o", t.TempDir(), src}},
		{"cleat build --json", []string{"build", "--json", "-o", t.TempDir(), src}},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, err := exec.Command(cleatBinary, c.args...).CombinedOutput()
			if err != nil {
				t.Errorf("`%s` rejected a package that auto-threads (the other two accept it):\n%s", c.name, out)
			}
		})
	}
}

// TestVetStillRejectsAGenuineThreadingError is the floor for the test above:
// testdata/methodglobalh has a method that reaches the host through the
// package-level h, which the transform cannot thread (a method has no free first
// parameter), so its threading error is real and must survive the filter in
// every command.
func TestVetStillRejectsAGenuineThreadingError(t *testing.T) {
	needBinary(t)
	src := filepath.Join("..", "..", "testdata", "methodglobalh")

	for _, c := range []struct {
		name string
		args []string
	}{
		{"cleat vet", []string{"vet", src}},
		{"cleat build", []string{"build", "-o", t.TempDir(), src}},
		{"cleat build --json", []string{"build", "--json", "-o", t.TempDir(), src}},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, err := exec.Command(cleatBinary, c.args...).CombinedOutput()
			if err == nil {
				t.Fatalf("`%s` accepted a method reaching the host through the package-level h:\n%s", c.name, out)
			}
			if !strings.Contains(string(out), "Wait") {
				t.Errorf("`%s` failed without naming the offending method:\n%s", c.name, out)
			}
		})
	}
}
