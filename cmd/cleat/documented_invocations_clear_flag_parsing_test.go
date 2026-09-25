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

// docInvocation matches a `cleat ...` line inside a fenced code block, with
// an optional shell prompt. Anchored at line start, like documented_cli_
// surface_test.go's invocation regexp, so prose mentioning "cleat" mid
// sentence is not mistaken for a command.
var docInvocation = regexp.MustCompile(`(?m)^[ \t]*\$?[ \t]*cleat[ \t]+(\S.*)$`)

// shellFields splits a documented invocation the way a shell would, not the
// way strings.Fields would: every `--input` example in this tree is a JSON
// literal wrapped in single quotes and containing spaces
// (`--input '{"name": "World"}'`), and strings.Fields blows that apart into
// one token per word. A caller that then re-joins fields with spaces (as
// exec.Command effectively does one argv slot per field) never sees the
// value a real shell would have passed as a single argument. Also drops an
// unquoted trailing `# comment`, the same as a real shell -- DX_COMPARISON.md
// annotates several invocations that way (`... ./order.wasm   # INSERT`), and
// without this the comment text is mistaken for extra positional arguments.
// Handles single and double quotes; does not handle backslash escapes or
// nested quotes of the same kind, because nothing documented here needs them.
func shellFields(s string) []string {
	var fields []string
	var cur strings.Builder
	inField := false
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
				continue
			}
			cur.WriteByte(c)
			continue
		}
		switch {
		case c == '#':
			i = len(s) // stop: the rest of the line is a comment
		case c == '\'' || c == '"':
			quote = c
			inField = true
		case c == ' ' || c == '\t':
			if inField {
				fields = append(fields, cur.String())
				cur.Reset()
				inField = false
			}
		default:
			cur.WriteByte(c)
			inField = true
		}
	}
	if inField {
		fields = append(fields, cur.String())
	}
	return fields
}

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

// docFlagToken reports whether s is a flag token rather than a value or a
// positional: one or two leading dashes followed by a letter, digit or
// underscore. A bare "-" or "--" is not a flag.
func docFlagToken(s string) bool {
	if len(s) < 2 || s[0] != '-' {
		return false
	}
	body := strings.TrimLeft(s, "-")
	if body == "" {
		return false
	}
	c := body[0]
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// docFlagName is the name the flag package prints in its rejection, which is
// the token without its leading dashes and without any "=value" suffix.
func docFlagName(s string) string {
	name := strings.TrimLeft(s, "-")
	if eq := strings.Index(name, "="); eq >= 0 {
		name = name[:eq]
	}
	return name
}

// docMoveToken returns fields with the token at index from moved to index to.
func docMoveToken(fields []string, from, to int) []string {
	rest := make([]string, 0, len(fields))
	for i, f := range fields {
		if i != from {
			rest = append(rest, f)
		}
	}
	at := to
	if to > from {
		at--
	}
	if at < 0 {
		at = 0
	}
	if at > len(rest) {
		at = len(rest)
	}
	out := append([]string{}, rest[:at]...)
	out = append(out, fields[from])
	return append(out, rest[at:]...)
}

// probeFindsUndefinedFlag re-runs the invocation with the token at flagIdx
// moved to a position flag.Parse actually reaches, and reports whether the
// binary then rejects that flag by name.
//
// The insertion point cannot be assumed, which is the whole reason this is a
// probe rather than a lookup. The dispatcher consumes a variable number of
// leading words -- `plugin update` and `schedule add` are two tokens, not one
// -- and inserting after the first of them yields "Unknown plugin subcommand:
// --show" instead of the flag error, which would hide exactly what this is
// looking for. So every position in the run of leading non-flag tokens is
// tried, and only the flag signal counts.
func probeFindsUndefinedFlag(binary, dir string, fields []string, subIdx, flagIdx int) bool {
	limit := len(fields)
	for i := subIdx + 1; i < len(fields); i++ {
		if docFlagToken(fields[i]) {
			limit = i
			break
		}
	}
	want := "flag provided but not defined: -" + docFlagName(fields[flagIdx])
	for p := subIdx + 1; p <= limit; p++ {
		if p == flagIdx {
			continue // already where it is; that position is what the caller ran
		}
		cmd := exec.Command(binary, docMoveToken(fields, flagIdx, p)...)
		cmd.Dir = dir
		raw, _ := cmd.CombinedOutput()
		if strings.Contains(string(raw), want) {
			return true
		}
	}
	return false
}

// flagsAfterPositionals returns the flag tokens in fields that flag.Parse
// cannot reach, because it stops at the first non-flag argument.
//
// This is cleat#2136. The run this test already made only inspects the prefix
// flag.Parse consumes, so `cleat plugin update example/hello-world@0.2.0
// --show` passed every check while `--show` does not exist -- the positional
// ends parsing and the flag after it is never looked at. That is how the
// fictional `--show` in cleat#2074 cleared this guard.
//
// Deliberately NOT a table of known flag names. The flags are registered
// inline in each command's own function, so a list here would be a second
// source of truth, and the drift would be silent in the direction that
// matters: a flag added to a command and not to the list reads as undefined.
// Asking the binary keeps the FlagSet itself the only definition.
func flagsAfterPositionals(binary, dir string, fields []string, subIdx int) []string {
	if subIdx < 0 || subIdx >= len(fields) {
		return nil
	}
	// Only a token after the first non-flag word can be hidden. Everything
	// before it is the prefix flag.Parse already consumed.
	firstBare := -1
	for i := subIdx + 1; i < len(fields); i++ {
		if !docFlagToken(fields[i]) {
			firstBare = i
			break
		}
	}
	if firstBare < 0 {
		return nil
	}

	var found []string
	afterDoubleDash := false
	for i := firstBare + 1; i < len(fields); i++ {
		if fields[i] == "--" {
			afterDoubleDash = true
			continue
		}
		if afterDoubleDash || !docFlagToken(fields[i]) {
			continue
		}
		if probeFindsUndefinedFlag(binary, dir, fields, subIdx, i) {
			found = append(found, fields[i])
		}
	}
	return found
}

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

				fields := shellFields(rest)
				sub := ""
				subIdx := -1
				for i, f := range fields {
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
						// subIdx is the SUBCOMMAND's index, not the first
						// non-flag token's, and the two come apart whenever a
						// global flag takes a value: `cleat --db X deploy ...`
						// has a non-flag token at index 1 that is a value, not
						// the command. Using that one made the check below
						// insert a subcommand flag into the GLOBAL flag
						// region, where it is legitimately undefined -- ten
						// healthy doc lines reported before this was fixed.
						sub, subIdx = f, i
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
				// The run above stops where flag.Parse stops, so it cannot see
				// a flag placed after a positional (cleat#2136).
				for _, tok := range flagsAfterPositionals(cleatBinary, workDir, fields, subIdx) {
					t.Errorf("%s documents `cleat %s`, where %s sits after a positional argument, "+
						"so flag.Parse stops before it and nothing on this path inspects it.\n"+
						"Moving %s to a position the command's own FlagSet parses makes the binary "+
						"reject it by name: it is not a flag of this command. (A global flag placed "+
						"after the subcommand lands here too, and globals must come before it -- see "+
						"cmd/cleat/main.go's usage.) Fix the document, R7. Do not remove this check "+
						"to accommodate it.",
						doc, rest, tok, tok)
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

// TestTheGuardSeesAFlagAfterAPositional is the known-positive for cleat#2136,
// and it is the acceptance test for the change rather than a formality: the
// defect was that this guard could not fail on the case it was written for,
// so a version of it that still cannot would pass everything else here.
//
// The pair below is one row measured before and after -- the same command,
// the same position, one token different -- because a control in a different
// row cannot say why the first one came out the way it did.
func TestTheGuardSeesAFlagAfterAPositional(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	dir := t.TempDir()

	// Known-positive: `plugin update` registers -all and -index-url, and has
	// never had a -show. Until cleat#2136 this returned nothing, because
	// flag.Parse stopped at the positional and never looked past it.
	bad := []string{"plugin", "update", "example/hello-world@0.2.0", "--show"}
	got := flagsAfterPositionals(cleatBinary, dir, bad, 0)
	if len(got) != 1 || got[0] != "--show" {
		t.Errorf("flagsAfterPositionals(%v) = %v, want exactly [--show].\n"+
			"This is the cleat#2074 case: the flag sits after a positional, so flag.Parse never "+
			"reaches it, and nothing else in this file can see that it does not exist.\n"+
			"Verified by hand against the built binary: `cleat plugin update --show x` prints "+
			"\"flag provided but not defined: -show\", while the documented order prints nothing "+
			"about flags at all.", bad, got)
	}

	// The control. -all IS a flag of `plugin update`, in the same position,
	// so it must not be reported. Without this, a probe that reported every
	// flag it was shown would have passed the case above and told us nothing:
	// a check that cannot disagree with itself is a claim, not a check.
	good := []string{"plugin", "update", "example/hello-world@0.2.0", "--all"}
	if got := flagsAfterPositionals(cleatBinary, dir, good, 0); len(got) != 0 {
		t.Errorf("flagsAfterPositionals(%v) = %v, want none: --all is registered on `plugin update`, "+
			"so a flag placed after a positional is not by itself the defect.", good, got)
	}

	// And a token after a literal -- stays positional, per the flag package's
	// own rule, so it is not a flag to check however flag-shaped it looks.
	afterSep := []string{"plugin", "update", "example/hello-world@0.2.0", "--", "--show"}
	if got := flagsAfterPositionals(cleatBinary, dir, afterSep, 0); len(got) != 0 {
		t.Errorf("flagsAfterPositionals(%v) = %v, want none: everything after a literal -- is a "+
			"positional, and the flag package does not inspect it either.", afterSep, got)
	}

	// The shape that produced ten false positives while this was being
	// written, kept as a control because it is the shape real docs use. A
	// GLOBAL flag takes a value, so a bare non-flag token appears BEFORE the
	// subcommand. An earlier version of this check took the first non-flag
	// token as the subcommand and inserted the token before the real one --
	// landing it in the global parse region, where a subcommand flag is
	// genuinely undefined -- and reported `--name` on `deploy`, which is a
	// flag `deploy` has always had.
	global := []string{"--db", "postgres://example/cleat", "deploy", "--name", "agent", "./out/agent_loop.wasm"}
	if got := flagsAfterPositionals(cleatBinary, dir, global, 2); len(got) != 0 {
		t.Errorf("flagsAfterPositionals(%v) = %v, want none: --name is registered on `deploy`, and "+
			"the value of the global --db is not the subcommand. Reporting here means the search is "+
			"reading the wrong token as the command and inserting flags before it.", global, got)
	}
}

// cleat#2048. TestEveryDocumentedCleatInvocationClearsFlagParsing checks that
// every documented FLAG exists. It does not check POSITIONAL arguments --
// its own comment says so -- so `cleat deploy <name> <wasm>` and
// `cleat run <Entry> '<json>'` both cleared it while being wrong: `deploy`
// only ever reads remainder[0] as the wasm path (a second bare positional is
// silently ignored, not an error), and `run` without --wasm treats
// remainder[0] as a package DIRECTORY to build, not an entry-point name.
// Twelve `deploy` docs and fourteen `run` docs carried the broken form
// (cleat#2048) and none of them tripped the flag-parsing guard, because
// nothing was wrong with their flags -- only with what came after.
//
// This is the positional half of that guard: for `deploy` and `run`, assert
// the SHAPE of what is left after flags are removed matches what the
// subcommand actually does with it.

// subcommandFlagValueKinds parses the fs.String/fs.Int/fs.Bool declarations
// inside the named function in path, returning flagName -> takesValue (true
// for String/Int, false for Bool). AST-derived, like dispatchedCommands
// above, so a flag added to runDeploy/runEmbedded later is picked up
// automatically -- a hand-maintained list would silently misclassify a new
// flag's value as a positional argument instead of failing loud.
func subcommandFlagValueKinds(t *testing.T, path, funcName string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	kinds := map[string]bool{}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != funcName {
			return true
		}
		found = true
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			var takesValue bool
			switch sel.Sel.Name {
			case "String", "Int", "Int64", "Float64", "Duration":
				takesValue = true
			case "Bool":
				takesValue = false
			default:
				return true
			}
			if len(call.Args) == 0 {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			name, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			kinds[name] = takesValue
			return true
		})
		return false
	})
	if !found {
		t.Fatalf("no func %s found in %s -- it was renamed, and this test's positional "+
			"check would silently pass every document instead of checking them", funcName, path)
	}
	if len(kinds) == 0 {
		t.Fatalf("found func %s in %s but no fs.String/fs.Int/fs.Bool flag declarations -- "+
			"it was restructured, and an empty flag set would misclassify every flag's own "+
			"value as a positional argument", funcName, path)
	}
	return kinds
}

// positionalArgsAfterFlags strips this subcommand's own flags (and, for a
// flag that takes a value, the following token) from fields, leaving only
// the bare positional arguments. A flag not in flagTakesValue is assumed to
// take no value -- if it were undefined entirely,
// TestEveryDocumentedCleatInvocationClearsFlagParsing already fails it.
func positionalArgsAfterFlags(fields []string, flagTakesValue map[string]bool) []string {
	var positionals []string
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if !strings.HasPrefix(f, "-") {
			positionals = append(positionals, f)
			continue
		}
		name := strings.TrimLeft(f, "-")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			// flag=value form; the value travels with this token, nothing
			// more to consume.
		} else if flagTakesValue[name] && i+1 < len(fields) {
			i++
		}
	}
	return positionals
}

// deployRunPositionalBaseline is docCommandBaseline's sibling for this
// check: a doc this test cannot yet clear, parked with a reason rather than
// silently ignored. Same shrink-only contract, checked at the end of the
// test below.
var deployRunPositionalBaseline = map[string]string{}

func TestDeployAndRunDocumentedPositionalsAreTheKindTheSubcommandExpects(t *testing.T) {
	root := repoRootForCLIScan(t)
	deployFlags := subcommandFlagValueKinds(t, filepath.Join(root, "cmd/cleat/main.go"), "runDeploy")
	runFlags := subcommandFlagValueKinds(t, filepath.Join(root, "cmd/cleat/run_embedded.go"), "runEmbedded")

	out, err := exec.Command("git", "-C", root, "ls-files", "*.md", "*.py").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	docs := strings.Fields(string(out))
	if len(docs) < 50 {
		t.Fatalf("git ls-files matched %d files; the scan did not see the repo", len(docs))
	}

	seen := map[string]bool{}
	checked := 0
	for _, doc := range docs {
		body, err := os.ReadFile(filepath.Join(root, doc))
		if err != nil {
			t.Fatalf("read %s: %v", doc, err)
		}
		blocks := fencedBlock.FindAllStringSubmatch(string(body), -1)
		if strings.HasSuffix(doc, ".py") {
			// research_agent.py and hello_workflow.py document their own
			// invocation in a module docstring, not a fenced markdown block --
			// this test's whole reason to exist is that exactly this kind of
			// doc-comment invocation goes unchecked otherwise (cleat#2048).
			blocks = [][]string{{string(body), string(body)}}
		}
		for _, block := range blocks {
			lines := strings.Split(block[1], "\n")
			for i := 0; i < len(lines); i++ {
				m := docInvocation.FindStringSubmatch(lines[i])
				if m == nil {
					continue
				}
				rest := strings.TrimSpace(m[1])
				for strings.HasSuffix(rest, `\`) && i+1 < len(lines) {
					i++
					rest = strings.TrimSuffix(rest, `\`) + " " + strings.TrimSpace(lines[i])
				}
				if strings.Contains(rest, "$(") || strings.Contains(rest, "{{") {
					continue
				}
				fields := shellFields(rest)
				if len(fields) == 0 {
					continue
				}
				sub := fields[0]
				if sub != "deploy" && sub != "run" {
					continue
				}
				key := doc + "|cleat " + rest
				seen[key] = true
				if _, parked := deployRunPositionalBaseline[key]; parked {
					continue
				}
				checked++

				args := fields[1:]
				if sub == "deploy" {
					positionals := positionalArgsAfterFlags(args, deployFlags)
					if len(positionals) != 1 || !strings.HasSuffix(positionals[0], ".wasm") {
						t.Errorf("%s documents `cleat %s`: deploy only ever reads its FIRST "+
							"positional as the wasm path (cmd/cleat/main.go's runDeploy, "+
							"`wasmPath := remainder[0]`) -- a second bare positional (a workflow "+
							"name, say) is silently ignored, not an error. Got %d positional(s) "+
							"%v, want exactly one ending in \".wasm\". Use --name for the workflow "+
							"name.", doc, rest, len(positionals), positionals)
					}
					continue
				}

				// run
				usesWasmFlag := false
				for _, f := range args {
					if f == "--wasm" || strings.HasPrefix(f, "--wasm=") {
						usesWasmFlag = true
						break
					}
				}
				positionals := positionalArgsAfterFlags(args, runFlags)
				if usesWasmFlag {
					if len(positionals) != 0 {
						t.Errorf("%s documents `cleat %s`: --wasm was given, so run_embedded.go "+
							"never looks at a positional at all -- %d unexpected positional(s) %v. "+
							"Use --entry-point and --input instead of a bare entry name and JSON "+
							"literal.", doc, rest, len(positionals), positionals)
					}
				} else if len(positionals) != 1 || strings.HasPrefix(positionals[0], "{") {
					t.Errorf("%s documents `cleat %s`: without --wasm, run_embedded.go treats "+
						"remainder[0] as a PACKAGE DIRECTORY to build (cmd/cleat/run_embedded.go's "+
						"runEmbedded, `pkgPath := remainder[0]`), not an entry-point name -- got %d "+
						"positional(s) %v. Either pass a real package path as the sole positional, "+
						"or use --wasm/--entry-point/--input to run a pre-built module.",
						doc, rest, len(positionals), positionals)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("extracted 0 `cleat deploy`/`cleat run` invocations -- the scan did not see what it exists to check")
	}

	for key, why := range deployRunPositionalBaseline {
		if !seen[key] {
			t.Errorf("deployRunPositionalBaseline has %q (%s) but nothing in the tree matches it. "+
				"The document was fixed or renamed: delete the entry.", key, why)
		}
	}
}
