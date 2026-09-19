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
