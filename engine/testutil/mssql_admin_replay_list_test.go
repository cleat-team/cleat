package testutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMSSQLPlainPredicateReplayFilesMatchesTheRealTree is the anti-vacuity
// check: the derivation must agree with the one case whose right answer is
// already known. cleat#3171 added migrations/mssql/013, which redefines
// finalize_workflow_status -- a routine 003_procedures.sql also defines --
// and nothing else in the tree redefines a 003-bundled routine (checked
// 2026-10-06; re-derive with the command TestProcedureMigrationListsAreComplete's
// own sibling check uses, grepping migrations/mssql/*.sql for
// finalize_workflow_status/fn_tenant_filter/drop_tenant). If this starts
// failing because a later migration legitimately redefines a bundled
// routine, the fix is to update the want list here, not the derivation.
func TestMSSQLPlainPredicateReplayFilesMatchesTheRealTree(t *testing.T) {
	root := repoRootForMSSQLTestutil(t)
	dir := filepath.Join(root, "migrations", "mssql")

	got, err := mssqlPlainPredicateReplayFiles(dir)
	if err != nil {
		t.Fatalf("mssqlPlainPredicateReplayFiles(%s): %v", dir, err)
	}

	want := []string{
		"002_defaults.sql",
		"003_procedures.sql",
		"013_a_promise_resolved_mid_segment_wakes_the_workflow.sql",
	}
	if len(got) != len(want) {
		t.Fatalf("mssqlPlainPredicateReplayFiles(%s) = %v, want %v", dir, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mssqlPlainPredicateReplayFiles(%s) = %v, want %v", dir, got, want)
		}
	}
}

// TestMSSQLPlainPredicateReplayFilesCatchesALaterRoutineRedefinition is the
// mechanism test: a synthetic tree, standing in for "the next migration
// cleat#3173 was filed to anticipate", proving the derivation includes a
// later file that redefines a bundled routine, excludes one that does not,
// and is not fooled by a redefinition that appears only in a comment -- the
// same "text search cannot tell a thing from a sentence about the thing"
// trap engine/procedure_migration_list_test.go guards against, checked here
// independently because that file's stripper is not reachable from this
// package.
func TestMSSQLPlainPredicateReplayFilesCatchesALaterRoutineRedefinition(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	write("002_defaults.sql", "-- the MERGE that seeds admin.rls_predicate_form; irrelevant here\n")
	write("003_procedures.sql", `
CREATE PROCEDURE admin.drop_tenant
AS
BEGIN
    SELECT 1;
END;
CREATE OR ALTER FUNCTION dbo.fn_tenant_filter(@tenant_id UNIQUEIDENTIFIER)
RETURNS TABLE
AS RETURN SELECT 1 AS fn_result;
`)
	write("004_unrelated_data_migration.sql", `
UPDATE workflow_instances SET priority = 0 WHERE priority IS NULL;
`)
	write("005_redefines_drop_tenant.sql", `
-- cleat#9999 (synthetic): drop_tenant now also clears a cache.
CREATE OR ALTER PROCEDURE admin.drop_tenant
AS
BEGIN
    SELECT 2;
END;
`)
	write("006_only_mentions_drop_tenant_in_a_comment.sql", `
-- admin.drop_tenant already handles this; see 005.
-- CREATE OR ALTER PROCEDURE admin.drop_tenant
SELECT 1;
`)
	write("007_defines_an_unbundled_routine.sql", `
CREATE PROCEDURE admin.some_other_routine
AS
BEGIN
    SELECT 3;
END;
`)

	got, err := mssqlPlainPredicateReplayFiles(dir)
	if err != nil {
		t.Fatalf("mssqlPlainPredicateReplayFiles(%s): %v", dir, err)
	}

	want := []string{
		"002_defaults.sql",
		"003_procedures.sql",
		"005_redefines_drop_tenant.sql",
	}
	if len(got) != len(want) {
		t.Fatalf("mssqlPlainPredicateReplayFiles(synthetic tree) = %v, want %v\n\n"+
			"004 touches no bundled routine, 006 mentions one only in a comment, and 007 "+
			"redefines a routine 003 does not define -- none of the three should appear",
			got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mssqlPlainPredicateReplayFiles(synthetic tree)[%d] = %q, want %q (full: %v)",
				i, got[i], want[i], got)
		}
	}
}

// TestMSSQLStripSQLCommentsDefeatsACommentedOutRedefinition is the narrower,
// string-level version of the same check: a CREATE statement commented out
// with -- or wrapped in /* */ must not be seen as a definition, and a real
// one, including one on the same line text a comment could plausibly
// contain, must still be seen.
func TestMSSQLStripSQLCommentsDefeatsACommentedOutRedefinition(t *testing.T) {
	for _, tc := range []struct {
		name  string
		sql   string
		match bool
	}{
		{"real procedure", "CREATE PROCEDURE admin.drop_tenant\nAS SELECT 1;", true},
		{"real function, OR ALTER", "CREATE OR ALTER FUNCTION dbo.fn_tenant_filter(@t UNIQUEIDENTIFIER)\nRETURNS TABLE", true},
		{"line comment", "-- CREATE PROCEDURE admin.drop_tenant\nSELECT 1;", false},
		{"block comment", "/*\nCREATE PROCEDURE admin.drop_tenant\n*/\nSELECT 1;", false},
		{"discussed in prose", "-- 003 defines admin.drop_tenant as a two-step delete.\nSELECT 1;", false},
		{"named without defining", "EXEC admin.drop_tenant @tenant_id;", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mssqlRoutineDefRe.MatchString(mssqlStripSQLComments(tc.sql))
			if got != tc.match {
				t.Errorf("match = %v, want %v, for:\n%s", got, tc.match, tc.sql)
			}
		})
	}
}

// TestMSSQLAdminDBRestoreSurvivesTheRealFinalizeWorkflowStatusRedefinition is
// cleat#3173's own regression test, using cleat#3171's real incident as its
// fixture rather than a synthetic one -- the literal scenario that found this
// gap: the admin pool's refcount drops to zero (a subtest's Cleanup runs),
// restoreMSSQLPlainPredicate replays its list, and finalize_workflow_status
// must still read the POST-013 body afterward, not the pre-013 one 003 alone
// would restore.
func TestMSSQLAdminDBRestoreSurvivesTheRealFinalizeWorkflowStatusRedefinition(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	db := MSSQLTestDB(t)
	SetupMSSQLFullSchema(t, db)

	definitionContainsPromiseSeq := func(t *testing.T) bool {
		t.Helper()
		var def string
		if err := db.QueryRow(
			`SELECT OBJECT_DEFINITION(OBJECT_ID('dbo.finalize_workflow_status'))`,
		).Scan(&def); err != nil {
			t.Fatalf("read finalize_workflow_status's definition: %v", err)
		}
		return strings.Contains(def, "promise_seq")
	}

	if !definitionContainsPromiseSeq(t) {
		t.Fatalf("finalize_workflow_status does not mention promise_seq right after " +
			"SetupMSSQLFullSchema -- migrations/mssql/013 is not what this database was built " +
			"from, so this test cannot exercise the scenario it names")
	}

	// A subtest that acquires the admin pool and releases it (its Cleanup
	// runs before this t.Run call returns) -- the exact lifecycle cleat#3171
	// hit: the FIRST MSSQL caller in the process to touch this path.
	t.Run("a subtest that calls MSSQLAdminDB and releases it", func(t *testing.T) {
		_ = MSSQLAdminDB(t, db)
	})

	// restoreMSSQLPlainPredicate has now run. Before cleat#3173's fix it
	// replayed 002+003 only, and 003 alone carries finalize_workflow_status's
	// PRE-013 body -- so this is exactly the assertion that failed, reverted,
	// mid-suite, the first time cleat#3171 hit it.
	if !definitionContainsPromiseSeq(t) {
		t.Fatalf("finalize_workflow_status reverted to its pre-013 body after the admin pool's " +
			"last caller released it -- restoreMSSQLPlainPredicate's replay list is missing a " +
			"later migration that redefines a routine 003_procedures.sql also defines (cleat#3171's " +
			"own regression, which cleat#3173 exists to prevent happening again for any OTHER routine)")
	}
}
