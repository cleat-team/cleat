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

// exemptSchemaDDLTables are the tables this package may create directly.
//
// THE EXEMPTION IS KEYED ON THE TABLE NAME, NOT ON A FILE, and that is the
// whole of what keeps this guard sharp. A file-level exemption would let a
// hand-written copy of workflow_instances land in the exempted file and say
// nothing; a name-level one means that same file is still checked for every
// other table.
//
// cleat_test_deletion_audit is cleat#982's deletion audit
// (mssql_row_disappearance.go). It is the case the paragraph above anticipated:
// a table that MUST NOT ship in a migration, because it exists only in a
// database some test explicitly installed an instrument into, and putting it
// under migrations/ would be putting a diagnostic into every production schema.
// So it cannot drift from a shipped definition -- there is no shipped
// definition to drift from, which is the property that makes the exemption
// safe and is not true of anything this guard was written for.
var exemptSchemaDDLTables = []string{
	"cleat_test_deletion_audit",
}

// TestNoHandWrittenSchema fails if any .go file in this package contains a
// string literal that defines or alters a table, other than the tables named
// in exemptSchemaDDLTables. Every other legitimate thing this package creates
// directly (PostgresRLSTestRole, the SQL Server administrative login in
// mssql_admin.go) is a ROLE or LOGIN, not a TABLE. If a future exception is
// genuinely needed, add it to exemptSchemaDDLTables deliberately rather than
// letting the check go silently softer.
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
			for _, exempt := range exemptSchemaDDLTables {
				if strings.Contains(hay, exempt) {
					return true
				}
			}
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

// TestTheSchemaGuardStillFailsForANonExemptTable is the known-positive for the
// exemption above.
//
// "No hand-written schema" is also what a guard reports when its exemption has
// swallowed everything -- an exemption is a way for a check to go quiet, and a
// quiet check reads exactly like a clean tree. This asserts the scan still
// rejects a table that is NOT on the list, using the same substring test the
// guard uses, so the two cannot come apart.
func TestTheSchemaGuardStillFailsForANonExemptTable(t *testing.T) {
	exempted := func(lit string) bool {
		hay := strings.ToLower(lit)
		for _, e := range exemptSchemaDDLTables {
			if strings.Contains(hay, e) {
				return true
			}
		}
		for _, bad := range forbiddenSchemaDDL {
			if strings.Contains(hay, bad) {
				return false
			}
		}
		return true
	}

	if exempted("CREATE TABLE workflow_instances (id NVARCHAR(200) PRIMARY KEY)") {
		t.Error("a hand-written CREATE TABLE workflow_instances is exempted; the " +
			"exemption has disarmed the guard for the tables it exists to protect")
	}
	if exempted("ALTER TABLE event_history ADD COLUMN service NVARCHAR(200) NULL") {
		t.Error("a hand-written ALTER TABLE event_history is exempted")
	}
	if !exempted("IF OBJECT_ID('dbo.cleat_test_deletion_audit','U') IS NULL " +
		"CREATE TABLE dbo.cleat_test_deletion_audit (audit_id BIGINT)") {
		t.Error("the cleat#982 audit table is not exempted, so the exemption does " +
			"nothing and mssql_row_disappearance.go cannot compile its instrument")
	}
}
