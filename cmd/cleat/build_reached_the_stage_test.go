package main

import (
	"strings"
	"testing"
)

// The markers an absence-asserting arm can use to prove that a build reached the
// stage it asserts about. They are two different KINDS of line, and which one a
// language needs is a property of how that language's checker runs.
const (
	// rustVetMarker is the Rust checker's OPENING line (vet_rust.go:852).
	//
	// The Rust checker is pure Go, so printing this line means the checker then
	// runs to completion: nothing between the line and the verdict can fail.
	rustVetMarker = "Vetting Rust crate"

	// asBuildMarker is the AssemblyScript build's COMPLETION line
	// (build_as.go:183, `fmt.Printf("  Wrote %s (%s)")`).
	//
	// A completion line rather than an opening one, because the AS checker is a
	// SUBPROCESS: `Compiling AssemblyScript to WASM...` (build_as.go:75) is
	// printed before `npx asc` runs, and asc can then fail on its own -- so the
	// opening line does not carry the guarantee the Rust one does. `  Wrote ` is
	// printed only when the whole build, checker included, completed.
	//
	// This is the file's own signal used the other way round: ARM 1 of
	// as_build_refuses_nondeterminism_test.go asserts `!Contains(out, "Wrote ")`
	// for the refused case, which is only meaningful if a non-refused build
	// prints it.
	asBuildMarker = "  Wrote "
)

// requireReachedStage asserts that a build reached the stage an
// absence-asserting arm depends on, so that "no finding was reported" means the
// check looked and found nothing rather than that nothing looked.
//
// cleat#3063. An absence is satisfied by EMPTY output, which is also what a build
// that never started produces. These fixtures keep their sources where the later
// compile stage rejects them, so the arms necessarily assert on TEXT -- which is
// what leaves them open to the one text they cannot tell from silence.
//
// marker is a parameter rather than a default because the two callers pass
// different KINDS of line, and that is a fact about each checker rather than a
// style choice: Rust's checker runs in-process, so its opening line suffices;
// AssemblyScript's runs as a subprocess, so only its completion line does. Copying
// one language's marker to another is exactly the mistake this measures against --
// see the Java sibling, which asserts "Vetting Java project" (an opening line,
// correctly, for the same in-process reason as Rust).
//
// This check is SEPARABLE from the exit-status check in each caller's `build`
// helper, and that is deliberate rather than incidental. A build that fails to
// start is caught by the exit status there; a build that starts and writes no
// output is caught here. On one input -- a binary that runs, exits 0 and prints
// nothing -- exactly one of the two fires, so a reader can tell which check did
// the work. Without that separation a fix could pass with one of its two checks
// doing all the work under the other's name, and the green would not say which.
// An editor changing either check should keep them separable for that reason.
//
// NAMING. The Java sibling defines its own requireTheCheckerRan in
// java_build_refuses_nondeterminism_test.go (cleat#3056/#3062). This package is
// shared, so a second function of that name would not compile; and this file
// cannot call that one instead, because #3062 may not have landed yet and a branch
// referencing a helper from an unmerged PR does not build on its own base. Once
// both have landed the two should be unified -- this one takes the marker and
// serves all three. Noted rather than done here, because doing it now would make
// this PR's build depend on that PR's merge order.
func requireReachedStage(t *testing.T, out, marker string) {
	t.Helper()
	if !strings.Contains(out, marker) {
		t.Fatalf("the build never printed %q, the line that proves it reached the stage this "+
			"arm's absence is asserted against -- so that absence has nothing behind it, and "+
			"this arm would have passed having measured nothing.\n\n"+
			"An absence-asserting arm is satisfied by empty output, which is also what a build "+
			"that never started produces. cleat#3063.\noutput:\n%s", marker, out)
	}
}
