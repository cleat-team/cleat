package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Every migration entry point the worker uses has to be told the configured
// schema: migration.NewRunner for core migrations, plugin.RunMigrations for
// plugin ones. cleat#1287.
//
// WHY A GUARD FOR TWO CALL SITES. WithSchema defaults to public when it is not
// called, which is deliberate -- the twenty-odd NewRunner call sites in tests
// want exactly that, and making it a required parameter would churn all of
// them to say "public" out loud. The cost of that choice is that forgetting
// the call is SILENT, and what it silently restores is the defect this whole
// change exists to remove: migrations building into public while the runtime
// pool, opened through dsnWithSchema, looks in --schema.
//
// A default that leans public is precisely the shape of the original bug.
// Leaving one behind without a check would be repeating it one layer up.
func TestEveryMigrationRunnerGetsTheConfiguredSchema(t *testing.T) {
	fset := token.NewFileSet()
	examined, checked := 0, 0

	for _, path := range trackedGoFiles(t) {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		examined++
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch {
			case isSelector(call.Fun, "migration", "NewRunner"):
				checked++
				// The result must be the receiver of a .WithSchema(...) call.
				// ast.Inspect walks outside-in, so the enclosing chain is not
				// available here; find it by looking for a WithSchema call
				// whose receiver is this NewRunner call instead.
				if !hasWithSchema(f, call) {
					pos := fset.Position(call.Pos())
					t.Errorf("%s:%d: migration.NewRunner(...) is not followed by "+
						".WithSchema(*schemaName).\n\n"+
						"Without it this runner builds into public while the "+
						"runtime pool looks in --schema, which is cleat#1287.",
						pos.Filename, pos.Line)
				}
			case isSelector(call.Fun, "plugin", "RunMigrations"):
				checked++
				// Here the schema arrives as a variadic option rather than a
				// chained call, so the shape to look for is an argument.
				if !hasSchemaOption(call) {
					pos := fset.Position(call.Pos())
					t.Errorf("%s:%d: plugin.RunMigrations(...) is not passed "+
						"plugin.WithSchema(*schemaName).\n\n"+
						"Without it plugin tables land in public while the "+
						"runtime pool looks in --schema, and every plugin's "+
						"first query fails. cleat#1287.",
						pos.Filename, pos.Line)
				}
			}
			return true
		})
	}

	// "0 problems" and "0 examined" are the same output. This package has had
	// a NewRunner call since before the flag existed, so zero here means the
	// scan is not reaching the file, not that the worker stopped migrating.
	if examined == 0 {
		t.Fatal("parsed no non-test Go files in this package")
	}
	// Two NewRunner calls and two RunMigrations calls as of cleat#1287; the
	// floor is deliberately "more than one of each shape" rather than a count,
	// because the population grows and a census would go stale. What it has to
	// exclude is a scan that found nothing.
	if checked < 2 {
		t.Fatalf("found %d migration entry point(s) in %d file(s); either the "+
			"worker no longer runs migrations, or this scan is looking in the "+
			"wrong place", checked, examined)
	}
}

// hasWithSchema reports whether target is the receiver of a .WithSchema call.
func hasWithSchema(f *ast.File, target *ast.CallExpr) bool {
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		outer, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := outer.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "WithSchema" {
			return true
		}
		if sel.X == ast.Expr(target) {
			found = true
			return false
		}
		return true
	})
	return found
}

// hasSchemaOption reports whether plugin.WithSchema(...) is among the call's
// arguments.
func hasSchemaOption(call *ast.CallExpr) bool {
	for _, arg := range call.Args {
		inner, ok := arg.(*ast.CallExpr)
		if ok && isSelector(inner.Fun, "plugin", "WithSchema") {
			return true
		}
	}
	return false
}

func isSelector(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == pkg
}

// trackedGoFiles lists this package's files according to git rather than the
// filesystem. .claude/worktrees/ holds whole copies of the repository, so a
// filesystem walk attributes a scratch checkout's code to this package.
func trackedGoFiles(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("git", "ls-files", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		if _, err := os.Stat(line); err != nil {
			continue
		}
		files = append(files, line)
	}
	return files
}
