package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMethodUsingGlobalHIsRejected is the end-to-end half of cleat#1614.
//
// Before the fix, testdata/methodglobalh built clean. `cleat build` printed
// "Verifying HostCalls threading... OK", left Wait out of the auto-threading
// list, kept the unassigned `var h cleat.HostCalls` in the emitted package,
// and wrote a .wasm whose first call to Wait dereferenced a nil
// *HostCallsImpl.
//
// The verifier and the transform disagreed about what "has access to h"
// means: phase 0 credited any durable function referencing the global, and
// transform.go skips every receiver because a method cannot be given a first
// parameter without changing its signature.
func TestMethodUsingGlobalHIsRejected(t *testing.T) {
	outDir := t.TempDir()
	src := filepath.Join("..", "..", "testdata", "methodglobalh")

	cmd := exec.Command(cleatBinary, "build", "--target", "go", "-o", outDir, src)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("cleat build ACCEPTED a method reaching the host through the package-level h; "+
			"the emitted module would nil-panic on first use:\n%s", out)
	}

	got := string(out)
	// Name the method, so the author knows which one.
	if !strings.Contains(got, "Wait") {
		t.Errorf("the error does not name the offending method:\n%s", got)
	}
	// Both working routes must be offered. The pre-1614 message told the
	// author to "declare a package-level 'var h cleat.HostCalls'", which is
	// the thing that produces this crash.
	if !strings.Contains(got, "field") {
		t.Errorf("the error does not offer the receiver-field route:\n%s", got)
	}
	if strings.Contains(got, "declare a package-level") {
		t.Errorf("the error still recommends the package-level global, which is what panics:\n%s", got)
	}
}

// TestMethodWithHostCallsFieldStillBuilds is the floor assertion for the test
// above: the tightening must reject a method that CANNOT reach the host, not
// methods in general.
//
// testdata/methodreceiverfield is the same Retrier.Wait, reaching the host
// through a cleat.HostCalls field on the receiver -- the struct_methods
// pattern, admitted by phase 3. Without this test, "no method may ever reach
// the host" would pass the test above.
func TestMethodWithHostCallsFieldStillBuilds(t *testing.T) {
	outDir := t.TempDir()
	src := filepath.Join("..", "..", "testdata", "methodreceiverfield")

	cmd := exec.Command(cleatBinary, "build", "--target", "go", "-o", outDir, src)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cleat build rejected a method that reaches the host through a receiver field:\n%s\n%v", out, err)
	}
	if !strings.Contains(string(out), "Verifying HostCalls threading... OK") {
		t.Errorf("expected the threading check to pass:\n%s", out)
	}
}
