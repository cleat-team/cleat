package engine

// Layer-separation proof for Finding S1's fix: migrations/postgres/031_rls_gap_concurrency_and_update_requests.sql
// adds Row-Level Security to concurrency_keys and workflow_update_requests.
//
// CLAUDE.md's standing requirement for this class of test: prove the DB
// policy blocks cross-tenant access on its own (Go-level filter removed),
// AND prove the existing Go-level filter blocks it on its own (DB policy
// bypassed). A test that only exercises the normal store methods through a
// superuser connection would pass for the wrong reason -- PostgreSQL never
// applies RLS to a superuser connection, and CLEAT_TEST_POSTGRES /
// CLEAT_TEST_DB conventionally point at one (verified below).
//
// This file applies 031_... directly via os.ReadFile + Exec, which is now
// redundant with (but harmless alongside) testutil.TestDB/SetupFullSchema:
// Stream A1 replaced engine/testutil's curated migration file list with the
// real migration.Runner over the whole migrations/postgres/ directory, so
// 031 is already applied by the time this test's testutil.TestDB call
// returns -- it was exactly the gap A1 closed (this file's own history is
// why: postgresSchemaFiles() was an explicit list that had fallen behind by
// one migration, this one, at the time A1 started). The direct apply here is
// left in place rather than removed: 031's own statements are idempotent
// (DROP POLICY IF EXISTS ... CREATE POLICY), so reapplying is a no-op, and
// keeping the explicit call documents the dependency locally rather than
// relying on a reader to know testutil now does this implicitly.

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// assertRLSPolicyExists fails unless table carries a policy called policy.
//
// This is how the applyXXX RLS helpers work since the cleat#2059 rebaseline.
// They used to re-apply a specific numbered migration; the consolidated
// baseline carries every one of those policies in 001_schema.sql, and
// SetupFullSchema applies the whole directory, so there was nothing left to
// apply -- and re-applying 001 wholesale is not safe, because it has no
// IF NOT EXISTS on its CREATE SCHEMA and it now redefines every routine.
//
// What the call sites actually need is that the policy is THERE. Asserting that
// states the dependency even more directly than re-installing it did: the old
// form could not fail if the migration were a no-op, and this cannot pass if
// the policy is absent.
func assertRLSPolicyExists(t *testing.T, db *sql.DB, table, policy string) {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM pg_policies
		 WHERE schemaname = current_schema() AND tablename = $1 AND policyname = $2`,
		table, policy).Scan(&n); err != nil {
		t.Fatalf("looking for policy %s on %s: %v", policy, table, err)
	}
	if n == 0 {
		t.Fatalf("%s has no RLS policy named %s; 001_schema.sql should have installed "+
			"one, so either the baseline lost it or SetupFullSchema did not run", table, policy)
	}
}

// apply031RLSGapMigration used to read and execute
// migrations/postgres/031_rls_gap_concurrency_and_update_requests.sql. That file
// is part of the consolidated baseline now; this asserts what the call sites
// need from it. Kept under its old name so the call sites still read as a
// stated dependency on 031's fix.
func apply031RLSGapMigration(t *testing.T, db *sql.DB) {
	t.Helper()
	assertRLSPolicyExists(t, db, "concurrency_keys", "tenant_isolation_concurrency_keys")
	assertRLSPolicyExists(t, db, "workflow_update_requests", "tenant_isolation_update_requests")
}

// assertNotSuperuserBypass is the check CLAUDE.md asks be made explicit
// rather than assumed: the PostgreSQL test role is documented (both in
// CLAUDE.md and in testutil.PostgresRLSTestRole's own comment) to default to
// a superuser. A policy test run as that role measures nothing.
func assertNotSuperuserBypass(t *testing.T, db *sql.DB) {
	t.Helper()
	var user string
	var super, bypass bool
	if err := db.QueryRow(`
		SELECT current_user,
		       coalesce((SELECT rolsuper FROM pg_roles WHERE rolname = current_user), false),
		       coalesce((SELECT rolbypassrls FROM pg_roles WHERE rolname = current_user), false)
	`).Scan(&user, &super, &bypass); err != nil {
		t.Fatalf("check connecting role: %v", err)
	}
	if super || bypass {
		t.Fatalf("connecting role %q is superuser=%v bypassrls=%v -- this connection cannot "+
			"observe RLS enforcement at all, so a test run against it proves nothing", user, super, bypass)
	}
}

// setSessionTenant runs SELECT set_config('cleat.tenant_id', tenant, false)
// -- session-level (is_local=false), not transaction-local, so it survives
// across the raw queries this test issues on the same *sql.Conn without
// wrapping each one in its own transaction.
func setSessionTenant(t *testing.T, ctx context.Context, conn *sql.Conn, tenant string) {
	t.Helper()
	if _, err := conn.ExecContext(ctx, `SELECT set_config('cleat.tenant_id', $1, false)`, tenant); err != nil {
		t.Fatalf("set_config cleat.tenant_id: %v", err)
	}
}

func TestConcurrencyKeysRLS_LayerSeparation(t *testing.T) {
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	defer adminDB.Close()
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, adminDB)
	defer testutil.CleanupPostgresTestData(t, adminDB)
	apply031RLSGapMigration(t, adminDB)

	ctx := context.Background()
	const tenantA = "c0000000-0000-4000-8000-00000000000a"
	const tenantB = "c0000000-0000-4000-8000-00000000000b"

	// Fixtures: one workflow per tenant (concurrency_keys.workflow_id has an
	// FK to workflow_instances(id)), and one concurrency key per tenant.
	const defName = "rls-gap-ck-def"
	// Once per tenant since D7 (IMPROVEMENT-PLAN 3.77).
	deployDefForTenants(t, adminDB, defName, 1, tenantA, tenantB)
	wfA := fmt.Sprintf("rls-gap-ck-a-%d", time.Now().UnixNano())
	wfB := fmt.Sprintf("rls-gap-ck-b-%d", time.Now().UnixNano())
	for tenant, wf := range map[string]string{tenantA: wfA, tenantB: wfB} {
		if _, _, err := NewPostgresStore(adminDB).WithTenant(tenant).StartNewRun(
			ctx, wf, defName, 1, []byte(`{}`), "", tenant, 0); err != nil {
			t.Fatalf("StartNewRun(%s): %v", tenant, err)
		}
	}
	if _, err := adminDB.ExecContext(ctx, `
		INSERT INTO concurrency_keys (key_hash, key_text, workflow_id, expires_at, tenant_id)
		VALUES (digest('rls-gap-key-a', 'sha256'), 'rls-gap-key-a', $1, now() + interval '1 hour', $2)
	`, wfA, tenantA); err != nil {
		t.Fatalf("seed concurrency_keys for A: %v", err)
	}
	if _, err := adminDB.ExecContext(ctx, `
		INSERT INTO concurrency_keys (key_hash, key_text, workflow_id, expires_at, tenant_id)
		VALUES (digest('rls-gap-key-b', 'sha256'), 'rls-gap-key-b', $1, now() + interval '1 hour', $2)
	`, wfB, tenantB); err != nil {
		t.Fatalf("seed concurrency_keys for B: %v", err)
	}

	// --- Layer 1: the DB policy alone, Go-level filter removed. ---
	//
	// appDB is neither superuser nor table owner, so FORCE ROW LEVEL
	// SECURITY applies to it unconditionally (testutil.OpenPostgresRLSTestDB's
	// own doc comment). The query below carries NO tenant_id predicate at
	// all -- standing in for a store method that forgot to filter -- so if
	// this returns only tenant A's row, the policy is what did it.
	appDB := testutil.OpenPostgresRLSTestDB(t, adminDB)
	defer appDB.Close()
	assertNotSuperuserBypass(t, appDB)

	conn, err := appDB.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire connection: %v", err)
	}
	defer conn.Close()
	setSessionTenant(t, ctx, conn, tenantA)

	rows, err := conn.QueryContext(ctx, `SELECT key_text FROM concurrency_keys`)
	if err != nil {
		t.Fatalf("query concurrency_keys with no tenant predicate: %v", err)
	}
	var seen []string
	for rows.Next() {
		var kt string
		if err := rows.Scan(&kt); err != nil {
			rows.Close()
			t.Fatalf("scan: %v", err)
		}
		seen = append(seen, kt)
	}
	rows.Close()
	if len(seen) != 1 || seen[0] != "rls-gap-key-a" {
		t.Fatalf("tenant A session with no WHERE tenant_id predicate saw %v, want exactly "+
			"[rls-gap-key-a] -- the RLS policy is not filtering this query", seen)
	}

	// --- Layer 2: the existing Go-level filter alone, DB policy bypassed. ---
	//
	// adminDB is a superuser connection, so it bypasses the policy just
	// added unconditionally -- this is the "policy removed" half without
	// actually dropping it. GetConcurrencyKeyCount carries its own
	// `AND tenant_id = $2` (engine/db.go), so isolation here must come from
	// that, not from the policy this half of the test cannot see.
	countA, err := NewPostgresStore(adminDB).WithTenant(tenantA).GetConcurrencyKeyCount(ctx, wfA)
	if err != nil {
		t.Fatalf("GetConcurrencyKeyCount(A, wfA): %v", err)
	}
	if countA != 1 {
		t.Errorf("tenant A's own concurrency key count for its own workflow = %d, want 1", countA)
	}
	// Tenant B's store reading tenant A's workflow ID: the Go filter
	// (tenant_id = s.tenantID) must return 0, even on a connection where the
	// policy this test just added cannot be the one doing the work.
	countCrossTenant, err := NewPostgresStore(adminDB).WithTenant(tenantB).GetConcurrencyKeyCount(ctx, wfA)
	if err != nil {
		t.Fatalf("GetConcurrencyKeyCount(B, wfA): %v", err)
	}
	if countCrossTenant != 0 {
		t.Errorf("tenant B's store (over a superuser connection, so the RLS policy is bypassed) "+
			"saw %d of tenant A's concurrency keys for wfA -- the Go-level tenant_id filter is "+
			"not doing the isolating on its own", countCrossTenant)
	}
}

func TestWorkflowUpdateRequestsRLS_LayerSeparation(t *testing.T) {
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	defer adminDB.Close()
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, adminDB)
	defer testutil.CleanupPostgresTestData(t, adminDB)
	apply031RLSGapMigration(t, adminDB)

	ctx := context.Background()
	const tenantA = "d0000000-0000-4000-8000-00000000000a"
	const tenantB = "d0000000-0000-4000-8000-00000000000b"

	const defName = "rls-gap-ur-def"
	// Once per tenant since D7 (IMPROVEMENT-PLAN 3.77).
	deployDefForTenants(t, adminDB, defName, 1, tenantA, tenantB)
	wfA := fmt.Sprintf("rls-gap-ur-a-%d", time.Now().UnixNano())
	wfB := fmt.Sprintf("rls-gap-ur-b-%d", time.Now().UnixNano())
	storeA := NewPostgresStore(adminDB).WithTenant(tenantA)
	storeB := NewPostgresStore(adminDB).WithTenant(tenantB)
	if _, _, err := storeA.StartNewRun(ctx, wfA, defName, 1, []byte(`{}`), "", tenantA, 0); err != nil {
		t.Fatalf("StartNewRun(A): %v", err)
	}
	if _, _, err := storeB.StartNewRun(ctx, wfB, defName, 1, []byte(`{}`), "", tenantB, 0); err != nil {
		t.Fatalf("StartNewRun(B): %v", err)
	}
	if err := storeA.CreateUpdateRequest(ctx, wfA, "update-a", `{"n":"a"}`, "promise-a"); err != nil {
		t.Fatalf("CreateUpdateRequest(A): %v", err)
	}
	if err := storeB.CreateUpdateRequest(ctx, wfB, "update-b", `{"n":"b"}`, "promise-b"); err != nil {
		t.Fatalf("CreateUpdateRequest(B): %v", err)
	}

	// --- Layer 1: the DB policy alone, Go-level filter removed. ---
	appDB := testutil.OpenPostgresRLSTestDB(t, adminDB)
	defer appDB.Close()
	assertNotSuperuserBypass(t, appDB)

	conn, err := appDB.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire connection: %v", err)
	}
	defer conn.Close()
	setSessionTenant(t, ctx, conn, tenantA)

	rows, err := conn.QueryContext(ctx, `SELECT update_name FROM workflow_update_requests`)
	if err != nil {
		t.Fatalf("query workflow_update_requests with no tenant predicate: %v", err)
	}
	var seen []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			t.Fatalf("scan: %v", err)
		}
		seen = append(seen, name)
	}
	rows.Close()
	if len(seen) != 1 || seen[0] != "update-a" {
		t.Fatalf("tenant A session with no WHERE tenant_id predicate saw %v, want exactly "+
			"[update-a] -- the RLS policy is not filtering this query", seen)
	}

	// --- Layer 2: the existing Go-level filter alone, DB policy bypassed. ---
	pendingOwn, err := storeA.GetPendingUpdateRequests(ctx, wfA)
	if err != nil {
		t.Fatalf("GetPendingUpdateRequests(A, wfA): %v", err)
	}
	if len(pendingOwn) != 1 {
		t.Errorf("tenant A's own pending update requests for its own workflow = %d, want 1", len(pendingOwn))
	}
	pendingCross, err := storeB.GetPendingUpdateRequests(ctx, wfA)
	if err != nil {
		t.Fatalf("GetPendingUpdateRequests(B, wfA): %v", err)
	}
	if len(pendingCross) != 0 {
		t.Errorf("tenant B's store (over a superuser connection, so the RLS policy is bypassed) "+
			"saw %d of tenant A's update requests for wfA -- the Go-level tenant_id filter is "+
			"not doing the isolating on its own", len(pendingCross))
	}
}
