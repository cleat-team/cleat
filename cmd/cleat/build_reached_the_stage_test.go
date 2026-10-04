package main

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// Shared preconditions for the build-refusal tests, one set for all three
// languages.
//
// TestJavaBuildRefusesNondeterminism, TestRustBuildRefusesNondeterminism and
// TestASBuildRefusesNondeterminism each drive `cleat build` and then assert
// something about the determinism checker's verdict. TWO preconditions have to hold
// before any such assertion means anything, and both now live here:
//
//  1. the build STARTED (requireBuildStarted). Otherwise the output is empty, and
//     every absence-assertion downstream holds for a reason with nothing behind it.
//  2. the build REACHED the checker (requireReachedStage). Otherwise "no finding was
//     reported" means "nothing looked".
//
// Each was written in one language first and generalised here afterwards; cleat#3075
// records why they are one set rather than a copy per file, and why their failure
// text is built by the two Msg functions rather than inline at each call site.
const (
	// javaVetMarker is the Java checker's OPENING line (vet_java.go:541). The Java
	// checker is pure Go, so printing this line means it then runs to completion.
	javaVetMarker = "Vetting Java project"

	// rustVetMarker is the Rust checker's OPENING line (vet_rust.go:852), for the
	// same in-process reason as Java's.
	rustVetMarker = "Vetting Rust crate"

	// asBuildMarker is the AssemblyScript build's COMPLETION line
	// (build_as.go:183, `fmt.Printf("  Wrote %s (%s)")`).
	//
	// A completion line rather than an opening one, because the AS checker is a
	// SUBPROCESS: `Compiling AssemblyScript to WASM...` (build_as.go:75) is printed
	// before `npx asc` runs, and asc can then fail on its own -- so the opening line
	// does not carry the guarantee the other two do. `  Wrote ` is printed only when
	// the whole build, checker included, completed.
	//
	// This is the file's own signal used the other way round: ARM 1 of
	// as_build_refuses_nondeterminism_test.go asserts `!Contains(out, "Wrote ")` for
	// the refused case, which is only meaningful if a non-refused build prints it.
	asBuildMarker = "  Wrote "
)

// The two failure texts are built here rather than written inline at each call site,
// so that their DISJOINTNESS can be asserted (TestTheTwoFailureMessagesAreDisjoint)
// instead of remembered.
//
// Why that matters, measured: cleat-review was counting which of the two checks fired
// during #3071's review, grepped "never started", and got 6 where the answer was 0 --
// the phrase also appeared in the OTHER check's message. A search satisfied by text
// present for an unrelated reason, in the diagnostics for the very defect those arms
// had (cleat#3075).
func buildStartedMsg(err error) string {
	return fmt.Sprintf("the build never started: %v\n\n"+
		"Nothing ran, so every assertion in this test would be about the absence of output "+
		"rather than about the checker. cleat#3056, cleat#3063, cleat#3075.", err)
}

func reachedStageMsg(marker, out string) string {
	return fmt.Sprintf("the build did not reach the checker: %q is absent from its output, so "+
		"the absence asserted below has nothing behind it and this arm would have passed "+
		"having measured nothing.\n\n"+
		"Empty output satisfies an absence, and empty output is also what a build that stopped "+
		"before this point produces. cleat#3056, cleat#3063, cleat#3075.\noutput:\n%s", marker, out)
}

// requireBuildStarted fails the test when the build never started.
//
// A build that fails to start -- exec failed, so there is no ExitError -- produces
// EMPTY output, and every absence-assertion in these tests then holds for a reason
// that has nothing to do with the checker. That is a failed measurement rather than
// a result.
//
// A NON-ZERO EXIT is not a failure here. Each fixture fails at a later compile stage
// on its own -- Java and Rust by crate/project layout, AS by toolchain -- and the
// arms assert on the TEXT. Only a failure to START is fatal.
func requireBuildStarted(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return // it started; how it ended is the arms' business, not this helper's
	}
	t.Fatalf("%s", buildStartedMsg(err))
}

// requireReachedStage asserts that a build reached the stage an absence-asserting
// arm depends on, so that "no finding was reported" means the check looked and found
// nothing rather than that nothing looked.
//
// marker is a parameter rather than a default because the three callers pass
// different KINDS of line, and that is a fact about each checker rather than a style
// choice: Java's and Rust's run in-process, so their opening line suffices;
// AssemblyScript's runs as a subprocess, so only its completion line does. Copying
// one language's marker to another is the mistake this measures against.
//
// This check is SEPARABLE from requireBuildStarted, and deliberately so rather than
// incidentally. A build that never starts is caught there; a build that starts and
// writes no output is caught here. On one input -- a binary that runs, exits 0 and
// prints nothing -- exactly one of the two fires, so a reader can tell which check
// did the work from the failure text alone, and
// TestTheTwoFailureMessagesAreDisjoint keeps it that way. Without the separation a
// fix could pass with one of its two checks doing all the work under the other's
// name, and the green would not say which.
func requireReachedStage(t *testing.T, out, marker string) {
	t.Helper()
	if !strings.Contains(out, marker) {
		t.Fatalf("%s", reachedStageMsg(marker, out))
	}
}

// TestTheTwoFailureMessagesAreDisjoint pins the property those messages were written
// for: a reader must be able to tell WHICH check fired from the failure text alone.
//
// Full disjointness is neither achievable nor the claim -- both messages necessarily
// contain "build" and "output". What is pinned are the DISTINCTIVE phrases a reader
// actually greps, one set per message, and the assertion is symmetric: each phrase
// must be present in its own message and absent from the other's.
func TestTheTwoFailureMessagesAreDisjoint(t *testing.T) {
	type msg struct {
		name    string
		text    string
		other   string
		phrases []string // must appear in this message and in none of the others
	}
	for _, m := range []msg{
		{
			name:    "buildStartedMsg",
			text:    buildStartedMsg(errors.New("exec: no such file or directory")),
			other:   reachedStageMsg(rustVetMarker, "some output"),
			phrases: []string{"never started", "Nothing ran"},
		},
		{
			name:    "reachedStageMsg",
			text:    reachedStageMsg(rustVetMarker, "some output"),
			other:   buildStartedMsg(errors.New("exec: no such file or directory")),
			phrases: []string{"did not reach the checker"},
		},
	} {
		for _, p := range m.phrases {
			if !strings.Contains(m.text, p) {
				t.Errorf("%s no longer contains %q -- a reader greps that phrase to tell which "+
					"check fired, so this test is what keeps the phrase honest", m.name, p)
			}
			if strings.Contains(m.other, p) {
				t.Errorf("%q appears in BOTH failure messages, so grepping it cannot say which "+
					"check fired -- the defect this pair exists to remove (cleat#3075)", p)
			}
		}
	}
}
