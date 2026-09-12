package main

// Two more surfaces a document can prescribe and the binary can lack, in the
// shape of documented_cli_surface_test.go: worker flags, and analyzer
// diagnostic codes. cleat#1311.
//
// The authority is the CODE in both cases -- `flag.*` calls in
// cmd/cleat-worker, and the code literals in internal/closure. Not a help
// string, and not docs/reference/error-codes.md: checking a document against
// another document is one derivation run twice, which is how
// `docs/troubleshooting.md` and `docs/workflow-go-constraints.md` agreed with
// each other for months that map iteration produces a W001 *warning* while the
// analyzer emitted E021 as an *error* that fails the build.
//
// That severity inversion is the reason this is not cosmetic. A wrong code is
// a lookup failure. A warning-where-there-is-an-error tells a reader the
// toolchain will tolerate what they wrote, and gives them a reason not to
// check.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// flagBaseline and codeBaseline record documented-but-absent items that are not
// being fixed here, each with a reason. Both may only shrink, and an entry that
// matches nothing fails -- a grant must not outlive its defect.
var flagBaseline = map[string]string{
	// A DELIBERATE PLACEHOLDER. The surrounding prose is about what happens when
	// a binary and its configuration disagree, and the block is labelled
	// "Bad: mismatched binary and config" -- the flag is fictional on purpose,
	// which is the one case where documenting a nonexistent flag is correct.
	"docs/operations/upgrading.md|--new-flag": "illustrative placeholder in a flag-compatibility example",

	// Real defects with no knowable replacement. `cleat-worker` has no
	// namespace concept at all (the nearest is tenants: --create-tenant,
	// --tenant-resolver), and the line above it passes --namespace to
	// `cleat deploy`, which has only name/task-queue/default/max-history-length.
	// Rewriting a production-deployment example on a guess would replace a
	// visible error with an invisible one.
	"docs/operations/deploying-to-production.md|--namespace": "no namespace concept on the worker; cleat#1311",
}

var codeBaseline = map[string]string{
	// The ONLY occurrence is the sentence recording that it is unassigned, so
	// the document is correct and the scan is reading prose about an absence as
	// a claim of presence. Kept as a baseline entry rather than special-cased
	// on wording, because the next such note will be phrased differently.
	"docs/reference/error-codes.md|E019": "the line says `E019 is not currently assigned` -- a sentence about the absence",
}

func repoRootFor(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func trackedFiles(t *testing.T, root, pattern string) []string {
	t.Helper()
	cmd := exec.Command("git", "ls-files", pattern)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files %s: %v", pattern, err)
	}
	return strings.Fields(string(out))
}

var workerFlagDef = regexp.MustCompile(`flag\.(?:String|Bool|Int|Int64|Uint|Uint64|Duration|Float64)\("([a-zA-Z0-9._-]+)"`)

func workerFlags(t *testing.T, root string) map[string]bool {
	t.Helper()
	flags := map[string]bool{}
	for _, f := range trackedFiles(t, root, "cmd/cleat-worker/*.go") {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range workerFlagDef.FindAllSubmatch(src, -1) {
			flags[string(m[1])] = true
		}
	}
	// A surface that came back empty would fail every document rather than
	// pass them, which is the loud direction -- but say so plainly.
	if len(flags) < 20 {
		t.Fatalf("found %d cleat-worker flags; the extractor no longer matches how they "+
			"are declared, and every finding below would be noise", len(flags))
	}
	return flags
}

// workerInvocation matches a cleat-worker command line inside a fenced block,
// including the shell line-continuations these runbooks use.
var (
	fenced         = regexp.MustCompile("(?s)```[^\n]*\n(.*?)```")
	workerCmdLine  = regexp.MustCompile(`(?m)^\s*(?:\$\s*)?cleat-worker\b`)
	longFlag       = regexp.MustCompile(`--([a-zA-Z0-9._-]+)`)
	continuedLines = regexp.MustCompile(`\\\s*\n\s*`)
)

func TestEveryDocumentedWorkerFlagExists(t *testing.T) {
	root := repoRootFor(t)
	flags := workerFlags(t, root)
	docs := trackedFiles(t, root, "*.md")
	if len(docs) < 50 {
		t.Fatalf("git ls-files matched %d markdown files; the scan did not see the repo", len(docs))
	}

	seen := map[string]bool{}
	for _, doc := range docs {
		body, err := os.ReadFile(filepath.Join(root, doc))
		if err != nil {
			t.Fatalf("read %s: %v", doc, err)
		}
		for _, block := range fenced.FindAllStringSubmatch(string(body), -1) {
			// JOIN CONTINUATIONS FIRST. These runbooks write one flag per line
			// with a trailing backslash, so a line-oriented read sees the flags
			// but not the `cleat-worker` that owns them -- the defect at
			// disaster-recovery.md:819 is spelled exactly that way.
			text := continuedLines.ReplaceAllString(block[1], " ")
			for _, line := range strings.Split(text, "\n") {
				if !workerCmdLine.MatchString(line) {
					continue
				}
				for _, m := range longFlag.FindAllStringSubmatch(line, -1) {
					name := m[1]
					if flags[name] {
						continue
					}
					key := doc + "|--" + name
					seen[key] = true
					if _, parked := flagBaseline[key]; parked {
						continue
					}
					t.Errorf("%s documents `cleat-worker --%s`, which the worker does not "+
						"define.\n\nA reader runs this literally and the worker exits on an "+
						"unknown flag (cleat#1311).", doc, name)
				}
			}
		}
	}
	for key, why := range flagBaseline {
		if !seen[key] {
			t.Errorf("flagBaseline has %q (%s) but nothing matches it; delete the entry.", key, why)
		}
	}
}

// emittedCode matches both forms internal/closure uses. The struct-literal form
// alone reported TWELVE codes as absent that are emitted by the tuple form --
// the strict parse was wrong and a deliberately looser second reading caught
// it, which is why both are spelled out here rather than one being trusted.
var emittedCode = regexp.MustCompile(`(?:Code:\s*"([EW]\d{3})"|=\s*"([EW]\d{3})"\s*,)`)

// citedCode deliberately has no word boundaries: `git grep -E` does not support
// \b and matches NOTHING with it, which reads as a clean scan.
var citedCode = regexp.MustCompile(`(?:^|[^A-Za-z0-9])([EW]\d{3})(?:[^0-9]|$)`)

func TestEveryDocumentedDiagnosticCodeIsEmitted(t *testing.T) {
	root := repoRootFor(t)

	emitted := map[string]bool{}
	for _, f := range trackedFiles(t, root, "*.go") {
		if strings.HasSuffix(f, "_test.go") || strings.Contains(f, "/testdata/") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			continue
		}
		for _, m := range emittedCode.FindAllStringSubmatch(string(src), -1) {
			if m[1] != "" {
				emitted[m[1]] = true
			} else {
				emitted[m[2]] = true
			}
		}
	}
	if len(emitted) < 10 {
		t.Fatalf("found %d diagnostic codes in the analyzer; the extractor no longer "+
			"matches how they are written, and every finding below would be noise", len(emitted))
	}

	seen := map[string]bool{}
	for _, doc := range trackedFiles(t, root, "docs/*.md") {
		checkDocCodes(t, root, doc, emitted, seen)
	}
	for _, doc := range trackedFiles(t, root, "docs/**/*.md") {
		checkDocCodes(t, root, doc, emitted, seen)
	}
	for key, why := range codeBaseline {
		if !seen[key] {
			t.Errorf("codeBaseline has %q (%s) but nothing matches it; delete the entry.", key, why)
		}
	}
}

func checkDocCodes(t *testing.T, root, doc string, emitted, seen map[string]bool) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, doc))
	if err != nil {
		t.Fatalf("read %s: %v", doc, err)
	}
	reported := map[string]bool{}
	for _, m := range citedCode.FindAllStringSubmatch(string(body), -1) {
		code := m[1]
		if emitted[code] || reported[code] {
			continue
		}
		reported[code] = true
		key := doc + "|" + code
		seen[key] = true
		if _, parked := codeBaseline[key]; parked {
			continue
		}
		t.Errorf("%s cites diagnostic %s, which the analyzer never emits.\n\n"+
			"A reader looks it up, finds nothing, and cannot tell whether the check "+
			"was removed or they misread the output (cleat#1311).", doc, code)
	}
}
