// Package hooks tests .githooks, which nothing tested before.
//
// The DCO half of prepare-commit-msg has shipped since 2026-09-04 with
// ShellCheck as its only gate, and ShellCheck does not run a hook -- it reads
// one. What that misses is everything this file is about: whether the trailer
// lands, whether it lands twice, and whether the hook and the check it feeds
// still agree about what a valid stream is.
package hooks

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRoot locates the checkout from the test's own directory rather than from
// a working directory, because `go test ./...` runs each package in its own.
func repoRoot(t *testing.T) string {
	t.Helper()
	// FATAL, NOT SKIP. scripts/check-skips.sh calls this its case (c): a
	// precondition that is always satisfiable in this repo. These tests live
	// inside the checkout they are asking about, so "not a git checkout" means
	// the run is broken, and a skip would report that as a pass.
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("cannot locate the checkout these tests live in: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// runHook invokes the hook against a temporary message file in a throwaway
// repository, and returns the resulting message.
//
// A THROWAWAY REPOSITORY, not this one. The hook reads `git config
// cleat.stream`, so running it here would read the developer's real setting
// and the test would pass or fail according to whose machine it ran on.
func runHook(t *testing.T, stream, message string) (string, error) {
	t.Helper()
	root := repoRoot(t)
	hook := filepath.Join(root, ".githooks", "prepare-commit-msg")

	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.name", "Test Person")
	run("config", "user.email", "test@example.com")
	if stream != "" {
		run("config", "cleat.stream", stream)
	}

	// The hook resolves the check file through `git rev-parse --show-toplevel`,
	// so the throwaway repo gets a copy of the real one. Copying rather than
	// pointing at the original is deliberate: the extraction is part of what is
	// under test, and it must work against a path the hook derives itself.
	checkSrc := filepath.Join(root, ".github", "workflows", "stream-trailer-check.yml")
	body, err := os.ReadFile(checkSrc)
	if err != nil {
		t.Fatalf("reading the stream check: %v", err)
	}
	checkDir := filepath.Join(dir, ".github", "workflows")
	if err := os.MkdirAll(checkDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(checkDir, "stream-trailer-check.yml"), body, 0o644); err != nil {
		t.Fatalf("writing the stream check: %v", err)
	}

	msgFile := filepath.Join(dir, "COMMIT_EDITMSG")
	if err := os.WriteFile(msgFile, []byte(message), 0o644); err != nil {
		t.Fatalf("writing the message: %v", err)
	}

	cmd := exec.Command("sh", hook, msgFile, "message")
	cmd.Dir = dir
	out, runErr := cmd.CombinedOutput()
	got, readErr := os.ReadFile(msgFile)
	if readErr != nil {
		t.Fatalf("reading the message back: %v", readErr)
	}
	if runErr != nil {
		return string(got), &hookError{stderr: string(out), err: runErr}
	}
	return string(got), nil
}

type hookError struct {
	stderr string
	err    error
}

func (e *hookError) Error() string { return e.err.Error() + ": " + e.stderr }

// runCommitMsgHook invokes .githooks/commit-msg directly against a message
// file, mirroring runHook's shape but with commit-msg's own signature: one
// positional argument (the message file), no SOURCE, no repository state to
// seed -- commit-msg's job in this repo reads nothing but the file itself.
func runCommitMsgHook(t *testing.T, message string) (string, error) {
	t.Helper()
	root := repoRoot(t)
	hook := filepath.Join(root, ".githooks", "commit-msg")

	dir := t.TempDir()
	msgFile := filepath.Join(dir, "COMMIT_EDITMSG")
	if err := os.WriteFile(msgFile, []byte(message), 0o644); err != nil {
		t.Fatalf("writing the message: %v", err)
	}

	// commit-msg calls `git interpret-trailers`, which needs to be run inside
	// SOME repository; it does not read repository config the way
	// prepare-commit-msg's stream lookup does, so an empty one is enough.
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")

	cmd := exec.Command("sh", hook, msgFile)
	cmd.Dir = dir
	out, runErr := cmd.CombinedOutput()
	got, readErr := os.ReadFile(msgFile)
	if readErr != nil {
		t.Fatalf("reading the message back: %v", readErr)
	}
	if runErr != nil {
		return string(got), &hookError{stderr: string(out), err: runErr}
	}
	return string(got), nil
}

// The hook stamps both trailers, once each.
func TestTheHookStampsTheStreamAndTheSignoff(t *testing.T) {
	got, err := runHook(t, "cleat-review", "fix: something\n")
	if err != nil {
		t.Fatalf("hook failed: %v", err)
	}
	for _, want := range []string{
		"Signed-off-by: Test Person <test@example.com>",
		"Claude-Stream: cleat-review",
	} {
		if strings.Count(got, want) != 1 {
			t.Errorf("message carries %q %d times, want 1:\n%s", want, strings.Count(got, want), got)
		}
	}
}

// An existing stream trailer is left alone.
//
// The config is a default for commits that do not say, not an override of ones
// that do -- otherwise `git commit --trailer` and an amend of a stamped commit
// would both produce two stamps, and `git interpret-trailers` would have to
// decide which is true.
func TestAnExplicitStreamSurvivesTheHook(t *testing.T) {
	got, err := runHook(t, "cleat-review", "fix: something\n\nClaude-Stream: WS-2\n")
	if err != nil {
		t.Fatalf("hook failed: %v", err)
	}
	if strings.Contains(got, "cleat-review") {
		t.Errorf("the configured stream overrode an explicit one:\n%s", got)
	}
	if strings.Count(got, "Claude-Stream:") != 1 {
		t.Errorf("message carries %d stream trailers, want 1:\n%s",
			strings.Count(got, "Claude-Stream:"), got)
	}
}

// A message that already names its stream commits even with no config set.
//
// This is the case that makes the early exit real rather than redundant.
// `git interpret-trailers --if-exists doNothing` already stops a SECOND
// Claude-Stream line, so removing the guard leaves TestAnExplicitStreamSurvives
// passing -- measured, by removing it. What the guard actually decides is the
// ORDER: it returns before the unset-stream refusal, so a commit whose message
// already answers the question is not refused for failing to answer it. A
// rebase onto a fresh checkout, or `git commit --trailer` on a machine that
// never set cleat.stream, both land here.
func TestAStampedMessageNeedsNoConfiguredStream(t *testing.T) {
	got, err := runHook(t, "", "fix: something\n\nClaude-Stream: WS-2\n")
	if err != nil {
		t.Fatalf("a message that already names its stream was refused: %v", err)
	}
	if !strings.Contains(got, "Claude-Stream: WS-2") {
		t.Errorf("the existing trailer did not survive:\n%s", got)
	}
}

// With no stream configured the hook REFUSES rather than guessing.
//
// This is the case the whole design turns on. A default would not leave the
// commit unattributed, it would attribute it to whichever stream the default
// named -- the failure the check was written after, where two pull requests
// went out as `coordinator` and the coordinator session found a red pull
// request in its own name that it had not opened.
func TestAnUnsetStreamIsRefusedRatherThanGuessed(t *testing.T) {
	got, err := runHook(t, "", "fix: something\n")
	if err == nil {
		t.Fatalf("the hook accepted a commit with no stream configured:\n%s", got)
	}
	he, ok := err.(*hookError)
	if !ok {
		t.Fatalf("unexpected error type: %v", err)
	}
	// The refusal has to carry the fix. A hook that says "no" and not "here is
	// the command" is a worse experience than the CI failure it is preventing.
	for _, want := range []string{"cleat.stream", "git config --local"} {
		if !strings.Contains(he.stderr, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, he.stderr)
		}
	}
	if strings.Contains(got, "Claude-Stream:") {
		t.Errorf("a stream was stamped despite the refusal:\n%s", got)
	}
}

// A stream the check would reject is refused locally, rather than committed and
// discovered in CI.
func TestAStreamTheCheckWouldRejectIsRefusedLocally(t *testing.T) {
	if _, err := runHook(t, "WS-9", "fix: something\n"); err == nil {
		t.Fatal("the hook stamped 'WS-9', which the stream check does not accept")
	}
}

// TestPrepareCommitMsgSkipsStampingOnAMalformedTrailer is half of the
// regression test for cleat#2588: a message whose Claude-Stream line sits in
// an earlier paragraph, separated from the actual trailer block by a blank
// line, is invisible to
// stream-trailer-check.yml's %(trailers:key=Claude-Stream,valueonly) --
// git's parser reads only the message's FINAL paragraph.
//
// prepare-commit-msg must not read this as "already present" (a raw grep
// over the whole file is satisfied by it) and stamp nothing while saying
// nothing either -- that was the original bug. But it must ALSO not refuse
// outright: cleat#2588's second review round found that prepare-commit-msg
// runs before the commit-message editor opens, so a refusal here blocks the
// only paths that could fix it (`commit --amend`, `rebase -i reword`) before
// the author ever gets a chance to edit. So the assertion here is narrower
// than a first read of the issue suggests: exit 0, leave the message
// UNCHANGED (no second trailer stamped alongside the broken one), and warn.
// The actual refusal is commit-msg's job --
// TestCommitMsgRefusesAMalformedTrailer below.
func TestPrepareCommitMsgSkipsStampingOnAMalformedTrailer(t *testing.T) {
	message := "fix: something\n\nClaude-Stream: WS-2\n\nSigned-off-by: Someone <someone@example.com>\n"
	got, err := runHook(t, "cleat-review", message)
	if err != nil {
		t.Fatalf("prepare-commit-msg refused a malformed trailer outright, which "+
			"blocks the only way to repair it (see the hook's own doc comment): %v", err)
	}
	if got != message {
		t.Errorf("prepare-commit-msg changed a message it should have left alone "+
			"for commit-msg to judge:\ngot:  %q\nwant: %q", got, message)
	}
}

// TestCommitMsgRefusesAMalformedTrailer is the other half: commit-msg runs on
// the FINAL message, after any editor has had its chance, so it is where the
// actual refusal belongs. Same shape as the test above, driven at
// .githooks/commit-msg directly.
func TestCommitMsgRefusesAMalformedTrailer(t *testing.T) {
	message := "fix: something\n\nClaude-Stream: WS-2\n\nSigned-off-by: Someone <someone@example.com>\n"
	got, err := runCommitMsgHook(t, message)
	if err == nil {
		t.Fatalf("commit-msg accepted a message where Claude-Stream is split from "+
			"the trailer block by a blank line:\n%s", got)
	}
	he, ok := err.(*hookError)
	if !ok {
		t.Fatalf("unexpected error type: %v", err)
	}
	for _, want := range []string{"final paragraph", "blank line"} {
		if !strings.Contains(he.stderr, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, he.stderr)
		}
	}
}

// TestCommitMsgAcceptsAWellFormedTrailer is commit-msg's negative control: a
// message whose Claude-Stream trailer git's parser DOES read must pass, so
// the malformed-shape check is not accidentally refusing every commit.
func TestCommitMsgAcceptsAWellFormedTrailer(t *testing.T) {
	message := "fix: something\n\nClaude-Stream: WS-2\nSigned-off-by: Someone <someone@example.com>\n"
	if _, err := runCommitMsgHook(t, message); err != nil {
		t.Fatalf("commit-msg refused a well-formed trailer: %v", err)
	}
}

// TestCommitMsgDoesNotDemandPresence is commit-msg's second negative control:
// it is deliberately narrower than "every commit must carry a Claude-Stream
// trailer" -- that presence check belongs to prepare-commit-msg (whose fix,
// an unset git config, has no repair-path problem) and to CI. A message with
// no Claude-Stream line anywhere must not be refused here.
func TestCommitMsgDoesNotDemandPresence(t *testing.T) {
	message := "fix: something\n\nCo-Authored-By: Someone <someone@example.com>\n"
	if _, err := runCommitMsgHook(t, message); err != nil {
		t.Fatalf("commit-msg refused a message with no Claude-Stream trailer at all, "+
			"which is not this hook's job to enforce: %v", err)
	}
}

// TestARepairingAmendSucceeds drives the pair of hooks through a REAL
// `git commit --amend`, not a direct hook invocation -- the class of failure
// cleat#2588's second review round found (prepare-commit-msg refusing before
// the editor runs, blocking the repair) is invisible to a test that invokes
// either hook directly, because the whole point is the ORDER git calls them
// in around an editor. GIT_EDITOR here stands in for the author fixing the
// blank line by hand.
func TestARepairingAmendSucceeds(t *testing.T) {
	root := repoRoot(t)
	dir := t.TempDir()

	run := func(env []string, args ...string) []byte {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return out
	}

	run(nil, "init", "-q")
	run(nil, "config", "user.name", "Test Person")
	run(nil, "config", "user.email", "test@example.com")
	run(nil, "config", "cleat.stream", "WS-2")
	run(nil, "config", "core.hooksPath", filepath.Join(root, ".githooks"))

	checkDir := filepath.Join(dir, ".github", "workflows")
	if err := os.MkdirAll(checkDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	checkBody, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "stream-trailer-check.yml"))
	if err != nil {
		t.Fatalf("reading the stream check: %v", err)
	}
	if err := os.WriteFile(filepath.Join(checkDir, "stream-trailer-check.yml"), checkBody, 0o644); err != nil {
		t.Fatalf("writing the stream check: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("writing f.txt: %v", err)
	}
	run(nil, "add", "f.txt")

	// Seed a malformed HEAD directly -- bypassing hooks is the only way to get
	// one into history at all, since prepare-commit-msg (correctly) will not
	// stamp a second trailer next to this shape and commit-msg would refuse a
	// normal `git commit` outright. That refusal is proven by the tests above;
	// this test is about what happens to a commit that already has the shape,
	// which is the scenario the second review round is actually about.
	malformed := "fix: something\n\nClaude-Stream: WS-2\n\nSigned-off-by: Test Person <test@example.com>\n"
	msgPath := filepath.Join(dir, "malformed-msg.txt")
	if err := os.WriteFile(msgPath, []byte(malformed), 0o644); err != nil {
		t.Fatalf("writing seed message: %v", err)
	}
	run([]string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=/dev/null"},
		"commit", "-q", "-F", msgPath)

	// A no-op editor (P4's shape): the blank line survives, so the amend must
	// still be refused, and HEAD must not move.
	beforeAmend := strings.TrimSpace(string(run(nil, "rev-parse", "HEAD")))
	cmd := exec.Command("git", "commit", "--amend")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_EDITOR=true")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("a no-op amend (leaving the blank line in place) was accepted:\n%s", out)
	}
	if got := strings.TrimSpace(string(run(nil, "rev-parse", "HEAD"))); got != beforeAmend {
		t.Fatalf("HEAD moved despite the refused amend: %s -> %s", beforeAmend, got)
	}

	// A repairing editor: removes the blank line between Claude-Stream and the
	// rest of the trailer block. This must succeed -- it is the exact path
	// cleat#2588's second review round found broken when the refusal lived in
	// prepare-commit-msg.
	editorScript := filepath.Join(dir, "fix-editor.sh")
	editorSrc := "#!/bin/sh\n" +
		"f=\"$1\"\n" +
		"awk 'BEGIN{p=0} /^Claude-Stream:/{print; getline; if ($0==\"\") next; print; next} {print}' " +
		"\"$f\" > \"$f.tmp\" && mv \"$f.tmp\" \"$f\"\n"
	if err := os.WriteFile(editorScript, []byte(editorSrc), 0o755); err != nil {
		t.Fatalf("writing editor script: %v", err)
	}

	cmd = exec.Command("git", "commit", "--amend")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_EDITOR="+editorScript)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("a repairing amend was refused: %v\n%s", err, out)
	}

	final := string(run(nil, "log", "-1", "--format=%B"))
	trailer := string(run(nil, "log", "-1", "--format=%(trailers:key=Claude-Stream,valueonly)"))
	if strings.TrimSpace(trailer) != "WS-2" {
		t.Fatalf("git's own trailer parser does not read the repaired commit's "+
			"stream after amend:\nmessage:\n%s\ntrailer read: %q", final, trailer)
	}
}

// TestAGenuinelyMissingTrailerIsStillStamped is the negative control for the
// test above: proves the new git-parser-based check does not become MORE
// restrictive than the raw grep it replaced. A message with no Claude-Stream
// line anywhere must still be stamped normally, exactly as
// TestTheHookStampsTheStreamAndTheSignoff already covers for the simplest
// case -- this one adds an unrelated trailer-shaped final paragraph, so the
// parser has something to parse before Claude-Stream is added to it.
func TestAGenuinelyMissingTrailerIsStillStamped(t *testing.T) {
	got, err := runHook(t, "WS-3", "fix: something\n\nCo-Authored-By: Someone <someone@example.com>\n")
	if err != nil {
		t.Fatalf("hook failed: %v", err)
	}
	if !strings.Contains(got, "Claude-Stream: WS-3") {
		t.Errorf("a genuinely missing trailer was not stamped:\n%s", got)
	}
}

// The hook and the check agree about the valid set, because the hook reads it
// from the check.
//
// This does not compare two lists -- it asserts there is only one. The failure
// it guards against is a second copy appearing in the hook during some later
// edit, at which point the two drift and every symptom looks like a broken
// hook rather than a stale one.
func TestTheHookReadsTheValidSetFromTheCheck(t *testing.T) {
	root := repoRoot(t)

	hookSrc, err := os.ReadFile(filepath.Join(root, ".githooks", "prepare-commit-msg"))
	if err != nil {
		t.Fatalf("reading the hook: %v", err)
	}
	checkSrc, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "stream-trailer-check.yml"))
	if err != nil {
		t.Fatalf("reading the check: %v", err)
	}

	valid := regexp.MustCompile(`(?m)^\s*VALID='(.*)'\s*$`).FindSubmatch(checkSrc)
	if valid == nil {
		t.Fatal("the check no longer has a VALID='...' line; the hook's extraction " +
			"silently skips validation when it cannot find one, so this is the only " +
			"thing that would notice")
	}

	// Every accepted value, taken from the check, must be accepted by the hook.
	// Running the hook rather than re-reading its source is the point: it tests
	// the extraction, not a second parse of the same line.
	inner := strings.Trim(string(valid[1]), "^$")
	inner = strings.TrimPrefix(inner, "(")
	inner = strings.TrimSuffix(inner, ")")
	streams := strings.Split(inner, "|")
	if len(streams) < 2 {
		t.Fatalf("parsed %d stream(s) from %q; the check's format has changed", len(streams), valid[1])
	}
	for _, s := range streams {
		if _, err := runHook(t, s, "fix: something\n"); err != nil {
			t.Errorf("the check accepts %q but the hook refused it: %v", s, err)
		}
	}

	// And the hook must not carry its own copy of the list. One literal stream
	// name in an error message or comment is fine; the whole alternation is
	// not.
	if regexp.MustCompile(`WS-1\|WS-2\|WS-3`).Match(hookSrc) {
		t.Error("the hook contains its own copy of the valid-stream alternation; " +
			"it should read the set from the check instead, or the two will drift")
	}
}
