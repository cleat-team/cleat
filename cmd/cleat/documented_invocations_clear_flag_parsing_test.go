package main

// cleat#1970. `cleat deploy --db <conn>` exited 2 with "flag provided but not
// defined: -db" from every doc that showed it, because --db is registered on
// the top-level FlagSet in main() -- before `switch command` -- and deploy's
// own FlagSet never declared it. Ten tracked markdown files carried the
// broken form. Fixed by giving `deploy` its own --db (mirroring `cleat
// lock`'s runLock, cmd/cleat/main.go), falling back to the global flag and
// CLEAT_DATABASE_URL. Two of those ten docs also documented a `--namespace`
// flag that has never existed on `deploy` at all (docs/reference/cli.md
// already carried a correction note saying so, dated 2026-08-09 -- the other
// two docs were never brought in line with it).
//
// documented_cli_surface_test.go already checks that every documented
// SUBCOMMAND exists. It does not run anything, so it cannot see a documented
// invocation whose subcommand is real but whose FLAGS are not --
// TestEveryTemplateDocumentedCommandActuallyRuns (the_templates_own_commands_
// are_run_test.go, cleat#1947) does run commands, but only the ones a
// SCAFFOLDED PROJECT's own Makefile/README tell its user to run -- a strict
// subset of cmd/cleat/templates/. None of the ten files #1970 was documented
// in is a template, so that guard could not have caught it.
//
// This test runs every `cleat <subcommand> ...` line in ANY tracked
// markdown file far enough to clear flag parsing. That is deliberately
// narrow: it does not assert the command succeeds, only that flag.Parse
// (global or subcommand) accepted every flag it was given. A doc's
// positional argument -- a wasm path, a workflow name -- is free to be a
// placeholder that goes on to fail for an unrelated reason (file not found,
// connection refused); this guard does not look past the flags, because it
// cannot know what a real deployment target or workflow file would be.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// docInvocation matches a `cleat ...` line inside a fenced code block, with
// an optional shell prompt. Anchored at line start, like documented_cli_
// surface_test.go's invocation regexp, so prose mentioning "cleat" mid
// sentence is not mistaken for a command.
var docInvocation = regexp.MustCompile(`(?m)^[ \t]*\$?[ \t]*cleat[ \t]+(\S.*)$`)

// docCommandBaseline records `<file>|cleat <line>` pairs this guard cannot
// yet clear, parked rather than fixed for the same reason docBaseline in
// documented_cli_surface_test.go is: the right fix is not always knowable
// from this test alone. It may only shrink, and TestEveryDocumentedCleat
// InvocationClearsFlagParsing below already asserts that -- an entry with
// nothing left in the tree to match is a grant covering nothing.
//
// cleat#2029's four entries were all fixed by rewriting the docs to the real
// CLI rather than by adding the flags they showed -- see that issue and the
// PR that closed it for the four sites and what each became.
var docCommandBaseline = map[string]string{}

func TestEveryDocumentedCleatInvocationClearsFlagParsing(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	root := repoRootForCLIScan(t)
	knownSubcommands := cliSurface(t, root)["cleat"]

	out, err := exec.Command("git", "-C", root, "ls-files", "*.md").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	docs := strings.Fields(string(out))
	// A scan that found nothing to scan reports a clean tree, which is the
	// "checks never started" reading. The floor belongs on the success path,
	// matching documented_cli_surface_test.go's own guard.
	if len(docs) < 50 {
		t.Fatalf("git ls-files matched %d markdown files; the scan did not see the repo", len(docs))
	}

	workDir := t.TempDir()
	tested := 0
	seen := map[string]bool{}
	for _, doc := range docs {
		// FROM THE WORKING TREE, not `git show HEAD:` -- see documented_cli_
		// surface_test.go's comment on why: a scratch worktree containing an
		// unfixed doc must not leak into this scan's file list, and an
		// uncommitted fix in progress must be checkable before it is
		// committed.
		body, err := os.ReadFile(filepath.Join(root, doc))
		if err != nil {
			t.Fatalf("read %s: %v", doc, err)
		}
		for _, block := range fencedBlock.FindAllStringSubmatch(string(body), -1) {
			lines := strings.Split(block[1], "\n")
			for i := 0; i < len(lines); i++ {
				m := docInvocation.FindStringSubmatch(lines[i])
				if m == nil {
					continue
				}
				rest := strings.TrimSpace(m[1])
				// Join shell line-continuations, the same way documented
				// CleatCommands does for template scaffolds.
				for strings.HasSuffix(rest, `\`) && i+1 < len(lines) {
					i++
					rest = strings.TrimSuffix(rest, `\`) + " " + strings.TrimSpace(lines[i])
				}
				if strings.Contains(rest, "$(") || strings.Contains(rest, "{{") {
					continue // depends on a shell or template variable this test cannot expand
				}

				fields := strings.Fields(rest)
				sub := ""
				for _, f := range fields {
					// Skip GLOBAL flags -- and their values, which do not
					// carry a "-" prefix and would otherwise be mistaken for
					// the subcommand position. `cleat --db X deploy ...` is
					// the correct spelling; treating the first non-flag
					// token as the subcommand unconditionally is what made
					// cleat#1933's defect look like a missing flag.
					if strings.HasPrefix(f, "-") {
						continue
					}
					if knownSubcommands[f] {
						sub = f
						break
					}
				}
				if sub == "" {
					continue // not a recognized subcommand invocation; existence is TestEveryDocumentedSubcommandExists's job, not this test's
				}

				key := doc + "|cleat " + rest
				seen[key] = true
				if _, parked := docCommandBaseline[key]; parked {
					continue
				}

				cmd := exec.Command(cleatBinary, fields...)
				cmd.Dir = workDir
				raw, _ := cmd.CombinedOutput()
				if strings.Contains(string(raw), "flag provided but not defined") {
					t.Errorf("%s documents `cleat %s`, which does not clear flag parsing:\n%s\n\n"+
						"Fix the document or the flag, or add the pair to docCommandBaseline "+
						"with a reason if the right fix is not knowable (cleat#1970).",
						doc, rest, raw)
				}
				tested++
			}
		}
	}
	if tested == 0 {
		t.Fatal("extracted 0 cleat invocations from tracked markdown -- the scan did not see what it exists to check")
	}

	// A baseline entry that no longer matches anything is a grant covering
	// something that is not there -- documented_cli_surface_test.go asserts
	// the same thing about its own docBaseline, for the same reason.
	for key, why := range docCommandBaseline {
		if !seen[key] {
			t.Errorf("docCommandBaseline has %q (%s) but nothing in the tree matches it. "+
				"The document was fixed or renamed: delete the entry.", key, why)
		}
	}
}
