package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A test may drop its own probe table. It may not drop a SHIPPED one and walk
// away.
//
// TestMSSQLCapabilityFollowsTheInstalledPredicate did exactly that: it dropped
// admin.rls_predicate_form to exercise the unknown-marker arm, asserted, and
// ended. Migration 075 is what creates that table, and migration.Runner only
// applies versions schema_migrations has not recorded -- so the table stayed
// dropped while schema_migrations went on reporting 75 as applied. Measured
// 2026-09-16: the test PASSED, and the next full `go test ./engine/` run
// reported 701 test failures, all tracing to "migration 075 has not been
// applied".
//
// The damage lands in the NEXT run, which is what made it expensive. CI never
// sees it -- every CI run gets a fresh database -- so it is purely a cost borne
// by anyone running the suite against a persistent one, and it presents as a
// mass failure with no relation to the change under test.
//
// Why this guard is a source scan rather than a schema check. The obvious
// check, "after the suite, is the schema intact", is what
// suite_does_not_poison_its_database_test.go does for Postgres, and it can only
// catch a poisoner that runs BEFORE it. Go orders tests within a package by
// file, and that guard's name sorts ahead of the file that poisoned this one --
// so it would have run first and reported a healthy database. An ordering-
// independent version needs a TestMain this package does not have and should
// not grow for one check. A source scan has no ordering to be wrong about.
//
// The scan reads the AST rather than the file text, deliberately. Every DROP
// here lives in a string literal, and ast.BasicLit cannot match a comment --
// engine/store_backends_procedures_test.go has two comments that quote
// `DROP FUNCTION IF EXISTS`, and a grep-based version of this guard reports
// both as findings.
func TestATestThatDropsAMigratedObjectRestoresIt(t *testing.T) {
	migrated := migratedObjectNames(t)
	if len(migrated) == 0 {
		t.Fatal("found no CREATE statements under migrations/, so this guard is " +
			"comparing against an empty set and cannot fail")
	}

	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatalf("listing test files: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no *_test.go files matched, so this guard scanned nothing")
	}

	dropRe := regexp.MustCompile(`(?i)\bDROP\s+(TABLE|VIEW|FUNCTION|PROCEDURE)\b`)
	fset := token.NewFileSet()
	scanned := 0

	for _, file := range files {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			// A file this package cannot parse is a compile error the build
			// reports; not this guard's business.
			continue
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !strings.HasPrefix(fn.Name.Name, "Test") {
				continue
			}
			scanned++
			dropped, restores := "", false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch v := n.(type) {
				case *ast.BasicLit:
					if v.Kind != token.STRING || dropped != "" {
						return true
					}
					s, err := strconv.Unquote(v.Value)
					if err != nil {
						return true
					}
					if !dropRe.MatchString(s) {
						return true
					}
					for name := range migrated {
						if containsObjectName(s, name) {
							dropped = name
							return true
						}
					}
				case *ast.SelectorExpr:
					if v.Sel.Name == "Cleanup" {
						restores = true
					}
				}
				return true
			})
			if dropped != "" && !restores {
				t.Errorf("%s: %s drops %s, which migrations/ creates, and registers no "+
					"t.Cleanup to put it back.\n"+
					"Nothing else recreates a migrated object: the runner skips any version "+
					"schema_migrations already records, so the drop outlives this run and "+
					"breaks the next one. Register the restore BEFORE the drop -- a t.Fatalf "+
					"added later jumps over a trailing one.",
					file, fn.Name.Name, dropped)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("parsed no Test functions, so this guard cannot have found anything")
	}
	t.Logf("scanned %d Test functions against %d migrated object names", scanned, len(migrated))
}

// migratedObjectNames collects what migrations/ creates.
//
// Temp tables are excluded: 075 builds a #cleat_bound_policies to replay the
// policy set, and a test dropping its own temp table is not this guard's
// subject.
func migratedObjectNames(t *testing.T) map[string]bool {
	t.Helper()
	createRe := regexp.MustCompile(
		`(?i)CREATE\s+(?:OR\s+(?:REPLACE|ALTER)\s+)?(?:TABLE|VIEW|FUNCTION|PROCEDURE)\s+` +
			`(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z_][A-Za-z0-9_.]*)`)

	names := map[string]bool{}
	var walk func(dir string)
	walk = func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			if e.IsDir() {
				walk(p)
				continue
			}
			if !strings.HasSuffix(e.Name(), ".sql") {
				continue
			}
			raw, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			// stripSQLComments lives in mssql_uuid_projection_test.go. Without it a
			// migration header quoting a CREATE counts as a definition.
			for _, m := range createRe.FindAllStringSubmatch(stripSQLComments(string(raw)), -1) {
				name := strings.Trim(m[1], ".")
				if name == "" || strings.HasPrefix(name, "#") {
					continue
				}
				names[name] = true
				if dot := strings.LastIndex(name, "."); dot >= 0 && dot+1 < len(name) {
					names[name[dot+1:]] = true
				}
			}
		}
	}
	walk(filepath.Join("..", "migrations"))
	return names
}

// containsObjectName reports whether sql names this object, matching whole
// identifiers so that `admin.rls_predicate_form` is not found inside a longer
// name and `workflow_instances` does not match `workflow_instances_archive`.
func containsObjectName(sql, name string) bool {
	idx := 0
	for {
		i := strings.Index(sql[idx:], name)
		if i < 0 {
			return false
		}
		i += idx
		beforeOK := i == 0 || !isIdentByte(sql[i-1])
		end := i + len(name)
		afterOK := end >= len(sql) || !isIdentByte(sql[end])
		if beforeOK && afterOK {
			return true
		}
		idx = i + 1
	}
}
