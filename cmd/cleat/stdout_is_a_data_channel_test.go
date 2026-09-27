package main

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// `cleat build` must not report failures on stdout. cleat#1128.
//
// THE CASE THIS PINS NEEDS NO JUDGEMENT ABOUT WHICH STREAM A WARNING BELONGS
// ON, which is why it is the one worth having. Before this change:
//
//	cleat build ./testdata/vet-checks/go/e013_sync_mutex > /dev/null
//	  exit 1, stdout 595 bytes, stderr 0 bytes
//
// So the build failed, said why, and a caller redirecting stdout -- to a file,
// to /dev/null, into a pipe -- saw an exit code and nothing else. That is not a
// question about conventions; it is a command that cannot be used in a script.
// After: exit 1, stdout 0 bytes, stderr 595 bytes. The same bytes, the other
// way round.
//
// SEPARATE ARMS RATHER THAN CombinedOutput, deliberately. The helper this
// package already has for vet merges the two streams and then hunts for the
// JSON, which cannot see this defect at all -- everything is present in the
// combined output whichever stream it came from. A test that could not fail
// before the fix would be worse than no test.
func TestBuildReportsFailuresOnStderrNotStdout(t *testing.T) {
	// No testing.Short() guard. The sibling vet helper has one because its
	// fixtures are slow; these two run in under a second each, so a skip here
	// would buy nothing and cost the thing scripts/check-skips.sh exists to
	// stop -- a skip is indistinguishable from a pass.
	if cleatBinary == "" {
		t.Fatal("UNMEASURED: cleatBinary is unset, so nothing was run")
	}

	// A fixture that LOADS and then fails analysis. A package that fails to
	// load reports through a different path that already used stderr, so it
	// would pass this test without the change.
	pkg := filepath.Join("..", "..", "testdata", "vet-checks", "go", "e013_sync_mutex")

	cmd := exec.Command(cleatBinary, "build", pkg)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	if err == nil {
		t.Fatalf("UNMEASURED: the fixture built successfully, so there is no failure "+
			"to report and every assertion below is vacuous.\nstderr: %s", stderr.String())
	}

	if !strings.Contains(stderr.String(), "E013") {
		t.Errorf("the failure is not reported on stderr.\nstderr: %q", stderr.String())
	}

	// The assertion that fails before the change.
	if stdout.Len() != 0 {
		t.Errorf("a failing build wrote %d bytes to stdout:\n%s\n\n"+
			"stdout is a data channel: it carries what the caller asked for, never "+
			"commentary about the build. Anything here means "+
			"`cleat build ... > out` writes commentary into out, and "+
			"`cleat build ... > /dev/null` fails with no explanation at all.",
			stdout.Len(), stdout.String())
	}
}

// The success path has the same contract, and it is worth its own arm because
// the failure path exits early -- a writer reached only after a successful
// compile is invisible to the test above.
func TestBuildWritesNothingToStdoutOnSuccess(t *testing.T) {
	// No testing.Short() guard. The sibling vet helper has one because its
	// fixtures are slow; these two run in under a second each, so a skip here
	// would buy nothing and cost the thing scripts/check-skips.sh exists to
	// stop -- a skip is indistinguishable from a pass.
	if cleatBinary == "" {
		t.Fatal("UNMEASURED: cleatBinary is unset, so nothing was run")
	}

	// testdata/autothread is the package TestBuildAutoThreadedPackage builds
	// successfully, so it is known to reach the end of the build rather than
	// chosen hopefully.
	src := filepath.Join("..", "..", "testdata", "autothread")
	outDir := t.TempDir()

	cmd := exec.Command(cleatBinary, "build", "--target", "go", "-o", outDir, src)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Fatal rather than Skip. testdata/autothread is built successfully by
		// TestBuildAutoThreadedPackage in this same package, so "it did not
		// build" is a real breakage in this repository rather than an absent
		// optional resource -- case (c) in scripts/check-skips.sh. Skipping
		// here would hide exactly the regression that makes the arm worth
		// having.
		t.Fatalf("the fixture TestBuildAutoThreadedPackage builds did not build: %v\n"+
			"stderr: %s", err, stderr.String())
	}

	if stderr.Len() == 0 {
		t.Error("a successful build printed no progress at all, on either stream -- " +
			"this test would pass against a silent binary")
	}
	if stdout.Len() != 0 {
		t.Errorf("a successful build wrote %d bytes to stdout:\n%s",
			stdout.Len(), stdout.String())
	}
}
