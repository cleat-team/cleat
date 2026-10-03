package hooks

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	zeroSHA = "0000000000000000000000000000000000000000"
	realSHA = "1111111111111111111111111111111111111111"
)

// runPrePush feeds pre-push's stdin the ref lines git would hand it, and returns
// its exit status and stderr.
//
// STDIN, NOT A CHECKOUT, and that is the whole point. pre-push receives the refs
// ACTUALLY BEING PUSHED; `git branch --show-current` names the checked-out
// branch, which is a different object. A test that drives this hook by checking
// out a branch therefore drives the one path the hook deliberately does not use,
// and would pass against an implementation that reads the wrong object — the
// fail-open bug the stdin reading exists to avoid (cleat#3006).
func runPrePush(t *testing.T, stdin string) (int, string) {
	t.Helper()
	hook := filepath.Join(repoRoot(t), ".githooks", "pre-push")
	cmd := exec.Command("sh", hook)
	cmd.Stdin = strings.NewReader(stdin)
	var stderr strings.Builder
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		return 0, stderr.String()
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), stderr.String()
	}
	t.Fatalf("running the hook: %v", err)
	return -1, ""
}

// refLine formats one pre-push stdin record.
func refLine(localRef, localSHA, remoteRef, remoteSHA string) string {
	return localRef + " " + localSHA + " " + remoteRef + " " + remoteSHA + "\n"
}

func TestThePrePushHookRefusesABranchOutsideTheSix(t *testing.T) {
	for _, tc := range []struct{ name, stdin string }{
		{"an arbitrary prefix", refLine("refs/heads/test/y", realSHA, "refs/heads/test/y", zeroSHA)},
		{"chore, the #2314 mistake", refLine("refs/heads/chore/x", realSHA, "refs/heads/chore/x", zeroSHA)},
		{"feat, which is not feature", refLine("refs/heads/feat/x", realSHA, "refs/heads/feat/x", zeroSHA)},
		{"the bare word, with no slash", refLine("refs/heads/feature", realSHA, "refs/heads/feature", zeroSHA)},
	} {
		code, stderr := runPrePush(t, tc.stdin)
		if code == 0 {
			t.Errorf("%s: allowed; want refused", tc.name)
			continue
		}
		// The refusal has to name the six, or the message sends the author to
		// CLAUDE.md to find out what it wanted.
		if !strings.Contains(stderr, "feature/") || !strings.Contains(stderr, "hotfix/") {
			t.Errorf("%s: refusal does not name the six prefixes: %q", tc.name, stderr)
		}
	}
}

// TestThePrePushHookAllowsTheSixAndEverythingItCannotClassify covers the
// allow-by-default half. This hook is SHARED — core.hooksPath points every
// checkout on the machine at it — so a refusal it should not have made blocks
// every stream's push, which is why anything unrecognised is left alone.
func TestThePrePushHookAllowsTheSixAndEverythingItCannotClassify(t *testing.T) {
	for _, tc := range []struct{ name, stdin string }{
		{"feature/", refLine("refs/heads/feature/x", realSHA, "refs/heads/feature/x", zeroSHA)},
		{"bugfix/", refLine("refs/heads/bugfix/x", realSHA, "refs/heads/bugfix/x", zeroSHA)},
		{"fix/", refLine("refs/heads/fix/x", realSHA, "refs/heads/fix/x", zeroSHA)},
		{"docs/", refLine("refs/heads/docs/x", realSHA, "refs/heads/docs/x", zeroSHA)},
		{"release/", refLine("refs/heads/release/x", realSHA, "refs/heads/release/x", zeroSHA)},
		{"hotfix/", refLine("refs/heads/hotfix/x", realSHA, "refs/heads/hotfix/x", zeroSHA)},
		// DELETING a badly named branch must be allowed. Refusing it would
		// enforce the prefix on the attempt to undo the mistake, which is the
		// opposite of the point.
		{"deleting a bad branch", refLine("(delete)", zeroSHA, "refs/heads/test/y", realSHA)},
		{"a tag, which is not a branch", refLine("refs/tags/v0.4.0", realSHA, "refs/tags/v0.4.0", zeroSHA)},
		{"no refs at all", ""},
	} {
		if code, stderr := runPrePush(t, tc.stdin); code != 0 {
			t.Errorf("%s: refused (exit %d): %s", tc.name, code, stderr)
		}
	}
}

// TestThePrePushHookReadsTheREMOTERef is the reason the hook reads stdin at all,
// and the case a `--show-current` implementation cannot see: a WELL-named local
// branch pushed to a BADLY-named remote branch.
func TestThePrePushHookReadsTheREMOTERef(t *testing.T) {
	code, stderr := runPrePush(t, refLine("refs/heads/feature/x", realSHA, "refs/heads/test/y", zeroSHA))
	if code == 0 {
		t.Fatal("a bad REMOTE ref was allowed because the LOCAL ref was well named: the hook is reading the wrong object")
	}
	if !strings.Contains(stderr, "test/y") {
		t.Errorf("the refusal does not name the offending ref: %q", stderr)
	}
}

// TestThePrePushHookRefusesOnlyTheOffendingRef: a push carrying one good ref and
// one bad one must refuse, and must not blame the good one.
func TestThePrePushHookRefusesOnlyTheOffendingRef(t *testing.T) {
	stdin := refLine("refs/heads/feature/x", realSHA, "refs/heads/feature/x", zeroSHA) +
		refLine("refs/heads/test/z", realSHA, "refs/heads/test/z", zeroSHA)

	code, stderr := runPrePush(t, stdin)
	if code == 0 {
		t.Fatal("a push carrying a bad ref was allowed")
	}
	if !strings.Contains(stderr, "test/z") {
		t.Errorf("the refusal does not name the bad ref: %q", stderr)
	}
	if strings.Contains(stderr, "feature/x") {
		t.Errorf("the refusal blames the GOOD ref: %q", stderr)
	}
}
