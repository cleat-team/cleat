package engine

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine/testutil"
)

// mssqlQueryRowAsTenant is mssqlExecAsTenant's read-side sibling: this
// test's writes land under RLS, and the admin login (sa) is NOT exempt
// from the FILTER predicate -- a bare adminDB.QueryRowContext with no
// session context set sees zero rows for everything, which reads
// identically to "the write landed under the wrong tenant" unless the
// verification query ALSO claims a tenant first. Measured directly: the
// first version of this test used adminDB.QueryRowContext bare and every
// verification query that expected a surviving row failed with "sql: no
// rows in result set", including for writes that (once fixed here) land
// correctly.
func mssqlQueryRowAsTenant(ctx context.Context, t *testing.T, db *sql.DB, tenant, stmt string, args ...any) *sql.Row {
	t.Helper()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("pin a connection: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := conn.ExecContext(ctx,
		`EXEC sp_set_session_context @key=N'tenant_id', @value=@p1`, tenant,
	); err != nil {
		t.Fatalf("set the tenant session context: %v", err)
	}
	return conn.QueryRowContext(ctx, stmt, args...)
}

// mssqlScanCount runs stmt (a literal "SELECT COUNT(*) ..." at every call
// site below -- deliberately no fmt.Sprintf-built query here, even in a
// test: gosec's G701 taint tracker does not exempt _test.go files from its
// ANALYSIS, only from its reports, and a dynamically-built query string in
// this file was measured to make it also misattribute a finding onto an
// unrelated line in mssql_deployment.go).
func mssqlScanCount(ctx context.Context, t *testing.T, db *sql.DB, tenant, stmt string, args ...any) int {
	t.Helper()
	var n int
	if err := mssqlQueryRowAsTenant(ctx, t, db, tenant, stmt, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// TestMSSQLStore_WithTenantRescopedStoreReadsTheDeploymentMethods extends
// TestMSSQLStore_WithTenantRescopedStoreReadsTheNewTenantsData's coverage
// (mssql_withtenant_rescope_test.go) to the 20 mssql_deployment.go methods
// converted in this PR -- cleat#2210's largest single-file batch. Same
// mechanism, same proof shape: open a store for tenant A, re-scope it to
// tenant B with WithTenant, and check each converted method reads/writes
// B's own data, not A's silently-reused SESSION_CONTEXT.
//
// Ordered reads-before-writes, with writes using their OWN dedicated rows:
// subtests run sequentially (no t.Parallel here), so a write landing on a
// row a later subtest reads would make this test's own ordering part of
// what it's proving, which is not the property under test.
func TestMSSQLStore_WithTenantRescopedStoreReadsTheDeploymentMethods(t *testing.T) {
	dsn := os.Getenv("CLEAT_TEST_MSSQL")
	if dsn == "" {
		t.Skip("CLEAT_TEST_MSSQL not set, skipping SQL Server tests")
	}
	if testing.Short() {
		t.Skip("Skipping MSSQL integration test in short mode")
	}

	ctx := context.Background()
	adminDB := testutil.MSSQLTestDB(t)
	t.Cleanup(func() { adminDB.Close() })
	testutil.SetupMSSQLFullSchema(t, adminDB)
	testutil.CleanupMSSQLTestData(t, adminDB)
	t.Cleanup(func() { testutil.CleanupMSSQLTestData(t, adminDB) })

	const (
		tenantA = "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"
		tenantB = "bbbbbbbb-bbbb-4bbb-bbbb-bbbbbbbbbbbb"
	)
	run := uuid.New().String()[:8]

	// defName: the read-only fixture. One workflow_def, fully populated
	// (wasm bytes, dag_spec, max_history_length), belonging to B alone.
	defName := "wt-resc-dep-def-" + run
	if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
		INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, min_version, max_history_length, dag_spec, tenant_id)
		VALUES (@p1, 1, 0x0061736d, 1, 1, 500, '{"start":"step1"}', @p2)`,
		defName, tenantB); err != nil {
		t.Fatalf("seed workflow_def: %v", err)
	}

	// wfID: one active instance of defName, for CountActiveInstances and
	// ListWorkflows.
	wfID := "wt-resc-dep-wf-" + run
	if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, next_wake_at, input, task_queue, tenant_id)
		VALUES (@p1, @p2, 1, 'ready', DATEADD(DAY, -1, SYSUTCDATETIME()), '{}', 'default', @p3)`,
		wfID, defName, tenantB); err != nil {
		t.Fatalf("seed workflow_instance: %v", err)
	}

	// One tag, for GetWorkflowTag/GetWorkflowTags (read-only subtests).
	readTag := "wt-resc-dep-readtag-" + run
	if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
		INSERT INTO workflow_tags (workflow_name, version, tag, tenant_id)
		VALUES (@p1, 1, @p2, @p3)`,
		defName, readTag, tenantB); err != nil {
		t.Fatalf("seed workflow_tag: %v", err)
	}

	// One routing rule, for GetRoutingRules (read-only subtest).
	if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
		INSERT INTO workflow_routing (workflow_name, target_version, weight, tenant_id)
		VALUES (@p1, 1, 1.0, @p2)`,
		defName, tenantB); err != nil {
		t.Fatalf("seed workflow_routing: %v", err)
	}

	// defName2: a SEPARATE def, dedicated to the two mutating-def subtests
	// (MarkVersionDeprecated, PurgeWorkflowDef), so they cannot disturb
	// defName's read assertions above regardless of subtest order.
	defName2 := "wt-resc-dep-def2-" + run
	if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
		INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, min_version, tenant_id)
		VALUES (@p1, 1, 0x0061736d, 1, 1, @p2)`,
		defName2, tenantB); err != nil {
		t.Fatalf("seed workflow_def (defName2): %v", err)
	}

	enableMSSQLTenantPolicies(t, adminDB)

	factory := NewMSSQLStoreFactory(dsn)
	defer factory.Close()

	// Opening for A, not B, is the point: it is A's SESSION_CONTEXT this
	// pool carries. WithTenant below re-scopes only the Go struct.
	opened, closerA, err := factory.OpenStore(ctx, tenantA)
	if err != nil {
		t.Fatalf("OpenStore(A): %v", err)
	}
	defer closerA.Close()
	storeA, ok := opened.(*MSSQLStore)
	if !ok {
		t.Fatalf("OpenStore returned %T, want *MSSQLStore", opened)
	}
	rescoped := storeA.WithTenant(tenantB)

	// ---- Reads first ----

	t.Run("LoadWASM", func(t *testing.T) {
		got, err := rescoped.LoadWASM(ctx, defName, 1)
		if err != nil {
			t.Fatalf("LoadWASM: %v", err)
		}
		if len(got) == 0 {
			t.Fatal("LoadWASM returned no bytes -- cleat#2210: a bare s.db read runs under A's SESSION_CONTEXT and reports B's own def as not found")
		}
	})

	t.Run("GetWASMLength", func(t *testing.T) {
		got, err := rescoped.GetWASMLength(ctx, defName, 1)
		if err != nil {
			t.Fatalf("GetWASMLength: %v", err)
		}
		if got == 0 {
			t.Fatal("GetWASMLength returned 0 -- cleat#2210: under the bug this misses the row and NULL-scans to 0")
		}
	})

	t.Run("LoadWorkflowConfig", func(t *testing.T) {
		got, err := rescoped.LoadWorkflowConfig(ctx, defName, 1)
		if err != nil {
			t.Fatalf("LoadWorkflowConfig: %v", err)
		}
		if got != 500 {
			t.Fatalf("LoadWorkflowConfig = %d, want 500 -- cleat#2210: under the bug this misses the row entirely (sql.ErrNoRows)", got)
		}
	})

	t.Run("LoadDAGSpec", func(t *testing.T) {
		got, err := rescoped.LoadDAGSpec(ctx, defName, 1)
		if err != nil {
			t.Fatalf("LoadDAGSpec: %v", err)
		}
		if string(got) != `{"start":"step1"}` {
			t.Fatalf("LoadDAGSpec = %s, want %s -- cleat#2210: under the bug this misses the row entirely (sql.ErrNoRows)", got, `{"start":"step1"}`)
		}
	})

	t.Run("GetWorkflowDef", func(t *testing.T) {
		got, err := rescoped.GetWorkflowDef(ctx, defName, 1)
		if err != nil {
			t.Fatalf("GetWorkflowDef: %v", err)
		}
		if got == nil {
			t.Fatal("GetWorkflowDef returned nil -- cleat#2210: under the bug this misses the row (sql.ErrNoRows) and the no-rows branch returns (nil, nil)")
		}
	})

	t.Run("ListWorkflowDefs", func(t *testing.T) {
		got, err := rescoped.ListWorkflowDefs(ctx, defName)
		if err != nil {
			t.Fatalf("ListWorkflowDefs: %v", err)
		}
		if len(got) == 0 {
			t.Fatal("ListWorkflowDefs returned none -- cleat#2210: under the bug this reports B's own def as nonexistent")
		}
	})

	t.Run("ResolveLatestVersion", func(t *testing.T) {
		got, err := rescoped.ResolveLatestVersion(ctx, defName)
		if err != nil {
			t.Fatalf("ResolveLatestVersion: %v", err)
		}
		if got != 1 {
			t.Fatalf("ResolveLatestVersion = %d, want 1 -- cleat#2210: under the bug this reports \"no non-deprecated version found\"", got)
		}
	})

	t.Run("ValidateVersion", func(t *testing.T) {
		got, err := rescoped.ValidateVersion(ctx, defName, 1)
		if err != nil {
			t.Fatalf("ValidateVersion: %v", err)
		}
		if !got {
			t.Fatal("ValidateVersion = false, want true -- cleat#2210: under the bug this reports B's own valid version as invalid")
		}
	})

	t.Run("CountActiveInstances", func(t *testing.T) {
		got, err := rescoped.CountActiveInstances(ctx, defName, 1)
		if err != nil {
			t.Fatalf("CountActiveInstances: %v", err)
		}
		if got < 1 {
			t.Fatalf("CountActiveInstances = %d, want >= 1 (wfID %q is status='ready', tenant B's own row) -- cleat#2210: under the bug this counts under A's SESSION_CONTEXT, which has none of B's rows", got, wfID)
		}
	})

	t.Run("ListWorkflows", func(t *testing.T) {
		got, err := rescoped.ListWorkflows(ctx, WorkflowFilter{})
		if err != nil {
			t.Fatalf("ListWorkflows: %v", err)
		}
		for _, wf := range got {
			if wf.ID == wfID {
				return
			}
		}
		t.Fatalf("ListWorkflows did not return %q (got %d workflow(s)) -- cleat#2210: under the bug this reports B's own instance as nonexistent", wfID, len(got))
	})

	t.Run("GetWorkflowTag", func(t *testing.T) {
		got, err := rescoped.GetWorkflowTag(ctx, defName, readTag)
		if err != nil {
			t.Fatalf("GetWorkflowTag: %v", err)
		}
		if got != 1 {
			t.Fatalf("GetWorkflowTag = %d, want 1 -- cleat#2210: under the bug this reports \"tag not found\"", got)
		}
	})

	t.Run("GetWorkflowTags", func(t *testing.T) {
		got, err := rescoped.GetWorkflowTags(ctx, defName)
		if err != nil {
			t.Fatalf("GetWorkflowTags: %v", err)
		}
		if got[readTag] != 1 {
			t.Fatalf("GetWorkflowTags = %v, want %s=1 -- cleat#2210: under the bug this reports no tags at all", got, readTag)
		}
	})

	t.Run("GetRoutingRules", func(t *testing.T) {
		got, err := rescoped.GetRoutingRules(ctx, defName)
		if err != nil {
			t.Fatalf("GetRoutingRules: %v", err)
		}
		if len(got) == 0 {
			t.Fatal("GetRoutingRules returned none -- cleat#2210: under the bug this reports B's own rule as nonexistent")
		}
	})

	t.Run("ResolveVersionByTag", func(t *testing.T) {
		got, err := rescoped.ResolveVersionByTag(ctx, defName, readTag)
		if err != nil {
			t.Fatalf("ResolveVersionByTag: %v", err)
		}
		if got != 1 {
			t.Fatalf("ResolveVersionByTag = %d, want 1 -- cleat#2210: under the bug this reports \"tag not found\"", got)
		}
	})

	// ---- Writes last, each on its own dedicated row ----
	//
	// Every verification query below goes through mssqlQueryRowAsTenant/
	// mssqlScanCount, not a bare adminDB query: sa is not exempt from
	// the FILTER predicate, so a verification query with no session
	// context set sees zero rows for everything, which reads identically
	// to "the write landed under the wrong tenant" whether it did or not.
	// Measured directly -- see those helpers' doc comment.
	//
	// workflow_routing has an FK to workflow_defs (fk_workflow_routing_def),
	// so the routing subtests below reuse defName/defName2 rather than
	// inventing workflow names with no backing def.

	t.Run("SetWorkflowTag", func(t *testing.T) {
		writeTag := "wt-resc-dep-writetag-" + run
		if err := rescoped.SetWorkflowTag(ctx, defName, 1, writeTag); err != nil {
			t.Fatalf("SetWorkflowTag: %v", err)
		}
		var tenantIDStr string
		if err := mssqlQueryRowAsTenant(ctx, t, adminDB, tenantB, `
			SELECT LOWER(CONVERT(NVARCHAR(36), tenant_id)) FROM workflow_tags WHERE workflow_name = @p1 AND tag = @p2`,
			defName, writeTag).Scan(&tenantIDStr); err != nil {
			t.Fatalf("verify SetWorkflowTag's write: %v", err)
		}
		if tenantIDStr != tenantB {
			t.Fatalf("SetWorkflowTag wrote tenant_id = %s, want %s", tenantIDStr, tenantB)
		}
	})

	t.Run("RemoveWorkflowTag", func(t *testing.T) {
		deleteTag := "wt-resc-dep-deletetag-" + run
		if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
			INSERT INTO workflow_tags (workflow_name, version, tag, tenant_id) VALUES (@p1, 1, @p2, @p3)`,
			defName, deleteTag, tenantB); err != nil {
			t.Fatalf("seed the tag RemoveWorkflowTag will delete: %v", err)
		}
		if n := mssqlScanCount(ctx, t, adminDB, tenantB,
			`SELECT COUNT(*) FROM workflow_tags WHERE workflow_name = @p1 AND tag = @p2`, defName, deleteTag); n != 1 {
			t.Fatalf("seed check: workflow_tags has %d row(s) for the tag about to be removed, want 1", n)
		}
		if err := rescoped.RemoveWorkflowTag(ctx, defName, deleteTag); err != nil {
			t.Fatalf("RemoveWorkflowTag: %v", err)
		}
		if n := mssqlScanCount(ctx, t, adminDB, tenantB,
			`SELECT COUNT(*) FROM workflow_tags WHERE workflow_name = @p1 AND tag = @p2`, defName, deleteTag); n != 0 {
			t.Fatalf("RemoveWorkflowTag left %d row(s) -- cleat#2210: under the bug this DELETEs scoped to A's SESSION_CONTEXT, matching none of B's rows, so B's tag survives", n)
		}
	})

	t.Run("SetRoutingRule", func(t *testing.T) {
		if err := rescoped.SetRoutingRule(ctx, defName, 1, 0.5); err != nil {
			t.Fatalf("SetRoutingRule: %v", err)
		}
		if n := mssqlScanCount(ctx, t, adminDB, tenantB,
			`SELECT COUNT(*) FROM workflow_routing WHERE workflow_name = @p1 AND target_version = 1 AND weight = 0.5 AND tenant_id = @p2`,
			defName, tenantB); n != 1 {
			t.Fatalf("SetRoutingRule did not leave exactly 1 matching row as tenant B (got %d) -- "+
				"cleat#2210: under the bug this INSERTs scoped to A's SESSION_CONTEXT", n)
		}
	})

	t.Run("RemoveRoutingRule", func(t *testing.T) {
		var ruleID string
		if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
			INSERT INTO workflow_routing (workflow_name, target_version, weight, tenant_id)
			VALUES (@p1, 1, 0.75, @p2)`,
			defName, tenantB); err != nil {
			t.Fatalf("seed the routing rule RemoveRoutingRule will delete: %v", err)
		}
		if err := mssqlQueryRowAsTenant(ctx, t, adminDB, tenantB,
			`SELECT LOWER(CONVERT(NVARCHAR(36), id)) FROM workflow_routing WHERE workflow_name = @p1 AND weight = 0.75`,
			defName).Scan(&ruleID); err != nil {
			t.Fatalf("read back the seeded rule id: %v", err)
		}
		if err := rescoped.RemoveRoutingRule(ctx, defName, ruleID); err != nil {
			t.Fatalf("RemoveRoutingRule: %v", err)
		}
		if n := mssqlScanCount(ctx, t, adminDB, tenantB,
			`SELECT COUNT(*) FROM workflow_routing WHERE workflow_name = @p1 AND weight = 0.75`, defName); n != 0 {
			t.Fatalf("RemoveRoutingRule left %d row(s) -- cleat#2210: under the bug this DELETEs scoped to A's SESSION_CONTEXT, matching none of B's rows, so B's rule survives", n)
		}
	})

	t.Run("MarkVersionDeprecated", func(t *testing.T) {
		if err := rescoped.MarkVersionDeprecated(ctx, defName2, 1, true); err != nil {
			t.Fatalf("MarkVersionDeprecated: %v", err)
		}
		var disabledAt sql.NullTime
		var gcEligible bool
		if err := mssqlQueryRowAsTenant(ctx, t, adminDB, tenantB,
			`SELECT disabled_at, gc_eligible FROM workflow_defs WHERE name = @p1 AND version = 1`,
			defName2).Scan(&disabledAt, &gcEligible); err != nil {
			t.Fatalf("verify MarkVersionDeprecated's write: %v", err)
		}
		if !disabledAt.Valid || !gcEligible {
			t.Fatalf("MarkVersionDeprecated did not mark %q deprecated (disabled_at.Valid=%v gc_eligible=%v) -- "+
				"cleat#2210: under the bug this UPDATEs scoped to A's SESSION_CONTEXT, matching none of B's rows",
				defName2, disabledAt.Valid, gcEligible)
		}
	})

	t.Run("PurgeWorkflowDef", func(t *testing.T) {
		purgeDef := "wt-resc-dep-purge-" + run
		if err := mssqlExecAsTenant(ctx, t, adminDB, tenantB, `
			INSERT INTO workflow_defs (name, version, wasm_bytes, abi_version, min_version, tenant_id)
			VALUES (@p1, 1, 0x0061736d, 1, 1, @p2)`,
			purgeDef, tenantB); err != nil {
			t.Fatalf("seed the def PurgeWorkflowDef will delete: %v", err)
		}
		if n := mssqlScanCount(ctx, t, adminDB, tenantB,
			`SELECT COUNT(*) FROM workflow_defs WHERE name = @p1 AND version = 1`, purgeDef); n != 1 {
			t.Fatalf("seed check: workflow_defs has %d row(s) for the def about to be purged, want 1", n)
		}
		if err := rescoped.PurgeWorkflowDef(ctx, purgeDef, 1); err != nil {
			t.Fatalf("PurgeWorkflowDef: %v", err)
		}
		if n := mssqlScanCount(ctx, t, adminDB, tenantB,
			`SELECT COUNT(*) FROM workflow_defs WHERE name = @p1 AND version = 1`, purgeDef); n != 0 {
			t.Fatalf("PurgeWorkflowDef left %d row(s) -- cleat#2210: under the bug this DELETEs scoped to A's SESSION_CONTEXT, matching none of B's rows, so B's def survives", n)
		}
	})
}
