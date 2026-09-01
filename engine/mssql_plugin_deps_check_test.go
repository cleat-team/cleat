package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestPluginDepsRejectsNonJSON pins migration 036.
//
// plugin_deps was the one JSON column SQL Server did not validate. PostgreSQL
// (JSONB) and MySQL (JSON) both reject invalid JSON outright, and SQL Server's
// own schema already applies ISJSON CHECK constraints to its other JSON columns
// -- ck_plugin_defs_config, ck_workflow_instances_input, and three more in
// 001_schema.sql. This column was simply missed.
//
// The gap was not academic: it is what let the []byte-to-NVARCHAR(MAX) write bug
// store mojibake in the first place. A validating column would have rejected
// that write immediately, which is exactly why the other two dialects never had
// the bug.
func TestPluginDepsRejectsNonJSON(t *testing.T) {
	// testutil.TestDB, not testutil.MSSQLTestDB. MSSQLTestDB falls back to a
	// default DSN (localhost:1433) when CLEAT_TEST_MSSQL is unset and t.Fatalf's
	// if that does not answer, so in a PostgreSQL-only job these tests would
	// FAIL rather than skip -- and on a developer machine they would silently
	// connect to whatever happens to be on 1433. TestDB makes the distinction
	// that matters: no database asked for is a skip, a configured-but-
	// unreachable one is a failure.
	db := testutil.TestDB(t, testutil.DialectMSSQL)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectMSSQL)
	testutil.CleanupMSSQLTestData(t, db)
	defer testutil.CleanupMSSQLTestData(t, db)

	ctx := context.Background()

	// Control: valid JSON is accepted. Without it, a schema that rejected
	// everything would pass the negative case below.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO workflow_defs (name, version, wasm_bytes, min_version, abi_version, plugin_deps, tenant_id)
		 VALUES ('deps-check-ok', 1, 0x0061736d, 1, 1, @p1, @p2)`,
		`{"llm":"1.2.0"}`, DefaultTenantUUID); err != nil {
		t.Fatalf("valid plugin_deps was rejected: %v", err)
	}

	_, err := db.ExecContext(ctx,
		`INSERT INTO workflow_defs (name, version, wasm_bytes, min_version, abi_version, plugin_deps, tenant_id)
		 VALUES ('deps-check-bad', 1, 0x0061736d, 1, 1, @p1, @p2)`,
		"not json at all", DefaultTenantUUID)
	if err == nil {
		t.Fatal("SQL Server accepted a non-JSON plugin_deps. The column is NVARCHAR(MAX), " +
			"so nothing but this constraint stands between it and the mojibake the " +
			"[]byte binding used to write.")
	}
	if !strings.Contains(err.Error(), "ck_workflow_defs_plugin_deps") {
		t.Errorf("rejected, but not by the expected constraint: %v", err)
	}
}

// TestPluginDepsCheckConstraintIsUntrusted pins the WITH NOCHECK decision,
// which looks like an oversight and is not.
//
// Every plugin_deps row written by SQL Server before the write fix is mangled,
// and mangled text is not JSON (measured: ISJSON returns 0 for it). A plain
// ADD CONSTRAINT validates existing rows, so it would FAIL on every existing
// deployment and block the upgrade. WITH NOCHECK enforces the rule going
// forward and leaves history alone, matching the read side's recovery path:
// log, return an empty map, self-heal on the next deploy.
//
// The cost is that SQL Server marks the constraint untrusted and will not use
// it for query optimisation. That costs nothing here -- nothing filters on
// plugin_deps -- and this test exists so that a later tidy-up to WITH CHECK,
// which would look like an improvement, cannot silently reintroduce a failed
// upgrade for anyone with pre-fix rows.
func TestPluginDepsCheckConstraintIsUntrusted(t *testing.T) {
	// testutil.TestDB rather than MSSQLTestDB -- see the note above.
	db := testutil.TestDB(t, testutil.DialectMSSQL)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectMSSQL)

	var isNotTrusted bool
	if err := db.QueryRow(`
		SELECT is_not_trusted FROM sys.check_constraints
		WHERE name = 'ck_workflow_defs_plugin_deps'
		  AND parent_object_id = OBJECT_ID('dbo.workflow_defs')
	`).Scan(&isNotTrusted); err != nil {
		t.Fatalf("reading the constraint's trust state (is it present at all?): %v", err)
	}
	if !isNotTrusted {
		t.Error("ck_workflow_defs_plugin_deps is trusted, so it was added WITH CHECK. " +
			"That validates existing rows, and every plugin_deps row written before the " +
			"[]byte-binding fix is mangled -- so the migration would fail on any existing " +
			"SQL Server deployment rather than upgrading it. See " +
			"migrations/mssql/036_plugin_deps_isjson.sql.")
	}
}
