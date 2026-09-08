package main

// Version helpers for TestVetPython's skip, kept in a _test.go file because
// they have no production caller.
//
// The test-only-code guard caught them in the production file and was right to:
// pythonVersionOnPath and pythonSDKMinVersion are used by the failure hint that
// `cleat vet` prints, but nothing outside a test ever compares versions. A
// baseline entry would have been the easier answer and the wrong one -- it
// records an exception where there is no exception to make.

import (
	"fmt"
)

// pythonAtLeast reports the interpreter's version and whether it is at least
// min (a "major.minor" string).
//
// Returns false when the version cannot be determined, so a caller gating on it
// treats "cannot tell" the same as "too old". That is the safe direction here:
// the alternative is running a test that will fail for an environmental reason
// and reporting it as a defect.
func pythonAtLeast(min string) (string, bool) {
	version := pythonVersionOnPath()
	var haveMajor, haveMinor, wantMajor, wantMinor int
	if _, err := fmt.Sscanf(version, "%d.%d", &haveMajor, &haveMinor); err != nil {
		return version, false
	}
	if _, err := fmt.Sscanf(min, "%d.%d", &wantMajor, &wantMinor); err != nil {
		return version, false
	}
	return version, versionAtLeast(haveMajor, haveMinor, wantMajor, wantMinor)
}

// versionAtLeast is the comparison on its own, so the boundaries can be tested
// without an interpreter to arrange.
//
// Numeric per component rather than a string compare: "3.9" > "3.10"
// lexicographically, which is the classic way this check is got wrong and would
// disable the caller's test on every Python from 3.10 onwards.
func versionAtLeast(haveMajor, haveMinor, wantMajor, wantMinor int) bool {
	if haveMajor != wantMajor {
		return haveMajor > wantMajor
	}
	return haveMinor >= wantMinor
}
