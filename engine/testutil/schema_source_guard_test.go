package testutil

// This is the guard for the defect Stream A1 fixed: engine/testutil hand-wrote
// its own copy of the MySQL and SQL Server schemas (and, for a while, a
// curated subset of the PostgreSQL one) instead of applying
// migrations/<dialect>/*.sql through migration.Runner. The two copies drifted
// -- mysql_schema.go declared event_history.service/operation/request
// nullable where 001_schema.sql declared them NOT NULL, so every MySQL test
// in the repo ran against a schema production never uses (migration 030 fixed
// the schema; this guard is what stops the *mechanism* that let it drift back
// out from under a future change).
//
// A `CREATE TABLE` anywhere in this package's Go source is that mechanism:
// this package is supposed to have no schema of its own, only the one under
// migrations/. Every dialect now goes through applyMigrations
// (migrations.go), and the sole legitimate DDL objects this package still
// creates directly are test-only artefacts no migration would ever ship --
// PostgresRLSTestRole (schema.go) and the SQL Server administrative login
// (mssql_admin.go) -- neither of which is a CREATE TABLE.
//
// Deliberately a substring match against string literals, in the same style
// as plugin/secret_field_guard_test.go's name-driven credential check: a
// literal `CREATE TABLE` is what every prior hand-written copy in this
// package looked like, and a guard that tried to be cleverer about detecting
// "a schema definition" would be easier to defeat by accident than the thing
// it is guarding against.
import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// forbiddenSchemaDDL are substrings that mean "this string literal defines a
// table", checked case-insensitively. CREATE TABLE is the one every past
// incident in this package used; ALTER TABLE ... ADD COLUMN is included too,
// because a schema patched in piecemeal (as mssql_schema.go's migrateMSSQL*
// helpers used to do, one ALTER per migration this package had not actually
// applied) is the same defect wearing a smaller diff.
var forbiddenSchemaDDL = []string{
	"create table",
	"alter table",
}

// TestNoHandWrittenSchema fails if any .go file in this package contains a
// string literal that defines or alters a table. There is no allowlist:
// every legitimate thing this package still creates directly
// (PostgresRLSTestRole, the SQL Server administrative login in
// mssql_admin.go) is a ROLE or LOGIN, not a TABLE, so none of it should ever
// need one. If a future exception is genuinely needed, add it deliberately
// here rather than letting the check go silently softer.
func TestNoHandWrittenSchema(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	self := filepath.Base(thisFile)

	fset := token.NewFileSet()
	var findings []string

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		if entry.Name() == self {
			// This file's own forbiddenSchemaDDL literals ("create table",
			// "alter table") would otherwise flag themselves.
			continue
		}
		path := filepath.Join(dir, entry.Name())
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			hay := strings.ToLower(lit.Value)
			for _, bad := range forbiddenSchemaDDL {
				if strings.Contains(hay, bad) {
					findings = append(findings, fset.Position(lit.Pos()).String()+
						": string literal contains "+strconv.Quote(bad))
				}
			}
			return true
		})
	}

	if len(findings) > 0 {
		t.Errorf("%d string literal(s) in engine/testutil define or alter a table:\n  %s\n\n"+
			"engine/testutil applies migrations/<dialect>/*.sql through migration.Runner "+
			"(migrations.go) rather than carrying its own copy of the schema -- that is the fix "+
			"for the tests-do-not-run-against-the-schema-that-ships class of defect (mysql_schema.go "+
			"used to declare event_history's call columns nullable where the shipped schema said "+
			"NOT NULL, and every MySQL test ran against the wrong one). If a test genuinely needs a "+
			"table no migration creates, that is a finding about the migration or the test, not a "+
			"reason to hand-write one here.",
			len(findings), strings.Join(findings, "\n  "))
	}
}
