package engine_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// A PluginDB adapter built without a Dialect silently rewrites nothing.
//
// cleat#1133. engine.SQLDBAdapter and engine.ReadOnlyDB put every statement
// through plugin.Rebind on the way to the driver, which is what lets a plugin
// write the primary dialect once. Rebind on the zero Dialect returns the query
// untouched -- correct for PostgreSQL, and for MySQL or SQL Server it means the
// adapter does nothing at all while looking exactly like one that works.
//
// THAT IS NOT A HYPOTHETICAL FAILURE MODE, IT IS THE STATE THIS GUARD WAS
// WRITTEN IN. Every multi-backend plugin test in the tree constructed
// `&engine.SQLDBAdapter{DB: be.DB}` and dropped `be.Dialect`, which was sitting
// on the same struct. So the entire multi-dialect plugin suite would have
// passed identically whether the adapter rewrote statements or not -- it could
// not have told the two apart. tests/plugin-harness/harness.go had the same
// omission with the dialect in its own function signature.
//
// WHERE A ZERO DIALECT IS FINE, AND WHY THIS GUARD IS NOT UNIVERSAL. There are
// ~235 adapter constructions in the tree and nearly all are single-backend
// PostgreSQL fixtures, where no rewrite is the right answer. Requiring the
// field everywhere would be a sweep that teaches people to type `Dialect:
// DialectPostgres` without meaning it. The guard therefore covers the three
// places where omitting it is definitely wrong:
//
//   - non-test code, which runs against whatever backend the operator has
//   - test files that use NewPluginTestBackends, which by construction run
//     against MySQL and SQL Server too
//   - test files that open a MySQL or SQL Server *sql.DB directly (their own
//     sql.Open call names the driver), even when they never loop over all
//     three dialects. cleat#2259/#2280's review found exactly this gap:
//     plugins/auditlog/audit_insert_dialect_test.go opens its own MySQL
//     connection and never went near NewPluginTestBackends, so the version
//     of this guard that only recognised that helper passed it clean while
//     its SQLDBAdapter{DB: db} -- no Dialect -- silently sent every
//     statement to MySQL still carrying $N text.
func TestEveryDialectSensitiveAdapterCarriesItsDialect(t *testing.T) {
	files := trackedGoFiles(t)
	if len(files) < 500 {
		t.Fatalf("only %d tracked .go files; the file list is broken and this "+
			"guard asserts nothing", len(files))
	}

	var offenders []string
	checked := 0
	for _, path := range files {
		src, err := readFile(path)
		if err != nil {
			continue
		}
		isTest := strings.HasSuffix(path, "_test.go")
		if isTest && !isDialectSensitiveTestFile(src) {
			continue // single-backend (or non-MySQL/MSSQL) fixture: a zero Dialect is correct
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			name := adapterTypeName(lit.Type)
			if name != "SQLDBAdapter" && name != "ReadOnlyDB" {
				return true
			}
			checked++
			for _, el := range lit.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Dialect" {
					return true
				}
			}
			offenders = append(offenders,
				fset.Position(lit.Pos()).String()+"  "+name+" built without a Dialect")
			return true
		})
	}
	if checked == 0 {
		t.Fatal("found no SQLDBAdapter/ReadOnlyDB constructions at all; the " +
			"AST match is broken and a pass here means nothing")
	}
	if len(offenders) > 0 {
		t.Errorf("%d dialect-sensitive adapter(s) built without a Dialect. Rebind on the "+
			"zero Dialect is a no-op, so these rewrite nothing on MySQL and SQL Server "+
			"while passing every PostgreSQL test:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
	t.Logf("checked %d dialect-sensitive adapter constructions across %d tracked files",
		checked, len(files))
}

// isDialectSensitiveTestFile reports whether path's source is one of the
// places TestEveryDialectSensitiveAdapterCarriesItsDialect's doc comment
// above describes -- a multi-backend loop (NewPluginTestBackends), a file
// that opens a MySQL or SQL Server *sql.DB directly, or a file that gets one
// via testutil.TestDB(t, testutil.DialectMySQL) (or DialectMSSQL) without
// ever containing a literal sql.Open("mysql"/... call of its own (cleat#2286).
//
// Matched as testutil.DialectMySQL/testutil.DialectMSSQL specifically, not
// the bare constant name: plugin.DialectMySQL/plugin.DialectMSSQL are the
// same strings (TestDialectConstantsAgree) but are also how a plugin names
// the dialect it is testing against an in-memory fake connector, with no
// real MySQL/MSSQL server anywhere in reach -- plugins/kvstore/kvstore_new_test.go
// and plugins/ratelimiter/ratelimiter_test.go do exactly this, and a bare
// match on the constant name flags both as false positives (checked by
// running the widened guard against the tree before qualifying it: it found
// those two and only those two). testutil.Dialect exists for one purpose --
// naming the backend a real *sql.DB in this package talks to -- so the
// qualified form carries none of that ambiguity.
func isDialectSensitiveTestFile(src string) bool {
	if strings.Contains(src, "NewPluginTestBackends") {
		return true
	}
	for _, driver := range []string{`sql.Open("mysql"`, `sql.Open("sqlserver"`, `sql.Open("mssql"`} {
		if strings.Contains(src, driver) {
			return true
		}
	}
	for _, ref := range []string{"testutil.DialectMySQL", "testutil.DialectMSSQL"} {
		if strings.Contains(src, ref) {
			return true
		}
	}
	return false
}

// The guard must be able to see a violation. Without this, every broken
// version of the scan above also passes.
func TestTheDialectGuardCanSeeAViolation(t *testing.T) {
	const bad = `package x
import "github.com/cleat-team/cleat/engine"
func f(db any) { _ = &engine.SQLDBAdapter{DB: db} }
`
	const good = `package x
import "github.com/cleat-team/cleat/engine"
func f(db any) { _ = &engine.SQLDBAdapter{DB: db, Dialect: "mssql"} }
`
	count := func(src string) int {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "x.go", src, 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		n := 0
		ast.Inspect(f, func(node ast.Node) bool {
			lit, ok := node.(*ast.CompositeLit)
			if !ok || adapterTypeName(lit.Type) != "SQLDBAdapter" {
				return true
			}
			for _, el := range lit.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Dialect" {
						return true
					}
				}
			}
			n++
			return true
		})
		return n
	}
	if got := count(bad); got != 1 {
		t.Errorf("the guard does not flag a missing Dialect: got %d, want 1", got)
	}
	if got := count(good); got != 0 {
		t.Errorf("the guard flags a present Dialect: got %d, want 0", got)
	}
}

// TestDialectSensitiveTestFileDetectsDirectMySQLOpen is a known-positive for
// the exact gap cleat#2259/#2280's review found: a _test.go file that opens
// its own MySQL (or SQL Server) *sql.DB and never goes near
// NewPluginTestBackends is just as dialect-sensitive as a multi-backend loop,
// and the version of isDialectSensitiveTestFile that only recognised
// NewPluginTestBackends excluded it -- so plugins/auditlog/
// audit_insert_dialect_test.go's SQLDBAdapter{DB: db} with no Dialect passed
// this guard clean while silently sending $N-shaped statements straight to
// MySQL.
func TestDialectSensitiveTestFileDetectsDirectMySQLOpen(t *testing.T) {
	directMySQL := `package x
import "database/sql"
func f() { db, _ := sql.Open("mysql", "dsn") }
`
	directMSSQL := `package x
import "database/sql"
func f() { db, _ := sql.Open("sqlserver", "dsn") }
`
	postgresOnly := `package x
import "database/sql"
func f() { db, _ := sql.Open("postgres", "dsn") }
`
	if !isDialectSensitiveTestFile(directMySQL) {
		t.Error("a direct sql.Open(\"mysql\", ...) test file was not flagged as dialect-sensitive")
	}
	if !isDialectSensitiveTestFile(directMSSQL) {
		t.Error("a direct sql.Open(\"sqlserver\", ...) test file was not flagged as dialect-sensitive")
	}
	if isDialectSensitiveTestFile(postgresOnly) {
		t.Error("a postgres-only fixture, with no NewPluginTestBackends and no MySQL/MSSQL open, was flagged as dialect-sensitive")
	}
}

// TestDialectSensitiveTestFileDetectsHelperDialectReference is the
// known-positive for cleat#2286: a _test.go file that gets its MySQL or SQL
// Server *sql.DB through testutil.TestDB(t, testutil.DialectMySQL) names the
// dialect constant without ever containing a literal sql.Open("mysql"/...
// call, and the pre-#2286 guard did not flag it even though it goes on to
// build an SQLDBAdapter/ReadOnlyDB with no Dialect.
//
// The negative case is the reason the match is qualified rather than bare:
// plugin.DialectMSSQL is the same string but names the dialect a plugin is
// testing against a fake, in-process connector -- kvstore_new_test.go and
// ratelimiter_test.go both do this, own no Dialect on their SQLDBAdapter,
// and reach no real MySQL/MSSQL server for it to matter. A bare match on the
// constant name flagged both as offenders when this was checked against the
// tree; the qualified match does not.
func TestDialectSensitiveTestFileDetectsHelperDialectReference(t *testing.T) {
	viaTestutilHelper := `package x
import "github.com/cleat-team/cleat/engine/testutil"
func f(t *testing.T) { db := testutil.TestDB(t, testutil.DialectMySQL) }
`
	viaPluginConstantOnFakeConnector := `package x
import "github.com/cleat-team/cleat/plugin"
func f(dialect plugin.Dialect) { _ = dialect == plugin.DialectMSSQL }
`
	if !isDialectSensitiveTestFile(viaTestutilHelper) {
		t.Error("a file referencing testutil.DialectMySQL through TestDB was not flagged as dialect-sensitive")
	}
	if isDialectSensitiveTestFile(viaPluginConstantOnFakeConnector) {
		t.Error("a file referencing plugin.DialectMSSQL alone (no testutil helper, no sql.Open) " +
			"was flagged as dialect-sensitive -- this is the kvstore/ratelimiter false-positive shape")
	}
}

// engine/testutil cannot import plugin -- engine's and plugin's own tests
// import testutil, so that would be an import cycle in the test binary. The
// dialect type is therefore declared twice, and the multi-backend tests convert
// with plugin.Dialect(be.Dialect). That conversion is only correct while the
// two sets of constants carry the same strings.
//
// Compare the SETS rather than the counts: two enumerations agreeing on a total
// while differing on members is a failure this repository has met more than
// once.
func TestDialectConstantsAgree(t *testing.T) {
	tu := map[string]testutil.Dialect{
		"postgres": testutil.DialectPostgres,
		"mysql":    testutil.DialectMySQL,
		"mssql":    testutil.DialectMSSQL,
	}
	pl := map[string]plugin.Dialect{
		"postgres": plugin.DialectPostgres,
		"mysql":    plugin.DialectMySQL,
		"mssql":    plugin.DialectMSSQL,
	}
	for k, v := range tu {
		if string(v) != k {
			t.Errorf("testutil.Dialect for %q is %q", k, v)
		}
		if string(pl[k]) != string(v) {
			t.Errorf("dialect %q: testutil=%q plugin=%q -- plugin.Dialect(be.Dialect) "+
				"is a silent mis-conversion", k, v, pl[k])
		}
	}
}

func adapterTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return t.Sel.Name
	}
	return ""
}

// trackedGoFiles uses git ls-files rather than a filesystem walk: this repo
// keeps a dozen worktrees under it, and a walk attributes their contents to
// the main tree -- a scope error that makes a guard MORE likely to pass as the
// working tree gets messier.
func trackedGoFiles(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", repoRootForGuard(t), "ls-files", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var files []string
	for _, l := range strings.Split(string(out), "\n") {
		if l != "" {
			files = append(files, repoRootForGuard(t)+"/"+l)
		}
	}
	return files
}

func repoRootForGuard(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func readFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}
