package engine

// Regression coverage for Finding S3: admin.drop_tenant
// (migrations/postgres/001_schema.sql, fixed by
// migrations/postgres/032_drop_tenant_deletes_tenant_data.sql) dropped a
// tenant's plugin schema and role and two admin.* bookkeeping rows, but
// never deleted a single row of the tenant's actual workflow data.
//
// This file applies 032 directly via os.ReadFile + Exec, the same approach
// engine/rls_gap_concurrency_and_update_requests_test.go uses for 031:
// engine/testutil's postgresSchemaFiles() is an explicit list (not a
// directory glob) owned by another stream this round per
// PARALLEL-WORKSTREAMS.md, so a migration added here is applied locally
// rather than by editing that list.
//
// CLAUDE.md's standing requirement: prove the regression test can fail, and
// read why. TestDropTenant_OldVersionLeavesDataBehind below calls the
// *pre-032* admin.drop_tenant (the version testutil.SetupFullSchema already
// installs via 001_schema.sql) against seeded tenant data and asserts the
// data survives -- pinning down the exact bug this migration fixes, rather
// than trusting a hand-run "I reverted it and it went red" that isn't
// preserved anywhere. TestDropTenant_DeletesAllTenantData then applies 032
// and proves the fixed version actually deletes everything, leaves a
// second tenant untouched, and refuses the default tenant.
//
// Every count below is read from adminDB, the superuser/schema-owner
// connection SetupFullSchema provisions -- not through any RLS-scoped
// store -- so a policy that merely hides rows cannot make either test pass
// for the wrong reason (the "TestCascadeDelete" failure mode CLAUDE.md
// calls out by name).

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// apply032DropTenantMigration reads and executes
// migrations/postgres/032_drop_tenant_deletes_tenant_data.sql against db.
// Must be called with a superuser/owner connection.
func apply032DropTenantMigration(t *testing.T, db *sql.DB) {
	t.Helper()
	path := filepath.Join("..", "migrations", "postgres", "032_drop_tenant_deletes_tenant_data.sql")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if _, err := db.Exec(string(data)); err != nil {
		t.Fatalf("apply %s: %v", path, err)
	}
}

// resetToOriginal001DropTenant reinstalls 001_schema.sql's admin.drop_tenant,
// undoing 032's CREATE OR REPLACE regardless of database history.
//
// testutil.applyPostgresSchemaFile (behind SetupFullSchema) fingerprints the
// combined contents of the files it applies and skips re-running them against a
// database where that exact fingerprint was already recorded -- 032 is not one
// of those files, so once 032 has been applied to a given CLEAT_TEST_POSTGRES
// database, SetupFullSchema alone can never revert it back to the pre-032
// admin.drop_tenant. TestDropTenant_OldVersionLeavesDataBehind needs exactly
// that pre-032 version to pin down the bug 032 fixes, so it calls this helper
// explicitly rather than relying on file-application order within the binary.
//
// It extracts ONLY that one function, and that is the whole point.
//
// This used to `db.Exec` the entire contents of 001_schema.sql. 001 defines
// five functions with CREATE OR REPLACE, so re-applying it reverted every one
// of them -- and two of the five have later migrations that fix them. The
// collateral one was cleat.assert_tenant_set: 034 makes it treat an empty
// tenant id like an unset one, and re-applying 001 put the pre-034 body back
// while leaving version 34 recorded in schema_migrations. No migration run
// repairs that, because the runner only applies versions it has not recorded.
//
// The result was a suite that poisoned its own database. Measured 2026-09-02:
// a full `go test ./engine/` run finished green and left assert_tenant_set on
// the 001 body, so the NEXT run failed TestAssertTenantSetRejectsEmptyStringLikeNull
// with "invalid input syntax for type uuid" -- a tenant/RLS failure with no
// connection to the test that caused it, appearing and disappearing depending
// on what had run against that database before. Re-derive the blast radius with
//
//	grep -n 'CREATE OR REPLACE FUNCTION' migrations/postgres/001_schema.sql
//
// and check each name for later definitions before widening this again.
func resetToOriginal001DropTenant(t *testing.T, db *sql.DB) {
	t.Helper()
	path := filepath.Join("..", "migrations", "postgres", "001_schema.sql")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	body := extractPlpgsqlFunction(t, string(data), "admin.drop_tenant")
	if _, err := db.Exec(body); err != nil {
		t.Fatalf("reinstall 001's admin.drop_tenant: %v", err)
	}
}

// extractPlpgsqlFunction returns the single CREATE OR REPLACE FUNCTION block
// for name from sql, from its CREATE line to the $$ LANGUAGE ... ; that ends
// it.
//
// It fails the test rather than returning something partial. A silently empty
// or truncated extraction here would leave the post-032 admin.drop_tenant in
// place, and TestDropTenant_OldVersionLeavesDataBehind -- whose whole job is to
// show the PRE-032 function losing data -- would then quietly assert that the
// fixed function is broken, or pass for the wrong reason.
func extractPlpgsqlFunction(t *testing.T, sql, name string) string {
	t.Helper()
	marker := "CREATE OR REPLACE FUNCTION " + name
	start := strings.Index(sql, marker)
	if start < 0 {
		t.Fatalf("no %q in 001_schema.sql; it was renamed or removed, and this "+
			"helper silently reinstalls nothing without this check", marker)
	}
	rest := sql[start:]
	end := strings.Index(rest, "$$ LANGUAGE plpgsql")
	if end < 0 {
		t.Fatalf("found %q but no terminating \"$$ LANGUAGE plpgsql\"; the function "+
			"body's shape changed", marker)
	}
	term := strings.Index(rest[end:], ";")
	if term < 0 {
		t.Fatalf("found %q but its LANGUAGE clause has no terminating semicolon", marker)
	}
	block := rest[:end+term+1]
	if strings.Count(block, "CREATE OR REPLACE FUNCTION") != 1 {
		t.Fatalf("the extracted block for %q contains %d function definitions, want 1 "+
			"-- extracting more than one is how this helper reverted migrations it "+
			"was never meant to touch", name,
			strings.Count(block, "CREATE OR REPLACE FUNCTION"))
	}
	return block
}

// cleanupDropTenantExtras deletes any rows this file's fixtures may have
// left in tables testutil.CleanupPostgresTestData does not know about
// (workflow_tags, workflow_routing, admin.tenant_api_keys,
// admin.tenant_roles, admin.tenants) for the given tenant, plus the
// tenant's plugin role/schema if either test left one behind. Without this,
// TestDropTenant_OldVersionLeavesDataBehind -- which deliberately exercises
// the pre-032 admin.drop_tenant that does NOT delete workflow_tags/
// workflow_routing -- leaves a workflow_tags row referencing the fixture's
// workflow_defs row, and CleanupPostgresTestData's later
// `DELETE FROM workflow_defs` fails its FK constraint (logged, not fatal,
// but it leaks state into whatever test runs against this database next).
func cleanupDropTenantExtras(t *testing.T, ctx context.Context, adminDB *sql.DB, tenant string) {
	t.Helper()
	for _, stmt := range []string{
		`DELETE FROM workflow_tags WHERE tenant_id = $1`,
		`DELETE FROM workflow_routing WHERE tenant_id = $1`,
		`DELETE FROM admin.tenant_api_keys WHERE tenant_id = $1`,
		`DELETE FROM admin.tenant_roles WHERE tenant_id = $1`,
		`DELETE FROM admin.tenants WHERE tenant_id = $1`,
	} {
		if _, err := adminDB.ExecContext(ctx, stmt, tenant); err != nil {
			t.Logf("cleanupDropTenantExtras: %s: %v", stmt, err)
		}
	}
	roleName := "cleat_tenant_" + tenantRoleSuffix(tenant)
	schemaName := "tenant_" + tenantRoleSuffix(tenant)
	if _, err := adminDB.ExecContext(ctx, `DO $$ BEGIN
		IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '`+roleName+`') THEN
			EXECUTE format('DROP OWNED BY %I', '`+roleName+`');
		END IF;
	END $$`); err != nil {
		t.Logf("cleanupDropTenantExtras: drop owned by %s: %v", roleName, err)
	}
	if _, err := adminDB.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+schemaName+` CASCADE`); err != nil {
		t.Logf("cleanupDropTenantExtras: drop schema %s: %v", schemaName, err)
	}
	if _, err := adminDB.ExecContext(ctx, `DROP ROLE IF EXISTS `+roleName); err != nil {
		t.Logf("cleanupDropTenantExtras: drop role %s: %v", roleName, err)
	}
}

// tenantRoleSuffix mirrors admin.create_tenant_role's
// replace(p_tenant_id::text, '-', '_').
func tenantRoleSuffix(tenant string) string {
	out := make([]rune, 0, len(tenant))
	for _, r := range tenant {
		if r == '-' {
			out = append(out, '_')
			continue
		}
		out = append(out, r)
	}
	return string(out)
}

// dropTenantFixture seeds one row in every table admin.drop_tenant is
// responsible for, for the given tenant: workflow_instances, event_history,
// workflow_signals, workflow_promises, concurrency_keys,
// workflow_update_requests, workflow_schedules, workflow_tags,
// workflow_routing, and idempotency_keys unconditionally.
//
// withAPIKey and withRole are separate switches, not a package deal,
// because each triggers a distinct compounding bug in the pre-032
// function and this file proves each one in isolation:
//   - withAPIKey seeds admin.tenant_api_keys. admin.tenant_api_keys'
//     FK to admin.tenants has no ON DELETE clause, and the pre-032
//     function deletes admin.tenants without deleting this table first,
//     so any tenant that ever had an API key issued makes that DELETE
//     raise a foreign key violation.
//   - withRole calls admin.create_tenant_role, giving the tenant a real
//     per-tenant Postgres role (and admin.tenant_roles row) that still
//     holds table GRANTs, which the pre-032 function's DROP ROLE step
//     cannot drop.
//
// Both failures abort the whole function (one top-level CALL is one
// transaction), so a tenant fixture seeded with either flag true proves a
// *different* failure shape (loud error, nothing deleted) than
// Finding S3's literal "succeeds, deletes almost nothing" -- which only
// reproduces with both flags false. defName must already be deployed.
// Returns the workflow ID used, so callers can key their own assertions
// off it.
func dropTenantFixture(t *testing.T, ctx context.Context, adminDB *sql.DB, tenant, defName, tag string, withAPIKey, withRole bool) string {
	t.Helper()
	wfID := "drop-tenant-" + tag

	if _, err := adminDB.ExecContext(ctx,
		`INSERT INTO admin.tenants (tenant_id, name) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		tenant, "tenant-"+tag); err != nil {
		t.Fatalf("seed admin.tenants(%s): %v", tag, err)
	}
	if _, err := adminDB.ExecContext(ctx,
		`INSERT INTO workflow_instances (id, def_name, def_version, status, tenant_id) VALUES ($1, $2, 1, 'ready', $3)`,
		wfID, defName, tenant); err != nil {
		t.Fatalf("seed workflow_instances(%s): %v", tag, err)
	}
	if _, err := adminDB.ExecContext(ctx,
		`INSERT INTO event_history (workflow_id, step, tenant_id) VALUES ($1, 0, $2)`,
		wfID, tenant); err != nil {
		t.Fatalf("seed event_history(%s): %v", tag, err)
	}
	if _, err := adminDB.ExecContext(ctx,
		`INSERT INTO workflow_signals (workflow_id, signal_name, tenant_id) VALUES ($1, $2, $3)`,
		wfID, "sig-"+tag, tenant); err != nil {
		t.Fatalf("seed workflow_signals(%s): %v", tag, err)
	}
	if _, err := adminDB.ExecContext(ctx,
		`INSERT INTO workflow_promises (workflow_id, promise_id, promise_name, tenant_id) VALUES ($1, $2, $3, $4)`,
		wfID, "p-"+tag, "promise-"+tag, tenant); err != nil {
		t.Fatalf("seed workflow_promises(%s): %v", tag, err)
	}
	if _, err := adminDB.ExecContext(ctx,
		`INSERT INTO concurrency_keys (key_hash, key_text, workflow_id, expires_at, tenant_id) VALUES (digest($1,'sha256'), $1, $2, now() + interval '1 hour', $3)`,
		"ckey-"+tag, wfID, tenant); err != nil {
		t.Fatalf("seed concurrency_keys(%s): %v", tag, err)
	}
	if _, err := adminDB.ExecContext(ctx,
		`INSERT INTO workflow_update_requests (workflow_id, update_name, tenant_id) VALUES ($1, $2, $3)`,
		wfID, "upd-"+tag, tenant); err != nil {
		t.Fatalf("seed workflow_update_requests(%s): %v", tag, err)
	}
	if _, err := adminDB.ExecContext(ctx,
		`INSERT INTO workflow_schedules (name, def_name, cron_expression, tenant_id) VALUES ($1, $2, '* * * * *', $3)`,
		"sched-"+tag, defName, tenant); err != nil {
		t.Fatalf("seed workflow_schedules(%s): %v", tag, err)
	}
	if _, err := adminDB.ExecContext(ctx,
		`INSERT INTO workflow_tags (workflow_name, version, tag, tenant_id) VALUES ($1, 1, $2, $3)`,
		defName, "tag-"+tag, tenant); err != nil {
		t.Fatalf("seed workflow_tags(%s): %v", tag, err)
	}
	if _, err := adminDB.ExecContext(ctx,
		`INSERT INTO workflow_routing (workflow_name, target_version, tenant_id) VALUES ($1, 1, $2)`,
		defName, tenant); err != nil {
		t.Fatalf("seed workflow_routing(%s): %v", tag, err)
	}
	if _, err := adminDB.ExecContext(ctx,
		`INSERT INTO idempotency_keys (key_hash, workflow_id, tenant_id) VALUES (digest($1,'sha256'), $2, $3)`,
		"idem-"+tag, wfID, tenant); err != nil {
		t.Fatalf("seed idempotency_keys(%s): %v", tag, err)
	}
	if withAPIKey {
		if _, err := adminDB.ExecContext(ctx,
			`INSERT INTO admin.tenant_api_keys (tenant_id, key_hash, description) VALUES ($1, digest($2,'sha256'), $3)`,
			tenant, "apikey-"+tag, "key for "+tag); err != nil {
			t.Fatalf("seed admin.tenant_api_keys(%s): %v", tag, err)
		}
	}
	if withRole {
		// admin.tenant_roles + a real role/schema, exercising the DROP ROLE
		// path (and the DROP OWNED BY fix for it -- see 032's comment).
		if _, err := adminDB.ExecContext(ctx, `SELECT admin.create_tenant_role($1)`, tenant); err != nil {
			t.Fatalf("create_tenant_role(%s): %v", tag, err)
		}
	}
	return wfID
}

// dropTenantCounts is a snapshot of every table admin.drop_tenant touches,
// for one tenant.
type dropTenantCounts struct {
	Instances, Events, Signals, Promises, ConcurrencyKeys, UpdateRequests,
	Schedules, Tags, Routing, IdempotencyKeys, APIKeys, TenantRoles, Tenants int
}

func countDropTenantRows(t *testing.T, ctx context.Context, adminDB *sql.DB, tenant string) dropTenantCounts {
	t.Helper()
	count := func(query string) int {
		var n int
		if err := adminDB.QueryRowContext(ctx, query, tenant).Scan(&n); err != nil {
			t.Fatalf("count query %q: %v", query, err)
		}
		return n
	}
	return dropTenantCounts{
		Instances:       count(`SELECT count(*) FROM workflow_instances WHERE tenant_id = $1`),
		Events:          count(`SELECT count(*) FROM event_history WHERE tenant_id = $1`),
		Signals:         count(`SELECT count(*) FROM workflow_signals WHERE tenant_id = $1`),
		Promises:        count(`SELECT count(*) FROM workflow_promises WHERE tenant_id = $1`),
		ConcurrencyKeys: count(`SELECT count(*) FROM concurrency_keys WHERE tenant_id = $1`),
		UpdateRequests:  count(`SELECT count(*) FROM workflow_update_requests WHERE tenant_id = $1`),
		Schedules:       count(`SELECT count(*) FROM workflow_schedules WHERE tenant_id = $1`),
		Tags:            count(`SELECT count(*) FROM workflow_tags WHERE tenant_id = $1`),
		Routing:         count(`SELECT count(*) FROM workflow_routing WHERE tenant_id = $1`),
		IdempotencyKeys: count(`SELECT count(*) FROM idempotency_keys WHERE tenant_id = $1`),
		APIKeys:         count(`SELECT count(*) FROM admin.tenant_api_keys WHERE tenant_id = $1`),
		TenantRoles:     count(`SELECT count(*) FROM admin.tenant_roles WHERE tenant_id = $1`),
		Tenants:         count(`SELECT count(*) FROM admin.tenants WHERE tenant_id = $1`),
	}
}

func (c dropTenantCounts) total() int {
	return c.Instances + c.Events + c.Signals + c.Promises + c.ConcurrencyKeys +
		c.UpdateRequests + c.Schedules + c.Tags + c.Routing + c.IdempotencyKeys +
		c.APIKeys + c.TenantRoles + c.Tenants
}

// TestDropTenant_OldVersionLeavesDataBehind pins down Finding S3 itself:
// the admin.drop_tenant that ships in 001_schema.sql (before 032 is
// applied) drops only two bookkeeping rows and leaves every row of actual
// tenant data in place -- succeeding, not erroring, exactly the "returns
// successfully having deleted almost nothing" shape the finding describes.
//
// This fixture deliberately passes withRole=false: a tenant that had ever
// been through admin.create_tenant_role trips a second, compounding bug in
// the pre-032 DROP ROLE step (see 032's own comment and
// TestDropTenant_RoleDropFailureRollsBackEverything below), which aborts
// the whole function and rolls back even the two rows it does otherwise
// delete. That is real and worth its own test, but it is a different
// failure shape (a loud error, not a silent near-no-op) from the one
// Finding S3 describes, so it is kept separate rather than conflated here.
func TestDropTenant_OldVersionLeavesDataBehind(t *testing.T) {
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	defer adminDB.Close()
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	resetToOriginal001DropTenant(t, adminDB) // guarantee the pre-032 function, see helper doc
	testutil.CleanupPostgresTestData(t, adminDB)
	defer testutil.CleanupPostgresTestData(t, adminDB)

	ctx := context.Background()
	const tenant = "01d00000-0000-4000-8000-00000000001d"
	const defName = "drop-tenant-old-def"
	defer cleanupDropTenantExtras(t, ctx, adminDB, tenant) // registered before any possible t.Fatalf below
	if err := NewPostgresStore(adminDB).DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
	dropTenantFixture(t, ctx, adminDB, tenant, defName, "old", false, false)

	before := countDropTenantRows(t, ctx, adminDB, tenant)
	if before.total() == 0 {
		t.Fatalf("fixture setup produced no rows for tenant -- test proves nothing")
	}

	if _, err := adminDB.ExecContext(ctx, `SELECT admin.drop_tenant($1)`, tenant); err != nil {
		t.Fatalf("admin.drop_tenant (pre-032 version, no tenant role provisioned): %v -- "+
			"this call was expected to succeed (Finding S3's whole point is that it succeeds "+
			"while deleting almost nothing); an error here means either the fixture or the "+
			"pre-032 function reset picked up something unexpected", err)
	}

	after := countDropTenantRows(t, ctx, adminDB, tenant)
	// The two rows the old version DOES delete.
	if after.TenantRoles != 0 {
		t.Errorf("admin.tenant_roles = %d after drop_tenant, want 0 (this row the old version does delete)", after.TenantRoles)
	}
	if after.Tenants != 0 {
		t.Errorf("admin.tenants = %d after drop_tenant, want 0 (this row the old version does delete)", after.Tenants)
	}
	// Finding S3: everything else survives. APIKeys is deliberately not
	// checked here -- this fixture seeds withAPIKey=false (see
	// TestDropTenant_APIKeyFailureRollsBackEverything for that case), so it
	// is legitimately 0 both before and after.
	if after.Instances == 0 || after.Events == 0 || after.Signals == 0 || after.Promises == 0 ||
		after.ConcurrencyKeys == 0 || after.UpdateRequests == 0 || after.Schedules == 0 ||
		after.Tags == 0 || after.Routing == 0 || after.IdempotencyKeys == 0 {
		t.Fatalf("pre-032 admin.drop_tenant unexpectedly deleted tenant data (counts: %+v) -- "+
			"if this fails, either 032 leaked into this test's schema setup or the bug this "+
			"migration fixes is already gone from 001_schema.sql", after)
	}
}

// TestDropTenant_APIKeyFailureRollsBackEverything documents the third bug
// 032 fixes (see its comment): admin.tenant_api_keys' FK to admin.tenants
// has no ON DELETE clause, and the pre-032 function deletes admin.tenants
// without deleting admin.tenant_api_keys first, so any tenant that ever had
// an API key issued makes that DELETE raise a foreign key violation --
// aborting the whole function and rolling back every DELETE it had already
// run, the same shape as the DROP ROLE failure above but from a completely
// independent cause.
func TestDropTenant_APIKeyFailureRollsBackEverything(t *testing.T) {
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	defer adminDB.Close()
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	resetToOriginal001DropTenant(t, adminDB)
	testutil.CleanupPostgresTestData(t, adminDB)
	defer testutil.CleanupPostgresTestData(t, adminDB)

	ctx := context.Background()
	const tenant = "01d00002-0000-4000-8000-000000000002"
	const defName = "drop-tenant-old-apikey-def"
	defer cleanupDropTenantExtras(t, ctx, adminDB, tenant)
	if err := NewPostgresStore(adminDB).DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
	dropTenantFixture(t, ctx, adminDB, tenant, defName, "oldapikey", true, false)

	before := countDropTenantRows(t, ctx, adminDB, tenant)
	if before.total() == 0 {
		t.Fatalf("fixture setup produced no rows for tenant -- test proves nothing")
	}

	_, err := adminDB.ExecContext(ctx, `SELECT admin.drop_tenant($1)`, tenant)
	if err == nil {
		t.Fatalf("admin.drop_tenant (pre-032 version, API key provisioned) succeeded -- " +
			"expected a foreign key violation deleting admin.tenants")
	}

	after := countDropTenantRows(t, ctx, adminDB, tenant)
	if after != before {
		t.Errorf("admin.drop_tenant's error left partial changes -- before=%+v after=%+v; "+
			"expected the whole function to roll back as one transaction", before, after)
	}
}

// TestDropTenant_RoleDropFailureRollsBackEverything documents the second,
// compounding bug 032 also fixes (see its comment): the pre-032 DROP ROLE
// step fails whenever admin.create_tenant_role has ever been called for the
// tenant, because that role still holds table-level GRANTs and Postgres
// refuses to drop a role that holds privileges anywhere. Since a single
// top-level `SELECT admin.drop_tenant(...)` is one transaction, that
// failure rolls back every DELETE the function had already run --
// including the two rows (admin.tenant_roles, admin.tenants) the "silent"
// path above shows it otherwise deletes successfully.
func TestDropTenant_RoleDropFailureRollsBackEverything(t *testing.T) {
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	defer adminDB.Close()
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	resetToOriginal001DropTenant(t, adminDB)
	testutil.CleanupPostgresTestData(t, adminDB)
	defer testutil.CleanupPostgresTestData(t, adminDB)

	ctx := context.Background()
	const tenant = "01d00001-0000-4000-8000-000000000001"
	const defName = "drop-tenant-old-role-def"
	defer cleanupDropTenantExtras(t, ctx, adminDB, tenant)
	if err := NewPostgresStore(adminDB).DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
	dropTenantFixture(t, ctx, adminDB, tenant, defName, "oldrole", false, true)

	before := countDropTenantRows(t, ctx, adminDB, tenant)
	if before.total() == 0 {
		t.Fatalf("fixture setup produced no rows for tenant -- test proves nothing")
	}

	_, err := adminDB.ExecContext(ctx, `SELECT admin.drop_tenant($1)`, tenant)
	if err == nil {
		t.Fatalf("admin.drop_tenant (pre-032 version, tenant role provisioned) succeeded -- " +
			"expected a DROP ROLE failure ('cannot be dropped because some objects depend on it')")
	}

	after := countDropTenantRows(t, ctx, adminDB, tenant)
	if after != before {
		t.Errorf("admin.drop_tenant's error left partial changes -- before=%+v after=%+v; "+
			"expected the whole function to roll back as one transaction", before, after)
	}
}

// TestDropTenant_DeletesAllTenantData applies 032 and proves the fixed
// admin.drop_tenant deletes every row in every table it is responsible
// for, for exactly the targeted tenant, leaving a second tenant's rows
// completely untouched, and refuses to run against the default tenant.
func TestDropTenant_DeletesAllTenantData(t *testing.T) {
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	defer adminDB.Close()
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, adminDB)
	defer testutil.CleanupPostgresTestData(t, adminDB)
	apply032DropTenantMigration(t, adminDB)

	ctx := context.Background()
	const tenantA = "02d00000-0000-4000-8000-00000000002a"
	const tenantB = "02d00000-0000-4000-8000-00000000002b"
	const defName = "drop-tenant-new-def"
	// Registered before the fixtures below so cleanup still runs if a
	// fixture call itself fails partway through via t.Fatalf.
	defer cleanupDropTenantExtras(t, ctx, adminDB, tenantA)
	defer cleanupDropTenantExtras(t, ctx, adminDB, tenantB)
	if err := NewPostgresStore(adminDB).DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
	dropTenantFixture(t, ctx, adminDB, tenantA, defName, "a", true, true)
	dropTenantFixture(t, ctx, adminDB, tenantB, defName, "b", true, true)

	beforeA := countDropTenantRows(t, ctx, adminDB, tenantA)
	beforeB := countDropTenantRows(t, ctx, adminDB, tenantB)
	if beforeA.total() == 0 || beforeB.total() == 0 {
		t.Fatalf("fixture setup produced no rows (A=%+v B=%+v) -- test proves nothing", beforeA, beforeB)
	}

	// Default-tenant guard.
	if _, err := adminDB.ExecContext(ctx, `SELECT admin.drop_tenant($1)`, DefaultTenantUUID); err == nil {
		t.Fatalf("admin.drop_tenant(default tenant) succeeded -- it must refuse, since that UUID is " +
			"shared by every single-tenant deployment and by workflow_defs")
	}

	if _, err := adminDB.ExecContext(ctx, `SELECT admin.drop_tenant($1)`, tenantA); err != nil {
		t.Fatalf("admin.drop_tenant(tenant A): %v", err)
	}

	afterA := countDropTenantRows(t, ctx, adminDB, tenantA)
	if afterA.total() != 0 {
		t.Errorf("tenant A still has rows after admin.drop_tenant (want all zero): %+v", afterA)
	}

	// Cross-tenant safety: B's rows must be exactly what they were before,
	// counted from adminDB -- the superuser/schema-owner connection, which
	// RLS cannot filter, so this is a real count and not "invisible to a
	// tenant-scoped query."
	afterB := countDropTenantRows(t, ctx, adminDB, tenantB)
	if afterB != beforeB {
		t.Errorf("tenant B's rows changed after tenant A's drop_tenant: before=%+v after=%+v", beforeB, afterB)
	}
}
