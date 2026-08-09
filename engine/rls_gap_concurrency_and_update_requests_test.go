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
// This file applies 031_... directly via os.ReadFile + Exec rather than
// through engine/testutil.postgresSchemaFiles(), which is an explicit list
// (not a directory glob) and is engine/testutil's -- owned by another
// stream this round per PARALLEL-WORKSTREAMS.md, which asks other streams
// not to add to it without asking first. Applying the migration file
// directly here keeps this stream's verification self-contained without
// touching that file.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// apply031RLSGapMigration reads and executes
// migrations/postgres/031_rls_gap_concurrency_and_update_requests.sql
// against db. Must be called with a superuser/owner connection, same
// requirement as SetupFullSchema.
func apply031RLSGapMigration(t *testing.T, db *sql.DB) {
	t.Helper()
	path := filepath.Join("..", "migrations", "postgres", "031_rls_gap_concurrency_and_update_requests.sql")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if _, err := db.Exec(string(data)); err != nil {
		t.Fatalf("apply %s: %v", path, err)
	}
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
	adminStore := NewPostgresStore(adminDB)
	const defName = "rls-gap-ck-def"
	if err := adminStore.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
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

	adminStore := NewPostgresStore(adminDB)
	const defName = "rls-gap-ur-def"
	if err := adminStore.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
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
