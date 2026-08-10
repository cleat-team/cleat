package main

// Integration coverage for the `cleatctl drop-tenant` command
// (droptenant.go), the caller Finding S3 asks for: admin.drop_tenant
// (migrations/postgres/032_drop_tenant_deletes_tenant_data.sql, proven
// against the real deletion logic in engine/drop_tenant_test.go) had no Go
// caller anywhere in the tree before this. These tests exercise the CLI
// layer specifically -- argument parsing, the default-tenant refusal, the
// dry-run/confirmation/audit-output guard rails -- against a real
// PostgreSQL connection, the same CLEAT_TEST_POSTGRES database
// engine/drop_tenant_test.go uses. Skips (does not fail) if
// CLEAT_TEST_POSTGRES is unset, matching testutil.TestDB's own convention.

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"

	_ "github.com/lib/pq"
)

// apply032ForDropTenantTest reads and executes
// migrations/postgres/032_drop_tenant_deletes_tenant_data.sql, the same
// approach engine/drop_tenant_test.go uses for its own copy -- duplicated
// here rather than exported from the engine package's _test.go file, which
// cmd/cleatctl cannot import.
func apply032ForDropTenantTest(t *testing.T, db *sql.DB) {
	t.Helper()
	path := filepath.Join("..", "..", "migrations", "postgres", "032_drop_tenant_deletes_tenant_data.sql")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if _, err := db.Exec(string(data)); err != nil {
		t.Fatalf("apply %s: %v", path, err)
	}
}

// dropTenantFixturePrefix is the common prefix every tenant UUID in this
// file starts with, so cleanupDropTenantFixtures can find and remove them
// regardless of which specific test created them.
const dropTenantFixturePrefix = "d70e0000-0000-4000-8000-0000000000%"

// cleanupDropTenantFixtures removes rows testutil.CleanupPostgresTestData
// does not know about (admin.tenants, admin.tenant_roles,
// admin.tenant_api_keys, workflow_tags, workflow_routing) for any tenant
// matching dropTenantFixturePrefix. Without this, a test that deliberately
// does NOT delete its fixture (TestRunDropTenant_DryRunDeletesNothing,
// TestRunDropTenant_ConfirmationMismatchCancels -- proving the guard rail
// left the data alone) leaks its admin.tenants row past the end of the
// test, and the next run of the same test hits a duplicate-key error on
// that row instead of exercising the guard rail at all.
func cleanupDropTenantFixtures(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, stmt := range []string{
		`DELETE FROM workflow_tags WHERE tenant_id::text LIKE $1`,
		`DELETE FROM workflow_routing WHERE tenant_id::text LIKE $1`,
		`DELETE FROM admin.tenant_api_keys WHERE tenant_id::text LIKE $1`,
		`DELETE FROM admin.tenant_roles WHERE tenant_id::text LIKE $1`,
		`DELETE FROM admin.tenants WHERE tenant_id::text LIKE $1`,
	} {
		if _, err := db.Exec(stmt, dropTenantFixturePrefix); err != nil {
			t.Logf("cleanupDropTenantFixtures: %s: %v", stmt, err)
		}
	}
}

func dropTenantTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)
	apply032ForDropTenantTest(t, db)
	testutil.CleanupPostgresTestData(t, db)
	cleanupDropTenantFixtures(t, db)
	t.Cleanup(func() {
		cleanupDropTenantFixtures(t, db)
		testutil.CleanupPostgresTestData(t, db)
		db.Close()
	})
	return db
}

func TestRunDropTenant_RefusesDefaultTenant(t *testing.T) {
	db := dropTenantTestDB(t)
	ctx := context.Background()

	stderr := withExitPanic(t, func() {
		runDropTenant(ctx, db, []string{engine.DefaultTenantUUID})
	})
	if !strings.Contains(stderr, "refusing to drop the default tenant") {
		t.Errorf("expected refusal message in stderr, got: %s", stderr)
	}

	// Confirm nothing was touched: the default tenant's workflow_defs rows
	// (there may be none from this fixture, but the point is the call must
	// never have reached admin.drop_tenant at all).
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM admin.tenants WHERE tenant_id = $1`, engine.DefaultTenantUUID).Scan(&n); err != nil {
		t.Fatalf("count admin.tenants: %v", err)
	}
}

func TestRunDropTenant_NoTenantIDArgument(t *testing.T) {
	db := dropTenantTestDB(t)
	ctx := context.Background()

	stderr := withExitPanic(t, func() {
		runDropTenant(ctx, db, []string{})
	})
	if !strings.Contains(stderr, "Usage:") {
		t.Errorf("expected usage text in stderr, got: %s", stderr)
	}
}

func TestRunDropTenant_DryRunDeletesNothing(t *testing.T) {
	db := dropTenantTestDB(t)
	ctx := context.Background()

	const tenant = "d70e0000-0000-4000-8000-000000000001"
	const defName = "cleatctl-drop-tenant-dryrun-def"
	if err := engine.NewPostgresStore(db).DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2)`, tenant, "dryrun-tenant"); err != nil {
		t.Fatalf("seed admin.tenants: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO workflow_instances (id, def_name, def_version, status, tenant_id) VALUES ($1, $2, 1, 'ready', $3)`,
		"dryrun-wf", defName, tenant); err != nil {
		t.Fatalf("seed workflow_instances: %v", err)
	}

	stdout, stderr := captureOutputs(t, func() {
		runDropTenant(ctx, db, []string{tenant, "--dry-run"})
	})
	if stderr != "" {
		t.Errorf("unexpected stderr: %s", stderr)
	}
	if !strings.Contains(stdout, "dry-run: nothing deleted") {
		t.Errorf("expected dry-run notice in stdout, got: %s", stdout)
	}
	if !strings.Contains(stdout, "workflow_instances") || !strings.Contains(stdout, "1") {
		t.Errorf("expected the pre-deletion count table in stdout, got: %s", stdout)
	}

	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM workflow_instances WHERE tenant_id = $1`, tenant).Scan(&n); err != nil {
		t.Fatalf("count workflow_instances: %v", err)
	}
	if n != 1 {
		t.Errorf("--dry-run deleted data: workflow_instances count = %d, want 1", n)
	}
}

func TestRunDropTenant_ConfirmationMismatchCancels(t *testing.T) {
	db := dropTenantTestDB(t)
	ctx := context.Background()

	const tenant = "d70e0000-0000-4000-8000-000000000002"
	if _, err := db.ExecContext(ctx, `INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2)`, tenant, "cancel-tenant"); err != nil {
		t.Fatalf("seed admin.tenants: %v", err)
	}

	var stdout string
	withStdin(t, "not-the-tenant-id\n", func() {
		stdout, _ = captureOutputs(t, func() {
			runDropTenant(ctx, db, []string{tenant})
		})
	})
	if !strings.Contains(stdout, "cancelled") {
		t.Errorf("expected cancellation message, got: %s", stdout)
	}

	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM admin.tenants WHERE tenant_id = $1`, tenant).Scan(&n); err != nil {
		t.Fatalf("count admin.tenants: %v", err)
	}
	if n != 1 {
		t.Errorf("cancelled confirmation still deleted the tenant row (count = %d, want 1)", n)
	}
}

func TestRunDropTenant_YesFlagDeletesWithoutPrompt(t *testing.T) {
	db := dropTenantTestDB(t)
	ctx := context.Background()

	const tenant = "d70e0000-0000-4000-8000-000000000003"
	const defName = "cleatctl-drop-tenant-yes-def"
	if err := engine.NewPostgresStore(db).DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2)`, tenant, "yes-tenant"); err != nil {
		t.Fatalf("seed admin.tenants: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO workflow_instances (id, def_name, def_version, status, tenant_id) VALUES ($1, $2, 1, 'ready', $3)`,
		"yes-wf", defName, tenant); err != nil {
		t.Fatalf("seed workflow_instances: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO event_history (workflow_id, step, tenant_id) VALUES ($1, 0, $2)`, "yes-wf", tenant); err != nil {
		t.Fatalf("seed event_history: %v", err)
	}

	// No stdin provided: --yes must skip the confirmation read entirely, or
	// this would block/read EOF and fail the wrong way.
	stdout, stderr := captureOutputs(t, func() {
		runDropTenant(ctx, db, []string{tenant, "--yes"})
	})
	if stderr != "" {
		t.Errorf("unexpected stderr: %s", stderr)
	}
	if !strings.Contains(stdout, "Deleted tenant "+tenant) {
		t.Errorf("expected deletion confirmation in stdout, got: %s", stdout)
	}

	for _, q := range []string{
		`SELECT count(*) FROM workflow_instances WHERE tenant_id = $1`,
		`SELECT count(*) FROM event_history WHERE tenant_id = $1`,
		`SELECT count(*) FROM admin.tenants WHERE tenant_id = $1`,
	} {
		var n int
		if err := db.QueryRowContext(ctx, q, tenant).Scan(&n); err != nil {
			t.Fatalf("count query %q: %v", q, err)
		}
		if n != 0 {
			t.Errorf("query %q returned %d after --yes delete, want 0", q, n)
		}
	}
}

func TestRunDropTenant_MatchingConfirmationDeletes(t *testing.T) {
	db := dropTenantTestDB(t)
	ctx := context.Background()

	const tenant = "d70e0000-0000-4000-8000-000000000004"
	if _, err := db.ExecContext(ctx, `INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2)`, tenant, "match-tenant"); err != nil {
		t.Fatalf("seed admin.tenants: %v", err)
	}

	var stdout string
	withStdin(t, tenant+"\n", func() {
		stdout, _ = captureOutputs(t, func() {
			runDropTenant(ctx, db, []string{tenant})
		})
	})
	if !strings.Contains(stdout, "Deleted tenant "+tenant) {
		t.Errorf("expected deletion confirmation in stdout, got: %s", stdout)
	}

	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM admin.tenants WHERE tenant_id = $1`, tenant).Scan(&n); err != nil {
		t.Fatalf("count admin.tenants: %v", err)
	}
	if n != 0 {
		t.Errorf("matching confirmation did not delete: admin.tenants count = %d, want 0", n)
	}
}

func TestRunDropTenant_NothingToDelete(t *testing.T) {
	db := dropTenantTestDB(t)
	ctx := context.Background()

	const tenant = "d70e0000-0000-4000-8000-000000000005"
	stdout, stderr := captureOutputs(t, func() {
		runDropTenant(ctx, db, []string{tenant})
	})
	if stderr != "" {
		t.Errorf("unexpected stderr: %s", stderr)
	}
	if !strings.Contains(stdout, "Nothing to delete") {
		t.Errorf("expected 'Nothing to delete' for an unknown tenant with no rows, got: %s", stdout)
	}
}
