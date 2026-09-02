package testutil

// This guard pins a fact the MySQL and SQL Server CI jobs depend on, and which
// nothing else checks: `engine` is the only package that blanket-wipes a MySQL
// or SQL Server database.
//
// It exists because the fact had already been recorded wrongly. Both jobs in
// .github/workflows/multi-db-ci.yml carried `-p 1` justified by a comment
// naming "three database-backed packages -- engine, engine/testutil and
// migration -- against one database". Measured 2026-08-31, none of that was
// true of those two dialects: engine/testutil's DB-backed tests are all
// PostgreSQL and skip in a job with no PostgreSQL, and every MySQL and SQL
// Server test in `migration` creates a scratch database of its own
// (newMySQLScratchDB, newMSSQLScratchDB) and drops it again. Only `engine`
// touches the shared `cleat` database there.
//
// A comment cannot hold that. The next package to add a
// CleanupMySQLTestData call would silently make the CI comment true and
// nothing would say so -- which is how the comment came to be wrong in the
// first place. This test is the mechanism: it fails the moment the set of
// packages changes, and its failure message says what has to happen next.
//
// PostgreSQL is deliberately not covered. CleanupPostgresTestData has a
// per-suite database available to it (SuiteTestDB, packagedb.go), so a second
// package calling it is a manageable situation rather than a new hazard. The
// non-PostgreSQL dialects have no such escape hatch yet, which is exactly why
// the count of callers matters.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// blanketNonPostgresCleanups are the helpers that issue an unqualified DELETE
// across every table of a MySQL or SQL Server database. CleanupAllTestData is
// included because it dispatches on dialect, so calling it is calling one of
// the other two.
//
// CleanupTestData is not here: it is scoped to a single run ID.
var blanketNonPostgresCleanups = []string{
	"CleanupMySQLTestData",
	"CleanupMSSQLTestData",
	"CleanupAllTestData",
}

// blanketCleanupCallers is the set of package directories, relative to the
// repo root, permitted to call them. Adding to this list is a deliberate act
// with a CI consequence -- see the failure message.
var blanketCleanupCallers = map[string]bool{
	"engine":          true,
	"engine/testutil": true, // the package that defines them, for its own coverage
}

func TestBlanketNonPostgresCleanupHasOneCaller(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root not found at %s: %v", root, err)
	}

	wanted := make(map[string]bool, len(blanketNonPostgresCleanups))
	for _, name := range blanketNonPostgresCleanups {
		wanted[name] = true
	}

	fset := token.NewFileSet()
	// found records which allowed callers were actually seen, so a helper that
	// stops being called anywhere fails too rather than leaving a stale
	// allowlist entry that reads as coverage.
	found := map[string]bool{}
	var findings []string

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			// ".claude" holds agent worktrees -- entire copies of this repo,
			// each with its own engine/*_test.go. Without it this guard counts
			// callers that live in a copy of the tree rather than in the tree,
			// and reports one number per worktree: measured 2026-09-02 in a
			// checkout with eight of them, 121 "callers" of which every single
			// one was under .claude/worktrees/agent-*/.
			//
			// It fails only locally, which is the bad way round -- CI has no
			// worktrees, so the guard is green in the one place anyone looks
			// and red for whoever is actually working. A guard that trips on
			// files outside the tree it guards teaches people to ignore it.
			case ".git", ".claude", "node_modules", "target", "web", "crates":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		pkgDir := filepath.ToSlash(filepath.Dir(rel))
		if pkgDir == "." {
			pkgDir = ""
		}

		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			// A file that does not parse is not this test's business; other
			// checks in the repo cover that, and failing here would turn every
			// syntax error into a confusing cleanup-guard failure.
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := calleeName(call.Fun)
			if name == "" || !wanted[name] {
				return true
			}
			// This file names all three in blanketNonPostgresCleanups as
			// string literals, not calls, so it cannot flag itself -- but it
			// is skipped anyway for the same reason schema_source_guard_test.go
			// skips itself: a guard that can trip on its own text is a guard
			// nobody can edit.
			if filepath.Base(path) == filepath.Base(thisFile) {
				return true
			}
			if blanketCleanupCallers[pkgDir] {
				found[pkgDir] = true
				return true
			}
			findings = append(findings, fset.Position(call.Pos()).String()+": "+name)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	if len(findings) > 0 {
		t.Errorf("%d call(s) to a blanket MySQL/SQL Server cleanup outside %v:\n  %s\n\n"+
			"These helpers issue an unqualified DELETE across every table of the shared "+
			"`cleat` database, and unlike the PostgreSQL path there is no per-suite database "+
			"to contain them: SuiteTestDB (packagedb.go) is PostgreSQL-only.\n\n"+
			"A second package calling one of these means two packages wiping one MySQL or SQL "+
			"Server database, which `go test` will run concurrently. Before adding a caller, "+
			"either give MySQL and SQL Server the SuiteTestDB treatment, or make the "+
			"serialisation real: the `-p 1` in .github/workflows/multi-db-ci.yml's test-mysql "+
			"and test-mssql jobs is currently retained for cost reasons, not because a "+
			"collision exists, and that comment has to change with this list.",
			len(findings), keysOf(blanketCleanupCallers), strings.Join(findings, "\n  "))
	}

	if !found["engine"] {
		t.Errorf("no package called any of %v, so this guard is measuring nothing.\n\n"+
			"Either the helpers were renamed and this list is stale, or the last caller went "+
			"away -- in which case the MySQL and SQL Server blanket wipes have no user and "+
			"should be deleted rather than guarded.", blanketNonPostgresCleanups)
	}
}

// calleeName returns the function name of a call expression, for both
// `Cleanup...(...)` and `testutil.Cleanup...(...)` forms.
func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// keysOf returns the allowlist sorted, so a failure message is the same on
// every run rather than reshuffling with Go's map iteration order.
func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
