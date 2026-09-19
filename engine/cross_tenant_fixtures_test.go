package engine

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// PostgreSQL-only: admin.claim_workflows (migrations/postgres/023_cross_tenant_claim.sql)
// does not exist on MySQL or SQL Server, and neither dialect implements
// CrossTenantClaimer. These tests are written directly against *PostgresStore
// rather than looped over registeredBackends, so a run without
// CLEAT_TEST_MYSQL/CLEAT_TEST_MSSQL set doesn't pay a skip for dialects that
// were never going to run this feature in the first place.

// xtcTenantA and xtcTenantB are two distinct tenants used throughout this
// file. workflow_instances.tenant_id carries no foreign key into
// admin.tenants (only admin.tenant_roles/admin.tenant_api_keys do), so
// fixtures below can use any well-formed UUID here without first registering
// a tenant row.
const (
	xtcTenantA = "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"
	xtcTenantB = "bbbbbbbb-bbbb-4bbb-bbbb-bbbbbbbbbbbb"
)

// xtcDefName/xtcDefVersion identify the workflow_defs rows most fixtures below
// reference -- one PER TENANT, not one shared row. See deployCrossTenantDef,
// which says the same thing where the work happens.
//
// This comment used to say the opposite, and the correction is worth keeping
// rather than just deleting: workflow_defs was a shared/global registry keyed
// by (name, version), so a single row deployed by the default tenant satisfied
// every tenant's foreign key. D7 (IMPROVEMENT-PLAN 3.77, migration
// postgres/035) made the key (tenant_id, name, version) and widened
// workflow_instances' FK to match, so that is no longer true. The helper was
// updated when D7 landed; this header was not, and for a day it contradicted
// the function sixty lines below it.
const (
	xtcDefName    = "cross-tenant-claim-test"
	xtcDefVersion = 1
)

// crossTenantClaimDB provisions two connections against the same schema.
//
// adminDB is the superuser/owner connection SetupFullSchema needs, and every
// raw fixture INSERT in this file uses it: it bypasses RLS, so a fixture row
// can carry any tenant_id regardless of what the connection itself is scoped
// to.
//
// appDB is a *separate* connection authenticated as
// testutil.PostgresRLSTestRole -- an ordinary, non-owning role that Postgres
// always subjects to Row-Level Security. It matters here specifically:
// ClaimWorkflows carries no `WHERE tenant_id = ...` in its own SQL (see
// engine/store_lifecycle.go) and relies entirely on the
// tenant_isolation_instances RLS policy to keep tenants apart. Calling it
// through adminDB would "prove" tenant isolation without RLS ever being
// evaluated -- a superuser bypasses it unconditionally -- so the contrast
// this file draws between ClaimWorkflows and ClaimWorkflowsAcrossTenants
// would be meaningless. See testutil.OpenPostgresRLSTestDB and the same
// pattern in engine/integration_test.go's TestRLSTenantIsolation.
//
// admin.claim_workflows needs its own grant beyond what SetupPostgresRLSRole
// gives the role: EXECUTE on the function itself, and USAGE on the admin
// schema it lives in. Neither is part of the role's deliberately-minimal
// baseline, so this file adds them itself rather than touching
// engine/testutil.
func crossTenantClaimDB(t *testing.T) (adminDB, appDB *sql.DB, teardown func()) {
	t.Helper()
	adminDB = testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, adminDB)

	appDB = testutil.OpenPostgresRLSTestDB(t, adminDB)

	for _, stmt := range []string{
		`GRANT USAGE ON SCHEMA admin TO ` + testutil.PostgresRLSTestRole,
		`GRANT EXECUTE ON FUNCTION admin.claim_workflows(text, text[], integer) TO ` + testutil.PostgresRLSTestRole,
		// 024's function, granted here for the same reason: the RLS test role's
		// baseline is deliberately minimal and neither grant is part of it.
		`GRANT EXECUTE ON FUNCTION admin.get_due_schedules() TO ` + testutil.PostgresRLSTestRole,
	} {
		if _, err := adminDB.Exec(stmt); err != nil {
			t.Fatalf("grant cross-tenant claim access to the RLS test role: %v\nstatement: %s", err, stmt)
		}
	}

	return adminDB, appDB, func() {
		appDB.Close()
		testutil.CleanupPostgresTestData(t, adminDB)
		adminDB.Close()
	}
}

// deployCrossTenantDef deploys the shared (xtcDefName, xtcDefVersion) row
// every plain seedWorkflow fixture below depends on via the FK on
// workflow_instances.
func deployCrossTenantDef(t *testing.T, adminDB *sql.DB) {
	t.Helper()
	// One definition PER TENANT, not one shared definition.
	//
	// Until D7 (IMPROVEMENT-PLAN 3.77) workflow_defs was keyed by
	// (name, version) with no tenant, so a single row satisfied every tenant's
	// foreign key and this helper deployed once. Under
	// (tenant_id, name, version) each tenant owns its own row, and a workflow
	// started by tenant B against tenant A's definition is refused by
	// workflow_instances_def_fkey -- which is the point of the change, so the
	// fixture moves rather than the constraint.
	for _, tenant := range []string{DefaultTenantUUID, xtcTenantA, xtcTenantB} {
		if err := NewPostgresStore(adminDB).WithTenant(tenant).DeployWorkflowDef(
			context.Background(), &WorkflowDef{
				Name:       xtcDefName,
				Version:    xtcDefVersion,
				WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1,
				MinVersion: 1,
			}); err != nil {
			t.Fatalf("deploy cross-tenant test def for %s: %v", tenant, err)
		}
	}
}

var xtcIDCounter int64

// xtcNextID returns a unique fixture ID. time.Now().UnixNano() alone can
// collide when a test seeds several rows back to back faster than the clock
// advances; the counter guarantees uniqueness regardless of clock
// resolution.
func xtcNextID(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), atomic.AddInt64(&xtcIDCounter, 1))
}

// seedWorkflowOpts controls the columns a seeded workflow_instances row
// carries. Any zero-valued field falls back to a default that keeps the row
// claimable but does not otherwise matter to the test using it.
type seedWorkflowOpts struct {
	tenantID   string // required
	taskQueue  string // default "default"
	priority   int
	generation int64
	errorCode  string
	errorOp    string
	traceID    string
	input      string    // default "{}"
	nextWakeAt time.Time // default: one minute in the past
	createdAt  time.Time // default: one minute in the past
}

// seedWorkflow inserts one ready, immediately-claimable workflow_instances
// row directly through adminDB and returns its id.
//
// It writes through raw SQL rather than through a store's StartNewRun
// deliberately: that path only ever writes the calling store's own
// tenant_id, and has no way to set error_code/error_op/trace_id/generation/
// created_at on an otherwise-ready row -- all of which
// TestClaimWorkflowsAcrossTenants_ColumnsMatchTheGoScan needs full,
// independent control over so a column-list/scan mismatch shows up as a
// wrong value rather than being masked by whatever StartNewRun happens to
// write.
func seedWorkflow(t *testing.T, adminDB *sql.DB, o seedWorkflowOpts) string {
	t.Helper()
	if o.tenantID == "" {
		t.Fatal("seedWorkflow: tenantID is required")
	}
	if o.taskQueue == "" {
		o.taskQueue = "default"
	}
	if o.input == "" {
		o.input = "{}"
	}
	if o.nextWakeAt.IsZero() {
		o.nextWakeAt = time.Now().Add(-time.Minute).UTC().Truncate(time.Microsecond)
	}
	if o.createdAt.IsZero() {
		o.createdAt = time.Now().Add(-time.Minute).UTC().Truncate(time.Microsecond)
	}

	id := xtcNextID("xtc")
	if _, err := adminDB.Exec(`
		INSERT INTO workflow_instances
			(id, def_name, def_version, status, input, tenant_id, task_queue,
			 next_wake_at, created_at, error_code, error_op, priority, generation, trace_id)
		VALUES ($1, $2, $3, 'ready', $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
	`, id, xtcDefName, xtcDefVersion, o.input, o.tenantID, o.taskQueue,
		o.nextWakeAt, o.createdAt, o.errorCode, o.errorOp, o.priority, o.generation, o.traceID); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
	return id
}

// claimedIDs formats a claimed slice for failure messages.
func claimedIDs(wfs []*WorkflowInstance) []string {
	ids := make([]string, len(wfs))
	for i, wf := range wfs {
		ids[i] = wf.ID
	}
	return ids
}
