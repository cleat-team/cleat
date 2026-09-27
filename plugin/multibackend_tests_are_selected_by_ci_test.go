package plugin_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A test that asks testutil.NewPluginTestBackends for every dialect must be
// SELECTED by the only CI job that supplies MySQL and SQL Server DSNs. Both, or
// it runs on PostgreSQL and reports green for three.
//
// cleat#1174. TestReaperTouchesNothingItShouldNot shipped in #1150 without the
// `_MultiBackend` suffix, and multi-db-ci.yml selected that job's work by name:
//
//	go test -count=1 -timeout=900s -run 'MultiBackend' ./plugins/...
//
// AS OF cleat#1203 THAT STEP RUNS UNFILTERED, so on today's tree this guard is
// satisfied by construction and is here for the day someone adds a filter back.
// It is not decoration: the filter had already hidden six more tests by the
// time it was removed, and the reason it is gone rather than worked around is
// that a naming contract fails silently in the one direction nobody checks.
//
// ci.yml's package matrix does run ./plugins/... unfiltered, but sets only
// CLEAT_TEST_DB, so the excluded test got PostgreSQL and nothing else. Its own
// docstring says it is the negative control without which the positive test
// passes against `UPDATE task_queue SET status = 'pending', started_at = NULL`
// -- no WHERE clause at all. That was the state on MySQL and SQL Server, and
// the SQL Server arm is the three-column join #1143 had just rewritten, which
// is the shape where over-reaping is easiest to introduce.
//
// THIS IS THE SECOND INSTANCE OF ONE SHAPE, which is why it is a test rather
// than a note. engine/tier2_toolchain_tests_placement_test.go exists because
// "TestARust" does not contain "TestRust", so three tests ran in no job at all.
// Same mechanism: a -run pattern is a naming contract that nothing enforces,
// and `go test` reports an unmatched -run as success.
//
// The pattern is READ FROM THE WORKFLOW rather than written here. A guard that
// hardcodes 'MultiBackend' passes the day someone changes the filter, which is
// the one day it needed to fail.
func TestEveryMultiBackendTestIsSelectedByTheMultiDialectJob(t *testing.T) {
	root := repoRoot(t)

	selectors := pluginsSelectors(t, root)
	if len(selectors) == 0 {
		t.Fatal("no `go test ... ./plugins/...` invocation in .github/workflows.\n\n" +
			"An unfiltered run is represented as a selector that matches everything, not as " +
			"an absent one, so reaching here means the multi-dialect job stopped running " +
			"./plugins/... AT ALL. That is the failure this is worth being loud about: it " +
			"removes every dialect arm from CI while each test keeps passing locally.")
	}

	tests := multiBackendTests(t, root)
	if len(tests) < 8 {
		t.Fatalf("the closure found only %d test(s) reaching testutil.%s; there were 12 on "+
			"2026-09-10 -- six calling it directly and six through plugintest.RunEveryArm.\n\n"+
			"A floor rather than a count, because the population grows: this fires when the "+
			"DETECTION breaks, not when someone adds a plugin. It is set above 6 on purpose. "+
			"The previous detection found exactly the six direct callers and reported itself "+
			"complete, so a floor of 4 was satisfied by a census missing half its population "+
			"(cleat#1203).", len(tests), seedName)
	}

	var unselected []string
	for _, fn := range tests {
		if !matchesAnySelector(fn.name, selectors) {
			unselected = append(unselected, fn.name+"  ("+fn.file+")")
		}
	}
	if len(unselected) > 0 {
		sort.Strings(unselected)
		t.Errorf("%d of %d multi-backend test(s) are not selected by the job that supplies "+
			"the MySQL and SQL Server DSNs:\n\n  %s\n\n"+
			"They ask NewPluginTestBackends for three dialects and CI gives them one. Nothing "+
			"reports it: an unmatched -run is not an error, and the test still passes on "+
			"PostgreSQL in ci.yml's package matrix.\n\n"+
			"The filter was REMOVED in cleat#1203 rather than the tests renamed, so a "+
			"pattern being in force here means one was reintroduced. Prefer removing it "+
			"again: unfiltered cost 31s against 13s filtered, measured with all three DSNs "+
			"set, against the step's 900s timeout -- and renaming leaves the contract as a "+
			"convention the next author has to know. Patterns in force: %v",
			len(unselected), len(tests), strings.Join(unselected, "\n  "), selectorSources(selectors))
	}
}

type multiBackendTest struct{ name, file string }

// seedName is the function whose call makes a test multi-backend: it returns
// one backend per DSN that is set, so a test that reaches it and is not run
// with MySQL and SQL Server configured reports green for three dialects having
// exercised one.
const seedName = "NewPluginTestBackends"

const seedPkg = "engine/testutil"

// multiBackendTests finds every Test function under plugins/ that reaches
// testutil.NewPluginTestBackends, DIRECTLY OR THROUGH A HELPER.
//
// "Through a helper" is the whole of this function's difficulty, and the first
// version of this guard did not have it. It scanned for a call in the test's
// own body, over `git ls-files 'plugins/**/*_test.go'`. Six tests call
// plugintest.RunEveryArm, which calls the seed at plugins/plugintest/arms.go:69
// -- one level of indirection, and they dropped out of the census silently.
// cleat#1203.
//
// It is worth being exact about how that got through, because the case WAS
// anticipated: the old scan fataled if it found the seed outside a Test
// function, with a message saying a helper would make the failure silent and
// to extend the scan before adding one. The tripwire never fired, because its
// file list was test files only and the helper lives in arms.go. THE GUARD
// LOOKED FOR THE HAZARD ONLY WHERE THE HAZARD CANNOT BE. Its floor message
// read "there were 6 when this guard was written" -- 6 detected, and 6 more it
// could not see, which is the flattering direction CLAUDE.md describes.
//
// So the scan is now a reachability closure over every tracked .go file in the
// module, seeded at the one function and iterated to a fixed point. Edges are
// resolved through each file's own import set rather than by bare name, so
// `plugintest.RunEveryArm` reaches the right RunEveryArm and a same-named
// function in another package does not join by accident.
//
// KNOWN LIMIT, stated rather than left to be discovered: an edge through a
// method value or an interface is not followed -- `x.Run()` resolves only when
// x is a package identifier. TestTheDetectionFollowsAHelper is the
// known-positive for the closure, and crossCheckAgainstAnIdentifierScan is the
// deliberately different second reading, which fails when the closure cannot
// account for a file that names the seed.
func multiBackendTests(t *testing.T, root string) []multiBackendTest {
	t.Helper()

	files := parseTrackedGoFiles(t, root)
	mod := modulePath(t, root)
	seed := funcKey{pkg: mod + "/" + seedPkg, name: seedName}

	// Every function in the module, keyed by import path and name, with the
	// set of functions its body can reach in one step.
	type fn struct {
		key   funcKey
		file  string
		isTst bool
		calls []funcKey
	}
	var all []fn
	seedDefined := false
	for _, f := range files {
		for _, d := range f.ast.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			key := funcKey{pkg: f.pkgPath, name: fd.Name.Name}
			if key == seed {
				seedDefined = true
			}
			all = append(all, fn{
				key:   key,
				file:  f.rel,
				isTst: strings.HasSuffix(f.rel, "_test.go") && strings.HasPrefix(fd.Name.Name, "Test"),
				calls: f.references(fd.Body),
			})
		}
	}
	if !seedDefined {
		t.Fatalf("no function %s.%s in the module.\n\n"+
			"The seed this guard is built on was renamed or moved. A closure seeded on a "+
			"name that does not exist reaches nothing and reports every test as fine, which "+
			"is why this is fatal rather than an empty result.", seed.pkg, seed.name)
	}

	reaches := map[funcKey]bool{seed: true}
	for changed := true; changed; {
		changed = false
		for _, f := range all {
			if reaches[f.key] {
				continue
			}
			for _, c := range f.calls {
				if reaches[c] {
					reaches[f.key] = true
					changed = true
					break
				}
			}
		}
	}

	var found []multiBackendTest
	for _, f := range all {
		if f.isTst && reaches[f.key] && strings.HasPrefix(f.file, "plugins/") {
			found = append(found, multiBackendTest{name: f.key.name, file: f.file})
		}
	}
	crossCheckAgainstAnIdentifierScan(t, files, reaches)
	return found
}

// crossCheckAgainstAnIdentifierScan is the second, deliberately different
// reading: it asks which files MENTION the seed, and requires the closure to
// have attributed a function in each of them.
//
// Its job is to disagree. The closure answers "is this consistent with itself";
// it cannot answer "what am I not looking at", and a dropped edge -- a method
// value, an interface, a reference from a var initializer rather than a
// function body -- looks exactly like a test that does not use a backend set.
// A closure that runs clean and a second reading that finds nothing more is
// evidence; a closure alone is a claim.
//
// It scans for the name as an IDENTIFIER, and the first version of this scanned
// the file text instead. That version failed on its first run, naming
// engine/plugindb_dialect_guard_test.go -- which holds the name in a comment
// and inside strings.Contains(src, "NewPluginTestBackends"), because it is
// another guard that text-scans for the same thing. A text search cannot tell a
// thing from a sentence about the thing, which is this repo's most-recorded
// trap, and the fix is not an exclusion list. An exclusion would have to name
// THIS file too, and would then be the mechanism by which a future real miss
// gets waved through. Asking about identifiers drops both files out on their
// merits.
func crossCheckAgainstAnIdentifierScan(t *testing.T, files []goFile, reaches map[funcKey]bool) {
	t.Helper()

	accounted := map[string]bool{}
	for _, f := range files {
		for _, d := range f.ast.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			if reaches[funcKey{pkg: f.pkgPath, name: fd.Name.Name}] {
				accounted[f.rel] = true
			}
		}
	}

	var unaccounted []string
	for _, f := range files {
		if accounted[f.rel] {
			continue
		}
		mentions := false
		ast.Inspect(f.ast, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == seedName {
				mentions = true
				return false
			}
			return true
		})
		if mentions {
			unaccounted = append(unaccounted, f.rel)
		}
	}
	if len(unaccounted) > 0 {
		sort.Strings(unaccounted)
		t.Errorf("%d file(s) name %s as an identifier, and the reachability closure "+
			"attributed no function in them:\n\n  %s\n\n"+
			"The closure dropped an edge. It follows a call only when the callee resolves "+
			"through a package identifier, so a method value, an interface, or a reference "+
			"outside any function body is invisible to it -- and every one of those makes "+
			"this guard UNDER-report, which is the direction that makes it pass a tree it "+
			"should fail. Extend the closure; do not add an exclusion.",
			len(unaccounted), seedName, strings.Join(unaccounted, "\n  "))
	}
}

type funcKey struct{ pkg, name string }

type goFile struct {
	rel     string
	pkgPath string
	ast     *ast.File
	alias   map[string]string // package identifier -> import path
}

// references returns every function this body can reach in one step, resolved
// through the file's imports. A selector whose left side is a package
// identifier resolves to that package; a bare identifier resolves to this file's
// own package. Anything else -- a method on a value, an interface call -- is not
// resolvable here and is left out; crossCheckAgainstAnIdentifierScan is what
// notices when that matters.
func (f goFile) references(body *ast.BlockStmt) []funcKey {
	var out []funcKey
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok {
				if p, ok := f.alias[id.Name]; ok {
					out = append(out, funcKey{pkg: p, name: x.Sel.Name})
					// Do not descend: the left side is a package name, not a
					// value, and recording it as a same-package reference would
					// manufacture edges.
					return false
				}
			}
		case *ast.Ident:
			out = append(out, funcKey{pkg: f.pkgPath, name: x.Name})
		}
		return true
	})
	return out
}

// parseTrackedGoFiles parses every .go file in the repo that is not ignored.
//
// git ls-files rather than filepath.Walk: .claude/worktrees/ holds whole extra
// copies of this repo, and attributing a test to a scratch checkout is a scope
// mistake that gets MORE likely as the working tree gets messier (#749).
//
// --others --exclude-standard is not decoration. A plain `git ls-files` sees
// only what is STAGED OR COMMITTED, so a newly written test file is invisible
// to this guard until it is added -- and invisible means "reports fine", the
// permissive direction. Measured 2026-09-10: a falsification that dropped a new
// file into plugins/ to make the cross-check fire did nothing at all, because
// the file was untracked. The flags keep the worktree exclusion, since
// .gitignore carries .claude/worktrees/ and --exclude-standard honours it.
func parseTrackedGoFiles(t *testing.T, root string) []goFile {
	t.Helper()

	out, err := exec.Command("git", "-C", root, "ls-files",
		"--cached", "--others", "--exclude-standard", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	rels := strings.Fields(string(out))
	if len(rels) == 0 {
		t.Fatal("git ls-files matched no .go files")
	}
	sort.Strings(rels)

	mod := modulePath(t, root)
	pkgPathOf := func(rel string) string {
		dir := filepath.Dir(rel)
		if dir == "." {
			return mod
		}
		return mod + "/" + filepath.ToSlash(dir)
	}

	fset := token.NewFileSet()
	parsed := make([]*ast.File, len(rels))
	for i, rel := range rels {
		f, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		parsed[i] = f
	}

	// The package name a dir DECLARES, so an import resolves to the right
	// identifier even where it differs from the directory name. Test packages
	// are skipped: nothing imports a _test package.
	declared := map[string]string{}
	for i, rel := range rels {
		name := parsed[i].Name.Name
		if strings.HasSuffix(name, "_test") {
			continue
		}
		if _, seen := declared[pkgPathOf(rel)]; !seen {
			declared[pkgPathOf(rel)] = name
		}
	}

	files := make([]goFile, len(rels))
	for i, rel := range rels {
		alias := map[string]string{}
		for _, imp := range parsed[i].Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			switch {
			case imp.Name != nil:
				alias[imp.Name.Name] = path
			case declared[path] != "":
				alias[declared[path]] = path
			default:
				seg := path
				if j := strings.LastIndex(seg, "/"); j >= 0 {
					seg = seg[j+1:]
				}
				alias[seg] = path
			}
		}
		files[i] = goFile{rel: rel, pkgPath: pkgPathOf(rel), ast: parsed[i], alias: alias}
	}
	return files
}

func modulePath(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	t.Fatal("no module line in go.mod")
	return ""
}

type selector struct {
	re   *regexp.Regexp
	src  string
	expr string
}

func selectorSources(ss []selector) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.src+": -run "+s.expr)
	}
	return out
}

func matchesAnySelector(name string, ss []selector) bool {
	for _, s := range ss {
		if s.re.MatchString(name) {
			return true
		}
	}
	return false
}

var runPattern = regexp.MustCompile(`-run[= ]+'([^']+)'|-run[= ]+"([^"]+)"|-run[= ]+([^\s]+)`)

// pluginsSelectors returns the -run patterns of every workflow command that
// runs ./plugins/....
//
// Continuations are joined and comments dropped BEFORE the command is examined.
// A `-run` on a continuation line otherwise reads as absent, and absent means
// "selects everything" -- which is the permissive direction, so it would make
// this guard pass a tree it should fail (#749 shipped exactly that bug three
// times). A `#` line mentioning go test otherwise reads as an invocation (#748,
// the over-reporting mirror).
func pluginsSelectors(t *testing.T, root string) []selector {
	t.Helper()

	dir := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	var out []selector
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, cmd := range joinedCommands(string(b)) {
			if !strings.Contains(cmd, "./plugins/...") || !strings.Contains(cmd, "go test") {
				continue
			}
			m := runPattern.FindStringSubmatch(cmd)
			if m == nil {
				// Unfiltered: every multi-backend test is selected here, so
				// the contract is satisfied by construction. Represent that
				// rather than dropping the command, or an unfiltered run would
				// look like no run at all.
				out = append(out, selector{re: regexp.MustCompile(``), src: e.Name(), expr: "(unfiltered)"})
				continue
			}
			expr := m[1] + m[2] + m[3]
			re, err := regexp.Compile(expr)
			if err != nil {
				t.Fatalf("%s: -run %q does not compile: %v", e.Name(), expr, err)
			}
			out = append(out, selector{re: re, src: e.Name(), expr: expr})
		}
	}
	return out
}

// joinedCommands splits a workflow into logical shell commands: comments
// removed, backslash-continuations joined.
func joinedCommands(src string) []string {
	var cmds []string
	var cur strings.Builder
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasSuffix(trimmed, `\`) {
			cur.WriteString(strings.TrimSuffix(trimmed, `\`))
			cur.WriteString(" ")
			continue
		}
		cur.WriteString(trimmed)
		cmds = append(cmds, cur.String())
		cur.Reset()
	}
	if cur.Len() > 0 {
		cmds = append(cmds, cur.String())
	}
	return cmds
}

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// TestTheDetectionFollowsAHelper is the known-positive for the closure, and it
// is deliberately not expressed as "the count went up".
//
// TestEveryArmRunsOnItsOwnDialect in plugins/blobstore reaches
// NewPluginTestBackends ONLY through plugintest.RunEveryArm. It is the case the
// first version of this guard could not see, and a count of 12 where there had
// been 6 would say the number moved without saying which name arrived -- the
// mistake #755 records, where a widened pattern fixed one test and left its
// sibling selected by nothing.
//
// So this names the test, and it fails if the detection ever regresses to
// scanning a function's own body.
func TestTheDetectionFollowsAHelper(t *testing.T) {
	root := repoRoot(t)

	const (
		wantName = "TestEveryArmRunsOnItsOwnDialect"
		wantFile = "plugins/blobstore/blobstore_dialect_arms_multidb_test.go"
	)

	for _, tc := range multiBackendTests(t, root) {
		if tc.name == wantName && tc.file == wantFile {
			return
		}
	}
	t.Fatalf("%s (%s) is not in the multi-backend census.\n\n"+
		"It calls plugintest.RunEveryArm, which calls NewPluginTestBackends. If this "+
		"fails, the detection stopped following helper calls, and every test that reaches "+
		"a backend set indirectly has silently left the population this guard protects -- "+
		"which is exactly the state cleat#1203 found, with six such tests in the tree and "+
		"the guard green.", wantName, wantFile)
}
