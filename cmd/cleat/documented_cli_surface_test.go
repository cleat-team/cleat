package main

// Every `cleat <subcommand>` and `cleatctl <subcommand>` written in a
// user-facing document must exist in the binary that document names.
//
// cleat#1315 is what the absence of this check costs. `docs/operations/
// upgrading.md` prescribed `cleat migrate up` as STEP ONE of an ordinary
// upgrade and `cleat migrate down` as the rollback, and there has never been a
// `migrate` subcommand on either binary. Someone following the runbook on a
// good day types a command that prints usage text before anything else
// happens; someone following it on a bad day is mid-incident.
//
// THE SURFACE IS DERIVED FROM THE DISPATCH, NOT FROM THE HELP TEXT. A help
// string is documentation too, and checking documentation against documentation
// is one derivation run twice -- the trap CLAUDE.md records as two scans
// agreeing on a number while differing on six members. So the set comes from
// the `switch command` statement plus the handful of commands dispatched before
// it, and a separate assertion below requires the help string to match. Either
// can now be wrong; they can no longer be wrong together.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// preSwitchCommands are dispatched by an `args[0] == "x"` comparison before the
// switch, so a scan of case clauses alone cannot see them. Listed here rather
// than pattern-matched because there are two of them and a pattern for
// "comparison against args[0] anywhere in the function" would also match
// argument parsing inside a subcommand.
var preSwitchCommands = map[string][]string{
	"cleat": {"init", "version"}, // cmd/cleat/main.go, above `switch command`
}

// docBaseline records `<binary> <subcommand>` pairs that appear in a document
// and do NOT exist, and are not being fixed in the change that introduced this
// test. It may only shrink.
//
// Every entry is a real defect. They are parked rather than fixed because the
// right replacement for each is a guess without knowing what the command was
// meant to do -- `cleat schedules` may be a stale plural of `cleat schedule`,
// or may predate it. Fixing them blind would replace a visible error with an
// invisible one.
var docBaseline = map[string]string{
	// `cleat <sub>` -- contributor and design documents. Left for someone who
	// knows what each was meant to do; see the note above.
	"docs/contributor/design/cleat-execution-design.md|cleat schedules": "possibly a stale plural of `cleat schedule`",
	"docs/contributor/plugins/plugin-migration-guide.md|cleat tenant":   "`cleatctl drop-tenant` is the nearest real command",
	"docs/contributor/plugins/plugin-migration-guide.md|cleat migrate":  "the same absent subcommand the upgrade runbook had",
	"docs/contributor/plugins/plugin-security.md|cleat config":          "no config subcommand on either binary",
	"docs/operations/abi-migration.md|cleat start":                      "`cleat run` and `cleat dev` are the nearest real commands",
	"examples/third-party-plugin/README.md|cleat workflow":              "no workflow subcommand on either binary",

	// `cleatctl <sub>` -- found by this guard, not by the issue that prompted
	// it, because the issue scanned only `cleat`. Each names a capability
	// rather than a renamed command, so none has an obvious replacement:
	// `cleatctl plugin status` is wrong in the binary AND the subcommand
	// (`cleat plugin` has validate/install/list/update/uninstall, no status),
	// and `cleatctl routing set` describes traffic splitting that does not
	// exist -- the only `routing` in the tree is the WASM backend's.
	"docs/troubleshooting.md|cleatctl events":                  "no events subcommand; `cleatctl replay` and `cleatctl debug` are the nearest",
	"docs/troubleshooting.md|cleatctl plugin":                  "wrong binary and wrong subcommand; `cleat plugin` has no status",
	"docs/explanation/workflow-versioning.md|cleatctl routing": "describes traffic splitting with no implementation",
}

func repoRootForCLIScan(t *testing.T) string {
	t.Helper()
	// Fatal, not Skip: this repo IS a git checkout and CI clones it, so a skip
	// would make the guard report success on the one tree it exists to scan.
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// dispatchedCommands returns the subcommands a binary's main.go actually
// dispatches, read from the case clauses of the switch whose tag is `ident`.
func dispatchedCommands(t *testing.T, path, ident string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	found := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok {
			return true
		}
		tag, ok := sw.Tag.(*ast.Ident)
		if !ok || tag.Name != ident {
			return true
		}
		for _, stmt := range sw.Body.List {
			cc, ok := stmt.(*ast.CaseClause)
			if !ok {
				continue
			}
			for _, e := range cc.List {
				lit, ok := e.(*ast.BasicLit)
				if ok && lit.Kind == token.STRING {
					if v, err := strconv.Unquote(lit.Value); err == nil {
						found[v] = true
					}
				}
			}
		}
		return true
	})
	if len(found) == 0 {
		t.Fatalf("no case clauses found in %s under `switch %s` -- the dispatch was "+
			"renamed or restructured, and an empty surface would fail every document "+
			"rather than pass them", path, ident)
	}
	return found
}

func cliSurface(t *testing.T, root string) map[string]map[string]bool {
	t.Helper()
	surface := map[string]map[string]bool{
		"cleat":    dispatchedCommands(t, filepath.Join(root, "cmd/cleat/main.go"), "command"),
		"cleatctl": dispatchedCommands(t, filepath.Join(root, "cmd/cleatctl/main.go"), "cmd"),
	}
	for bin, extra := range preSwitchCommands {
		for _, c := range extra {
			surface[bin][c] = true
		}
	}
	return surface
}

var fencedBlock = regexp.MustCompile("(?s)```[^\n]*\n(.*?)```")

func TestEveryDocumentedSubcommandExists(t *testing.T) {
	root := repoRootForCLIScan(t)
	surface := cliSurface(t, root)

	out, err := exec.Command("git", "-C", root, "ls-files", "*.md").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	docs := strings.Fields(string(out))
	// A scan that found nothing to scan reports a clean tree, which is the
	// "checks never started" reading. The floor belongs on the success path.
	if len(docs) < 50 {
		t.Fatalf("git ls-files matched %d markdown files; the scan did not see the repo", len(docs))
	}

	invocation := regexp.MustCompile(`(?m)^\s*(?:\$\s*)?(cleat|cleatctl)\s+([a-z][a-z0-9-]*)`)
	seen := map[string]bool{}
	for _, doc := range docs {
		// FROM THE WORKING TREE, not from `git show HEAD:`. The first version of
		// this test read HEAD and was exactly backwards: it passed a change that
		// ADDED a bad command (not yet committed) and failed a change that fixed
		// one (fix not yet committed). `git ls-files` supplies the file LIST --
		// so scratch worktrees stay out of scope -- and the bytes come from disk.
		body, err := os.ReadFile(filepath.Join(root, doc))
		if err != nil {
			t.Fatalf("read %s: %v", doc, err)
		}
		for _, block := range fencedBlock.FindAllStringSubmatch(string(body), -1) {
			for _, m := range invocation.FindAllStringSubmatch(block[1], -1) {
				bin, sub := m[1], m[2]
				if surface[bin][sub] {
					continue
				}
				key := doc + "|" + bin + " " + sub
				seen[key] = true
				if _, parked := docBaseline[key]; parked {
					continue
				}
				t.Errorf("%s documents `%s %s`, which %s does not dispatch.\n\n"+
					"A reader follows this literally. Fix the document, or add the "+
					"pair to docBaseline with a reason if the right replacement is "+
					"not knowable (cleat#1315).", doc, bin, sub, bin)
			}
		}
	}

	// A baseline entry that no longer matches anything is a grant covering
	// something that is not there -- the same defect as a ledger line matching
	// zero skips. It must be deleted when the document is fixed.
	for key, why := range docBaseline {
		if !seen[key] {
			t.Errorf("docBaseline has %q (%s) but nothing in the tree matches it. "+
				"The document was fixed or renamed: delete the entry.", key, why)
		}
	}
}

// The help text is documentation, and it drifts the same way a runbook does.
// Deriving the surface from the dispatch above makes this checkable: the two
// can now disagree, which is the only reason their agreement is worth anything.
func TestTheUsageTextListsExactlyWhatIsDispatched(t *testing.T) {
	root := repoRootForCLIScan(t)
	surface := cliSurface(t, root)["cleat"]

	src, err := os.ReadFile(filepath.Join(root, "cmd/cleat/main.go"))
	if err != nil {
		t.Fatalf("read cmd/cleat/main.go: %v", err)
	}
	m := regexp.MustCompile(`Valid commands: ([a-z0-9, -]+)`).FindSubmatch(src)
	if m == nil {
		t.Fatal("no `Valid commands:` line in cmd/cleat/main.go -- this test cannot " +
			"compare a list it cannot find, and silently passing would be the empty green")
	}
	listed := map[string]bool{}
	for _, c := range strings.Split(string(m[1]), ",") {
		if c = strings.TrimSpace(c); c != "" {
			listed[c] = true
		}
	}
	for c := range surface {
		if !listed[c] {
			t.Errorf("`cleat %s` is dispatched but missing from the usage text", c)
		}
	}
	for c := range listed {
		if !surface[c] {
			t.Errorf("the usage text offers `cleat %s`, which is not dispatched", c)
		}
	}
}
