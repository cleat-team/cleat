package engine

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// PostgreSQL-only: these fixtures were built for admin.claim_workflows, which
// cleat#3444 dropped, and the schema they read is still Postgres-only. #1926
// retired the CrossTenantClaimer interface that once made this dialect-specific --
// unconditional per-tenant rotation needs no such interface at all. These
// tests are written directly against *PostgresStore rather than looped over
// registeredBackends, so a run without CLEAT_TEST_MYSQL/CLEAT_TEST_MSSQL set
// doesn't pay a skip for a dialect that was never going to run this SQL in
// the first place. crossTenantClaimDB, the helper that provisioned an RLS
// connection and granted it EXECUTE on that function, was removed with it
// (cleat#3453): it had no caller and its GRANT would have failed.

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
// created_at on an otherwise-ready row -- all of which a column-list/scan
// parity test for the retired cross-tenant claim path (#1926) needed full,
// independent control over so a mismatch would show up as a wrong value
// rather than being masked by whatever StartNewRun happens to
// write. That test is gone; today's only caller
// (a_tenant_list_needs_no_rls_exemption_test.go) sets just tenantID, so
// errorCode/errorOp/traceID/generation currently go unexercised by anything
// in the tree.
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
	// workflow_instances carries only id, tenant_id, parent_close_policy,
	// event_count and history_swept_at (cleat#3245 B5); everything else this
	// fixture controls is written straight to its owning table below.
	if _, err := adminDB.Exec(`
		INSERT INTO workflow_instances (id, tenant_id)
		VALUES ($1, $2)
	`, id, o.tenantID); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
	// cleat#3245 Phase 3 step 3 piece B: ClaimWorkflows' candidate scan now
	// reads status/next_wake_at/task_queue/generation from workflow_leases,
	// not workflow_instances -- mirror the subset this fixture controls, or
	// a row seeded here is never claimable. error_code/error_op/trace_id
	// have no workflow_leases column (not needed by the claim predicate),
	// so they are not mirrored.
	if _, err := adminDB.Exec(`
		INSERT INTO workflow_leases
			(id, status, tenant_id, task_queue, next_wake_at, created_at, priority, generation)
		VALUES ($1, 'ready', $2, $3, $4, $5, $6, $7)
	`, id, o.tenantID, o.taskQueue, o.nextWakeAt, o.createdAt, o.priority, o.generation); err != nil {
		t.Fatalf("seed workflow_leases: %v", err)
	}
	// cleat#3245 B1a: the claim hands back input, def_name, def_version,
	// error_code, error_op and trace_id from workflow_payloads, and refuses a
	// run that has no row there -- so the fields this fixture controls are
	// mirrored onto it too.
	if _, err := adminDB.Exec(`
		INSERT INTO workflow_payloads
			(id, tenant_id, input, def_name, def_version, error_code, error_op, trace_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, id, o.tenantID, o.input, xtcDefName, xtcDefVersion, o.errorCode, o.errorOp, o.traceID); err != nil {
		t.Fatalf("seed workflow_payloads: %v", err)
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
