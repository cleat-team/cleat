// A `cleat build --target java` whose Gradle WRAPPER could not fetch its
// distribution is an ENVIRONMENT failure, and every test in this package that
// runs that build reported it as a failure of whatever the test was about.
//
// cleat#3039. Measured once, in Tier 1 Gate -- rest shard 3/3, on a tree that
// did not touch Java at all:
//
//	Downloading https://services.gradle.org/distributions/gradle-7.6.4-bin.zip
//	Exception in thread "main" java.io.IOException: Server returned HTTP response code: 502
//	  for URL: https://github.com/gradle/gradle-distributions/releases/download/v7.6.4/gradle-7.6.4-bin.zip
//	    at org.gradle.wrapper.Download.downloadInternal(Download.java:109)
//	Error: gradle build failed: exit status 1
//
// TestJavaBuildRefusesAStaleManifest asserts that the build failed AND that the
// reason was "no entry-point manifest" -- so a 502 fetching Gradle, which is
// not a claim about manifests at all, was reported as this test failing. The
// same shape reaches every other caller, including the one that asserts the
// build SUCCEEDS.
//
// IT IS DELIBERATELY NOT A SKIP, and the first draft of this file was one --
// `t.Skipf("... the wrapper could not fetch its distribution")`. Two rules in
// this repo refuse that, and both were READ rather than assumed:
//
//   - scripts/check-skips.sh allows a skip only for a resource that is
//     "genuinely optional and nobody asked for it" (its criterion (a)). A build
//     that REQUIRES the Gradle distribution does not qualify, and the guard's
//     own text names this shape as the one that "must be t.Fatalf naming the
//     redacted config, not a skip" (its criterion (b)).
//   - CLAUDE.md defines a legitimate skip as "a toolchain that is not
//     installed, a DSN that is not set" -- a precondition that is ABSENT. Here
//     the toolchain is present and the download failed, which is an environment
//     failure at RUN time, not an absent precondition.
//
// The skip would also have had a cost that outlives the incident: a 502 that is
// not transient -- a host that cannot reach services.gradle.org at all -- would
// make this a PERMANENT SILENT SKIP, which is the failure class both rules
// exist to prevent. A loud failure that names the resource keeps it visible.
//
// So the red stays, and stops wearing the wrong clothing. What the issue
// actually reports is the MISATTRIBUTION -- a 502 "reported as this test
// failing, which reads on the PR as a red for the change under test" -- and a
// failure that says which environment resource failed answers exactly that.
package main

import (
	"bytes"
	"os/exec"
	"testing"
)

// gradleWrapperCouldNotFetch is the discriminator. It matches the wrapper
// PACKAGE rather than one class, and the width is the point.
//
// The first draft matched `org.gradle.wrapper.Download` -- the class named in
// the incident's stack trace -- on the reasoning that a narrower match is the
// safer error direction. That reasoning was wrong for THIS branch, and
// cleat-review's reading is what showed it: every failure the wrapper can have
// is raised from `org.gradle.wrapper.*` -- the fetch, its checksum, DNS, TLS, a
// timeout, a 5xx -- so matching one class leaves this exact defect in place for
// all the rest, to be rediscovered as a new bug the next time one of them fires.
// A false NEGATIVE here is the original defect; a false positive only makes a
// message less precise.
//
// It stays ORDERING-SAFE at this width. Those frames appear only inside a stack
// trace, and the wrapper package runs BEFORE Gradle itself starts -- so a build
// that got far enough to reach the manifest assertions cannot also carry one.
// Widening within the package does not weaken that: nothing in the package
// executes after a task.
//
// The nearest over-match to guard against is a class name in a MESSAGE rather
// than a frame. `Downloading https://services.gradle.org/...` and `Welcome to
// Gradle 7.6.4!` are what a SUCCESSFUL fetch prints, and neither contains
// `org.gradle.wrapper.` -- pinned as a negative control below, because that is
// the case a wider pattern is most likely to swallow.
func gradleWrapperCouldNotFetch(out []byte) bool {
	return bytes.Contains(out, []byte("org.gradle.wrapper."))
}

// javaBuild runs `cleat build --target java` and returns its combined output and
// error. When the Gradle wrapper could not fetch its distribution it fails with
// a message naming THAT, so the caller cannot report an environment failure as
// a result about the artifact (cleat#3039).
//
// Every caller goes through here so the diagnosis cannot drift between them: a
// fix applied to the one test that happened to fail in CI would leave the three
// siblings reporting the same environment failure as their own.
func javaBuild(t *testing.T, repoRoot, javaDir, outDir string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command(cleatBinary, "build", "--target", "java", "-o", outDir, javaDir)
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if gradleWrapperCouldNotFetch(out) {
		t.Fatalf("the Gradle wrapper could not fetch its distribution, so this build never reached "+
			"the behaviour under test. That is an ENVIRONMENT failure and says nothing about the "+
			"artifact -- but it is deliberately a FAILURE and not a skip, because a distribution "+
			"that stays unreachable would become a permanent silent skip. Re-run; if it reproduces, "+
			"this host could not reach services.gradle.org. cleat#3039.\n%s", out)
	}
	return out, err
}

// TestGradleWrapperCouldNotFetchRecognisesTheFailureAndNothingElse carries a
// KNOWN POSITIVE: the verbatim output from the run that filed cleat#3039. A
// predicate that never fires passes over every tree, including the one that
// produced this text, so the positive is what makes the negative mean anything.
func TestGradleWrapperCouldNotFetchRecognisesTheFailureAndNothingElse(t *testing.T) {
	// KNOWN POSITIVE -- copied from shard 3/3's artifact for run 37156068689.
	// Trimmed to the lines that matter; the elided lines are further frames of
	// the same stack, which is the file's own case for matching the class name
	// rather than a specific HTTP code.
	const wrapperFailed = `Vetting Java project in /tmp/TestJavaBuildRefusesAStaleManifest3622390906/001/examples/java-workflow...
  Summary: 1 files, 0 errors, 0 warnings
  Compiling Java to WASM via TeaVM...
Downloading https://services.gradle.org/distributions/gradle-7.6.4-bin.zip
Exception in thread "main" java.io.IOException: Server returned HTTP response code: 502
  for URL: https://github.com/gradle/gradle-distributions/releases/download/v7.6.4/gradle-7.6.4-bin.zip
	at org.gradle.wrapper.Download.downloadInternal(Download.java:109)
	at org.gradle.wrapper.Download.download(Download.java:80)
Error: gradle build failed: exit status 1
`

	// NEGATIVE CONTROL -- the refusals the tests are actually about. Each must
	// NOT be classified as an environment failure, or the skip would dissolve
	// the assertion it sits beside.
	for _, tc := range []struct {
		name string
		out  string
	}{
		{
			name: "the staleness refusal (TestJavaBuildRefusesAStaleManifest)",
			out: "Error: build failed: no entry-point manifest was written by the Java build " +
				"and none was found under build/ -- refusing to embed a possibly stale entry point list",
		},
		{
			name: "the two-manifest refusal (TestJavaBuildRefusesMultipleManifests)",
			out:  "Error: expected exactly one entry-point manifest under build/, found 2 entry-point manifests",
		},
		{
			name: "a successful build that merely mentions the distribution URL",
			out: "Downloading https://services.gradle.org/distributions/gradle-7.6.4-bin.zip\n" +
				"Welcome to Gradle 7.6.4!\nBUILD SUCCESSFUL in 12s",
		},
		{
			name: "empty output (the build did not run at all)",
			out:  "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if gradleWrapperCouldNotFetch([]byte(tc.out)) {
				t.Errorf("classified as a Gradle-wrapper fetch failure, but this is not one.\n\n"+
					"A skip here would dissolve the assertion this test exists for: the failure is "+
					"about the artifact, and reporting it as an environment precondition is the "+
					"inverse of cleat#3039.\noutput:\n%s", tc.out)
			}
		})
	}

	// KNOWN POSITIVE 1 -- the failing run's verbatim output.
	if !gradleWrapperCouldNotFetch([]byte(wrapperFailed)) {
		t.Error("the verbatim output from the run that filed cleat#3039 was NOT recognised as a " +
			"Gradle-wrapper failure.\n\n" +
			"This is the known positive: without it the negative controls above pass on a predicate " +
			"that can never be true, which is a green measuring nothing.")
	}

	// KNOWN POSITIVE 2 -- SYNTHETIC, and labelled as such because it is: this
	// text was CONSTRUCTED from the wrapper's exception shape, not copied from a
	// run, so it says nothing about any real incident. It exists for exactly one
	// reason -- to pin the WIDTH. The predicate matched
	// `org.gradle.wrapper.Download` in an earlier draft; against that version
	// this case is NOT recognised, so this assertion fails if the match is ever
	// narrowed back to a single class.
	const wrapperChecksumFailed = `Downloading https://services.gradle.org/distributions/gradle-7.6.4-bin.zip
Exception in thread "main" java.lang.RuntimeException: Could not verify the checksum of gradle-7.6.4-bin.zip
	at org.gradle.wrapper.Install.forceFetch(Install.java:120)
	at org.gradle.wrapper.Install.access$200(Install.java:37)
Error: gradle build failed: exit status 1
`
	if !gradleWrapperCouldNotFetch([]byte(wrapperChecksumFailed)) {
		t.Error("a wrapper failure raised outside `Download` was NOT recognised.\n\n" +
			"SYNTHETIC case (see the comment above), so this asserts nothing about a real run -- " +
			"but it does assert that the predicate is matching the wrapper PACKAGE rather than one " +
			"class. Narrowed back to `Download`, every other wrapper failure -- a checksum, DNS, TLS, " +
			"a timeout -- reports as a failure of the test under it, which is cleat#3039 still in place.")
	}
}
