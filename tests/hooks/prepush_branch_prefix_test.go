package hooks

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// wantRefused is how every refusal in this file is read, and it asks for what a bare `code != 0` does not (cleat#4479, cleat#4483). The status must be
// exactly 1, which is the hook's own "refused": a hook that died some other way (a crashed `grep`, a bad invocation) also exits non-zero, and a test
// that accepts any non-zero status certifies it. And the message must name the branch, because a refusal that says nothing is not one an author can
// act on, and because it is the only evidence the refusal path (not an earlier exit) produced the status. It reports through t.Errorf and returns
// whether the refusal was as wanted, so a caller can skip its own further checks.
func wantRefused(t *testing.T, label string, code int, stderr, branch string) bool {
	t.Helper()
	ok := true
	if code != 1 {
		t.Errorf("%s: exit status %d, want 1 (the hook's refusal; 0 is an allow and anything else is the hook failing some other way): %q", label, code, stderr)
		ok = false
	}
	// The branch must be on a line of its own, as the hook prints the refused names, and not merely appear somewhere in the message: the hook's fixed "accepts these prefixes: feature/ ..."
	// line contains the string `feature`, so a substring match could not fail for that branch (cleat#4483). And the refused names must come BEFORE the sentence that says what is accepted,
	// or the message reads as though the refused name were an accepted one (the order it had until cleat#4483).
	// The sentence is matched whole, and its ABSENCE is a failure, not a skip: an order check that finds no such sentence has nothing to order, and passing then lets the message's wording
	// change (say, "allows:" for "accepts:") reinstate the very defect this guards (cleat#4483, found in review).
	const acceptsSentence = ".github/workflows/branch-naming.yml accepts these prefixes:"
	line := strings.Index(stderr, "\n  "+branch+"\n")
	accepts := strings.Index(stderr, acceptsSentence)
	switch {
	case line < 0:
		t.Errorf("%s: the refusal does not list the branch %q on a line of its own: %q", label, branch, stderr)
		ok = false
	case accepts < 0:
		t.Errorf("%s: the refusal does not carry the sentence %q, so the order of the refused names and the accepted prefixes cannot be checked: %q", label, acceptsSentence, stderr)
		ok = false
	case accepts < line:
		t.Errorf("%s: the message says what is accepted before it lists the refused branch %q, so the name reads as an accepted one: %q", label, branch, stderr)
		ok = false
	}
	return ok
}

func TestThePrePushHookRefusesABranchOutsideTheSix(t *testing.T) {
	for _, tc := range []struct{ name, branch string }{
		{"an arbitrary prefix", "test/y"},
		{"chore, the #2314 mistake", "chore/x"},
		{"feat, which is not feature", "feat/x"},
		{"the bare word, with no slash", "feature"},
	} {
		code, stderr := runPrePush(t, refLine("refs/heads/"+tc.branch, realSHA, "refs/heads/"+tc.branch, zeroSHA))
		if !wantRefused(t, tc.name, code, stderr, tc.branch) {
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
		// The three permanent branches `Validate branch name` exempts. The hook
		// shipped refusing all three, which is the drift the tie test above and
		// this list exist for: they are not work branches and have no prefix to
		// choose.
		{"develop", refLine("refs/heads/develop", realSHA, "refs/heads/develop", zeroSHA)},
		{"main", refLine("refs/heads/main", realSHA, "refs/heads/main", zeroSHA)},
		{"master", refLine("refs/heads/master", realSHA, "refs/heads/master", zeroSHA)},
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
	wantRefused(t, "a bad remote ref from a well-named local branch", code, stderr, "test/y")
}

// TestThePrePushHookReadsTheRuleFromTheCheck is the tie this hook shipped
// without, and the reason is concrete: it carried its own copy of the six, and
// the drift was there on day one -- `Validate branch name` exempts
// main/master/develop and the copy refused all three.
//
// The extraction is tested by RUNNING the hook against every value the check
// declares, not by re-parsing the workflow in the test. A second parse here
// would pass against a hook with a hardcoded list, which is the whole defect.
func TestThePrePushHookReadsTheRuleFromTheCheck(t *testing.T) {
	root := repoRoot(t)
	hookSrc, err := os.ReadFile(filepath.Join(root, ".githooks", "pre-push"))
	if err != nil {
		t.Fatalf("reading the hook: %v", err)
	}
	checkPath := filepath.Join(root, ".github", "workflows", "branch-naming.yml")
	checkSrc, err := os.ReadFile(checkPath)
	if err != nil {
		t.Fatalf("reading the check: %v", err)
	}

	// The hook has to NAME the check it reads, and the test has to ask about
	// CODE rather than about the file. A plain substring search is satisfied by
	// this hook's own comments -- they name the workflow, the file, and why the
	// rule is not repeated here -- so a version that deleted the extraction and
	// pasted the list back in would pass it. Comments are stripped first.
	code := regexp.MustCompile(`(?m)^\s*#.*$`).ReplaceAllString(string(hookSrc), "")
	for _, want := range []string{"branch-naming.yml", "validPrefixes"} {
		if !strings.Contains(code, want) {
			t.Errorf("the hook's code does not mention %q; if it no longer reads the rule "+
				"from the check, it is carrying a copy again", want)
		}
	}

	prefixes := regexp.MustCompile(`const validPrefixes\s*=\s*\[(.*?)\]`).FindSubmatch(checkSrc)
	if prefixes == nil {
		t.Fatal("the check no longer declares `const validPrefixes = [...]`. The hook " +
			"fails open when it cannot extract one, so nothing else would notice")
	}
	permLine := regexp.MustCompile(`(?m)^.*branch === .*$`).FindString(string(checkSrc))
	if permLine == "" {
		t.Fatal("the check no longer declares its permanent-branch exemptions; the hook " +
			"fails open when it cannot extract them")
	}
	names := regexp.MustCompile(`'([a-z]+/)'`).FindAllStringSubmatch(string(prefixes[1]), -1)
	if len(names) < 2 {
		t.Fatalf("parsed %d prefix(es) from the check; its format has changed", len(names))
	}

	for _, m := range names {
		b := m[1] + "example"
		if code, stderr := runPrePush(t, refLine("refs/heads/"+b, realSHA, "refs/heads/"+b, zeroSHA)); code != 0 {
			t.Errorf("the check accepts the %q prefix but the hook refused it: %s", m[1], stderr)
		}
	}
	for _, m := range regexp.MustCompile(`'([a-z]+)'`).FindAllStringSubmatch(permLine, -1) {
		if code, stderr := runPrePush(t, refLine("refs/heads/"+m[1], realSHA, "refs/heads/"+m[1], zeroSHA)); code != 0 {
			t.Errorf("the check exempts %q but the hook refused it: %s", m[1], stderr)
		}
	}
}

// TestThePrePushHookFailsOpenWhenItCannotReadTheCheck is the allow-by-default
// half of the same arrangement. A checkout without .github, or a workflow whose
// shape has moved, must not refuse every push on the machine -- CI still
// catches a bad name, and this hook is a convenience.
func TestThePrePushHookFailsOpenWhenItCannotReadTheCheck(t *testing.T) {
	// Run the hook from a directory that is not a checkout, so
	// `git rev-parse --show-toplevel` yields nothing and the rule cannot resolve.
	hook := filepath.Join(repoRoot(t), ".githooks", "pre-push")
	cmd := exec.Command("sh", hook)
	cmd.Dir = t.TempDir()
	cmd.Stdin = strings.NewReader(refLine("refs/heads/test/y", realSHA, "refs/heads/test/y", zeroSHA))
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("the hook refused a push it could not classify (exit %v): %s", err, stderr.String())
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
	wantRefused(t, "a push carrying one good ref and one bad", code, stderr, "test/z")
	if strings.Contains(stderr, "feature/x") {
		t.Errorf("the refusal blames the GOOD ref: %q", stderr)
	}
}

// TestThePrePushHookAllowsABotsBranchByPrefix is cleat#4471. `Validate branch
// name` exempts a PR by its AUTHOR (`dependabot[bot]`); the hook cannot see the
// author, only the name, and dependabot's names are fixed. A maintainer pushing
// the dashboard bundle rebuild onto a dependabot PR's branch was refused a name
// nobody can change, while the check passed the same PR.
func TestThePrePushHookAllowsABotsBranchByPrefix(t *testing.T) {
	for _, name := range []string{
		"dependabot/npm_and_yarn/web/web-toolchain-dadb0bc5ca", // the name that was refused
		"dependabot/go_modules/go-minor-and-patch-1a2b3c",
		"dependabot/github_actions/actions/checkout-9",
		"dependabot/cargo/crates/serde-1.0.200",
	} {
		if code, stderr := runPrePush(t, refLine("refs/heads/"+name, realSHA, "refs/heads/"+name, zeroSHA)); code != 0 {
			t.Errorf("%s: refused (exit %d); the check exempts dependabot's PRs: %s", name, code, stderr)
		}
	}
}

// TestThePrePushHookExemptsOnlyAPrefixAndNothingElse is the other half, and the
// half a "contains dependabot" implementation fails: the exemption is for the
// names a bot gives its branches, and a person's branch that happens to mention
// the word still needs a prefix, as does every ordinary bad name.
func TestThePrePushHookExemptsOnlyAPrefixAndNothingElse(t *testing.T) {
	for _, name := range []string{
		"feat/x",                  // an ordinary bad name is still refused
		"chore/x",                 // so is the #2314 one
		"dependabot",              // the bare word, no slash: not a bot's name
		"dependabot/",             // the prefix with nothing after it
		"dependabotx/y",           // a longer first segment
		"x/dependabot/y",          // the word in the middle
		"my-dependabot/npm",       // the word as a suffix of the first segment
		"Dependabot/npm_and_yarn", // the exemption is case-exact, as the check's is
	} {
		code, stderr := runPrePush(t, refLine("refs/heads/"+name, realSHA, "refs/heads/"+name, zeroSHA))
		wantRefused(t, name, code, stderr, name)
	}
}

// TestThePrePushHookStopsExemptingABotWhenTheCheckDoes ties the exemption to the
// check instead of repeating it, the arrangement the rest of this hook exists
// for. It runs the hook in a scratch repository whose branch-naming.yml is the
// real one with the bot line taken out, and a dependabot/ branch is then refused
// like any other. A hook that hardcoded `dependabot/` would still allow it.
func TestThePrePushHookStopsExemptingABotWhenTheCheckDoes(t *testing.T) {
	root := repoRoot(t)
	check, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "branch-naming.yml"))
	if err != nil {
		t.Fatalf("reading the check: %v", err)
	}
	botLine := regexp.MustCompile(`(?m)^.*author === '[a-z]+\[bot\]'.*$`)
	if !botLine.Match(check) {
		t.Fatal("the check no longer exempts a bot by `author === '<name>[bot]'`; the hook derives its " +
			"bot prefix from that line, so it exempts nothing now. Update the hook and this test together")
	}
	without := botLine.ReplaceAll(check, []byte("            if (labels.includes('bot')) {"))

	run := func(workflow []byte) (int, string) {
		dir := t.TempDir()
		if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("git init: %v: %s", err, out)
		}
		wf := filepath.Join(dir, ".github", "workflows")
		if err := os.MkdirAll(wf, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wf, "branch-naming.yml"), workflow, 0o644); err != nil {
			t.Fatal(err)
		}
		name := "dependabot/npm_and_yarn/web/x"
		cmd := exec.Command("sh", filepath.Join(root, ".githooks", "pre-push"))
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader(refLine("refs/heads/"+name, realSHA, "refs/heads/"+name, zeroSHA))
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

	// The control: the unmodified check, in the same scratch repository, allows
	// the name. Without it the refusal below could be the scratch setup failing.
	if code, stderr := run(check); code != 0 {
		t.Fatalf("the real check, copied into a scratch repository, did not exempt a dependabot branch (exit %d): %s", code, stderr)
	}
	// The refusal is read as every other one in this file is: exit 1 and the branch named (cleat#4479). Status alone passed a hook that exited 2.
	code, stderr := run(without)
	if code == 0 {
		t.Fatal("the check no longer exempts dependabot and the hook still does: it is carrying its own copy of the exemption")
	}
	wantRefused(t, "a dependabot branch once the check stops exempting bots", code, stderr, "dependabot/npm_and_yarn/web/x")
}
