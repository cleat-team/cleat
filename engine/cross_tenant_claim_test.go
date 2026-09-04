package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lib/pq"

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

// TestClaimWorkflowsAcrossTenants_SeesAllTenants is the property the whole
// migration exists for, proven by direct contrast rather than by trusting
// each half in isolation.
//
// An ordinary, tenant-scoped ClaimWorkflows can only ever see its own
// tenant's ready work -- that's what RLS is for -- so a non-default tenant's
// workflows never execute unless something else can see across the
// boundary. ClaimWorkflowsAcrossTenants is that something else: one call
// against ready rows seeded for two different tenants must return both,
// while the ordinary claim, run through the same real RLS enforcement (see
// crossTenantClaimDB), must return only its own.
func TestClaimWorkflowsAcrossTenants_SeesAllTenants(t *testing.T) {
	adminDB, appDB, teardown := crossTenantClaimDB(t)
	defer teardown()
	deployCrossTenantDef(t, adminDB)
	ctx := context.Background()

	// Pair 1: the cross-tenant claim must see ready work from both tenants
	// in a single call.
	crossA := seedWorkflow(t, adminDB, seedWorkflowOpts{tenantID: xtcTenantA})
	crossB := seedWorkflow(t, adminDB, seedWorkflowOpts{tenantID: xtcTenantB})

	cross := NewPostgresStore(appDB)
	claimed, err := cross.ClaimWorkflowsAcrossTenants(ctx, "worker-cross", 10)
	if err != nil {
		t.Fatalf("ClaimWorkflowsAcrossTenants: %v", err)
	}
	gotCross := map[string]bool{}
	for _, wf := range claimed {
		gotCross[wf.ID] = true
	}
	if len(claimed) != 2 || !gotCross[crossA] || !gotCross[crossB] {
		t.Fatalf("ClaimWorkflowsAcrossTenants claimed %v, want exactly [%s %s] -- "+
			"one call must see ready work from every tenant",
			claimedIDs(claimed), crossA, crossB)
	}

	// Pair 2: the ordinary, tenant-scoped claim must NOT. A fresh pair,
	// rather than reusing pair 1, so this half doesn't depend on pair 1
	// having already removed tenant A's row from the ready pool.
	ordA := seedWorkflow(t, adminDB, seedWorkflowOpts{tenantID: xtcTenantA})
	ordB := seedWorkflow(t, adminDB, seedWorkflowOpts{tenantID: xtcTenantB})

	storeA := NewPostgresStore(appDB).WithTenant(xtcTenantA)
	ordinary, err := storeA.ClaimWorkflows(ctx, "worker-ordinary", 10)
	if err != nil {
		t.Fatalf("ClaimWorkflows (tenant-scoped): %v", err)
	}
	if len(ordinary) != 1 || ordinary[0].ID != ordA {
		t.Fatalf("tenant-scoped ClaimWorkflows returned %v, want exactly [%s] -- "+
			"it must never see tenant B's ready workflow %s",
			claimedIDs(ordinary), ordA, ordB)
	}
}

// TestClaimWorkflowsAcrossTenants_UngrantedDeploymentFallsBack covers what a
// deployment that turned the flag on without applying and granting migration
// 023 actually gets.
//
// The answer has to be ErrCrossTenantClaimUnsupported and not a raw error,
// because that sentinel is what cmd/cleat-worker's claimGeneral keys on: it
// warns once and falls back to the per-tenant claim, so dispatch keeps running
// on what this connection can legitimately see. A raw error propagates instead,
// and the worker fails every poll -- a missing GRANT would take dispatch down
// rather than narrow it.
//
// Revoking EXECUTE is the reversible half of the provisioning gap and reaches
// the real code path (42501, insufficient_privilege). The absent-function half
// (42883) would mean dropping a function the migration owns, so it is covered
// by the table-driven test below instead; this one proves the mapping is
// actually consulted on the live path, which is the part that could rot.
func TestClaimWorkflowsAcrossTenants_UngrantedDeploymentFallsBack(t *testing.T) {
	adminDB, appDB, teardown := crossTenantClaimDB(t)
	defer teardown()
	deployCrossTenantDef(t, adminDB)
	ctx := context.Background()

	seedWorkflow(t, adminDB, seedWorkflowOpts{tenantID: xtcTenantA})

	store := NewPostgresStore(appDB)

	// Sanity first: with the grant in place the claim works. Without this, a
	// revoke that silently did nothing would still leave the assertion below
	// looking like it passed for the right reason.
	if _, err := store.ClaimWorkflowsAcrossTenants(ctx, "worker-granted", 10); err != nil {
		t.Fatalf("with EXECUTE granted the claim should work, got: %v", err)
	}

	revoke := `REVOKE EXECUTE ON FUNCTION admin.claim_workflows(text, text[], integer) FROM ` + testutil.PostgresRLSTestRole
	if _, err := adminDB.Exec(revoke); err != nil {
		t.Fatalf("revoke EXECUTE: %v", err)
	}
	// defer, not t.Cleanup: t.Cleanup runs after this function's defers, and
	// teardown above has closed adminDB by then -- the restore silently failed
	// with "sql: database is closed". The grant is database state, not test
	// state, so leaving it revoked would leak into whatever runs next against
	// the same database.
	defer func() {
		grant := `GRANT EXECUTE ON FUNCTION admin.claim_workflows(text, text[], integer) TO ` + testutil.PostgresRLSTestRole
		if _, err := adminDB.Exec(grant); err != nil {
			t.Errorf("restoring the EXECUTE grant failed, so the next test against this "+
				"database starts from a revoked one: %v", err)
		}
	}()

	_, err := store.ClaimWorkflowsAcrossTenants(ctx, "worker-ungranted", 10)
	if err == nil {
		t.Fatal("claiming across tenants without EXECUTE returned no error; the worker would " +
			"believe it had swept every tenant using a function it may not call")
	}
	if !errors.Is(err, ErrCrossTenantClaimUnsupported) {
		t.Fatalf("error is %v, want ErrCrossTenantClaimUnsupported -- without that sentinel "+
			"cmd/cleat-worker propagates instead of falling back, so a missing GRANT stops "+
			"dispatch entirely rather than narrowing it to one tenant", err)
	}
	// The remediation has to travel with it: this message is the entire content
	// of the one WARN line the worker logs.
	if !strings.Contains(err.Error(), "023_cross_tenant_claim.sql") {
		t.Errorf("error does not name the migration that fixes it: %v", err)
	}
}

// TestCrossTenantProvisioningGap_MapsBothCodes pins the three SQLSTATEs to the
// meaning the fallback depends on, including the negative case.
//
// The negative case is the one that matters. crossTenantProvisioningGap
// returning non-empty for an unanticipated error would downgrade a real claim
// failure into a silent fallback to the narrower claim -- which is the exact
// class of bug this whole mechanism exists to prevent, arriving through the
// mechanism itself.
func TestCrossTenantProvisioningGap_MapsBothCodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"undefined_function: migration 023 never applied", &pq.Error{Code: "42883"}, true},
		{"insufficient_privilege: EXECUTE never granted", &pq.Error{Code: "42501"}, true},
		// 023 applied, 040 not: the function exists and returns 14 columns,
		// so asking it for pending_terminal_status is undefined_column.
		// IMPROVEMENT-PLAN 3.112.
		{"undefined_column: migration 040 never applied", &pq.Error{Code: "42703"}, true},
		{"deadlock: a real claim failure, must propagate", &pq.Error{Code: "40P01"}, false},
		{"unique_violation: a real claim failure, must propagate", &pq.Error{Code: "23505"}, false},
		{"not a pq error at all", errors.New("connection refused"), false},
		{"wrapped undefined_function still counts", fmt.Errorf("claim: %w", &pq.Error{Code: "42883"}), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := crossTenantProvisioningGap(tc.err) != ""; got != tc.want {
				t.Errorf("crossTenantProvisioningGap(%v) treated as provisioning gap = %v, want %v",
					tc.err, got, tc.want)
			}
		})
	}
}

// TestClaimWorkflowsAcrossTenants_ClaimedRowsCarryTenantID checks the field
// the whole design depends on downstream. cmd/cleat-worker's storeForTenant
// re-scopes to each returned instance's TenantID before touching it again
// (see the CrossTenantClaimer doc comment in engine/store_lifecycle.go); a
// claimed row with an empty or wrong TenantID would be silently routed to
// the wrong store -- or none.
func TestClaimWorkflowsAcrossTenants_ClaimedRowsCarryTenantID(t *testing.T) {
	adminDB, appDB, teardown := crossTenantClaimDB(t)
	defer teardown()
	deployCrossTenantDef(t, adminDB)
	ctx := context.Background()

	wfA := seedWorkflow(t, adminDB, seedWorkflowOpts{tenantID: xtcTenantA})
	wfB := seedWorkflow(t, adminDB, seedWorkflowOpts{tenantID: xtcTenantB})

	cross := NewPostgresStore(appDB)
	claimed, err := cross.ClaimWorkflowsAcrossTenants(ctx, "worker-tenantid", 10)
	if err != nil {
		t.Fatalf("ClaimWorkflowsAcrossTenants: %v", err)
	}

	want := map[string]string{wfA: xtcTenantA, wfB: xtcTenantB}
	if len(claimed) != len(want) {
		t.Fatalf("claimed %d workflows, want %d (%v)", len(claimed), len(want), claimedIDs(claimed))
	}
	for _, wf := range claimed {
		wantTenant, ok := want[wf.ID]
		if !ok {
			t.Errorf("claimed unexpected workflow %q", wf.ID)
			continue
		}
		if wf.TenantID == "" {
			t.Errorf("claimed workflow %q has an empty TenantID, want %q", wf.ID, wantTenant)
		} else if wf.TenantID != wantTenant {
			t.Errorf("workflow %q: TenantID = %q, want %q", wf.ID, wf.TenantID, wantTenant)
		}
	}
}

// TestClaimWorkflowsAcrossTenants_ColumnsMatchTheGoScan pins
// admin.claim_workflows's RETURNS TABLE column list (a migration) to
// scanClaimedWorkflows's Scan call (Go) -- nothing else keeps the two in
// step. 023_cross_tenant_claim.sql names this test by name in its own
// comment.
//
// Every field below is a distinct, non-zero sentinel rather than a
// plausible-looking default. That matters because a reordered column list
// of compatible types (e.g. two adjacent text columns, or two adjacent
// integer columns, swapped) scans without error -- Scan only checks that
// the Go type on each side is convertible, not that it's the RIGHT value --
// so only checking every field independently catches a transposition. A
// test that merely asserted "the scan didn't error" would pass on a
// silently-wrong mapping.
func TestClaimWorkflowsAcrossTenants_ColumnsMatchTheGoScan(t *testing.T) {
	adminDB, appDB, teardown := crossTenantClaimDB(t)
	defer teardown()
	ctx := context.Background()

	const (
		wantDefName    = "colcheck-def-name"
		wantErrorCode  = "colcheck-error-code"
		wantErrorOp    = "colcheck-error-op"
		wantTraceID    = "colcheck-trace-id"
		wantPriority   = 7
		wantGeneration = int64(5) // the claim does generation+1; want 6 back.
		wantInput      = `{"marker":"colcheck-input"}`
		wantTenant     = xtcTenantA
		workerID       = "worker-colcheck"
	)
	wantNextWakeAt := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Microsecond)
	wantCreatedAt := time.Now().Add(-3 * time.Hour).UTC().Truncate(time.Microsecond)

	// A def distinct from xtcDefName, so def_name really is an
	// end-to-end-unique sentinel and not something another fixture also
	// happens to write.
	// Deployed as wantTenant, because the instance below is that tenant's and
	// workflow_instances_def_fkey now carries tenant_id (D7 / 3.77).
	if err := NewPostgresStore(adminDB).WithTenant(wantTenant).DeployWorkflowDef(ctx, &WorkflowDef{
		Name:       wantDefName,
		Version:    1,
		WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1,
		MinVersion: 1,
	}); err != nil {
		t.Fatalf("deploy sentinel def: %v", err)
	}

	id := xtcNextID("colcheck")
	if _, err := adminDB.Exec(`
		INSERT INTO workflow_instances
			(id, def_name, def_version, status, input, tenant_id, task_queue,
			 next_wake_at, created_at, error_code, error_op, priority, generation, trace_id)
		VALUES ($1, $2, 1, 'ready', $3, $4, 'default', $5, $6, $7, $8, $9, $10, $11)
	`, id, wantDefName, wantInput, wantTenant, wantNextWakeAt, wantCreatedAt,
		wantErrorCode, wantErrorOp, wantPriority, wantGeneration, wantTraceID); err != nil {
		t.Fatalf("seed sentinel workflow: %v", err)
	}

	claimed, err := NewPostgresStore(appDB).ClaimWorkflowsAcrossTenants(ctx, workerID, 10)
	if err != nil {
		t.Fatalf("ClaimWorkflowsAcrossTenants: %v", err)
	}
	var wf *WorkflowInstance
	for _, c := range claimed {
		if c.ID == id {
			wf = c
		}
	}
	if wf == nil {
		t.Fatalf("sentinel workflow %q was not claimed (claimed: %v)", id, claimedIDs(claimed))
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"ID", wf.ID, id},
		{"DefName", wf.DefName, wantDefName},
		{"DefVersion", wf.DefVersion, 1},
		{"Status", wf.Status, "running"},        // set by the claim itself, not the seed.
		{"AssignedTo", wf.AssignedTo, workerID}, // ditto.
		{"TenantID", wf.TenantID, wantTenant},
		{"ErrorCode", wf.ErrorCode, wantErrorCode},
		{"ErrorOp", wf.ErrorOp, wantErrorOp},
		{"Priority", wf.Priority, wantPriority},
		{"Generation", wf.Generation, wantGeneration + 1},
		{"TraceID", wf.TraceID, wantTraceID},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v -- a column-list/scan mismatch surfaces as exactly "+
				"this: a value read out of the wrong field", c.name, c.got, c.want)
		}
	}
	// JSONB round-trips through Postgres with normalized whitespace (a
	// space after each ":"), so compare parsed values rather than raw
	// bytes -- a byte-for-byte comparison here would fail on formatting,
	// not content, and mask the check this field exists for.
	var gotInput, wantInputParsed map[string]any
	if err := json.Unmarshal(wf.Input, &gotInput); err != nil {
		t.Fatalf("Input is not valid JSON: %v (%s)", err, wf.Input)
	}
	if err := json.Unmarshal([]byte(wantInput), &wantInputParsed); err != nil {
		t.Fatalf("test bug: wantInput is not valid JSON: %v", err)
	}
	if gotInput["marker"] != wantInputParsed["marker"] {
		t.Errorf("Input marker = %v, want %v (full Input: %s)", gotInput["marker"], wantInputParsed["marker"], wf.Input)
	}
	// next_wake_at and created_at are not touched by the claim (only
	// status/assigned_to/heartbeat_at/generation are), so these must come
	// back exactly as seeded.
	if !wf.NextWakeAt.Equal(wantNextWakeAt) {
		t.Errorf("NextWakeAt = %s, want %s (the claim does not write this column)",
			wf.NextWakeAt.Format(time.RFC3339Nano), wantNextWakeAt.Format(time.RFC3339Nano))
	}
	if !wf.CreatedAt.Equal(wantCreatedAt) {
		t.Errorf("CreatedAt = %s, want %s (the claim does not write this column)",
			wf.CreatedAt.Format(time.RFC3339Nano), wantCreatedAt.Format(time.RFC3339Nano))
	}
}

// TestClaimWorkflowsAcrossTenants_RespectsLimitAndTaskQueue checks the two
// ordinary claim behaviors that removing the tenant boundary must not also
// remove: the store's configured task_queue set still filters which rows
// are even candidates, and p_limit still bounds how many of them come back
// in one call (the rest stay ready rather than being claimed and lost).
func TestClaimWorkflowsAcrossTenants_RespectsLimitAndTaskQueue(t *testing.T) {
	adminDB, appDB, teardown := crossTenantClaimDB(t)
	defer teardown()
	deployCrossTenantDef(t, adminDB)
	ctx := context.Background()

	// Three ready workflows on "queue-a", spread across two tenants --
	// cross-tenant claiming removes the tenant filter, not the queue one.
	queueA := []string{
		seedWorkflow(t, adminDB, seedWorkflowOpts{tenantID: xtcTenantA, taskQueue: "queue-a"}),
		seedWorkflow(t, adminDB, seedWorkflowOpts{tenantID: xtcTenantB, taskQueue: "queue-a"}),
		seedWorkflow(t, adminDB, seedWorkflowOpts{tenantID: xtcTenantA, taskQueue: "queue-a"}),
	}
	// Two more, ready, on a queue this store never polls.
	seedWorkflow(t, adminDB, seedWorkflowOpts{tenantID: xtcTenantA, taskQueue: "queue-b"})
	seedWorkflow(t, adminDB, seedWorkflowOpts{tenantID: xtcTenantB, taskQueue: "queue-b"})

	inQueueA := map[string]bool{}
	for _, id := range queueA {
		inQueueA[id] = true
	}

	cross := NewPostgresStore(appDB, "queue-a")
	claimed, err := cross.ClaimWorkflowsAcrossTenants(ctx, "worker-queue", 2)
	if err != nil {
		t.Fatalf("ClaimWorkflowsAcrossTenants: %v", err)
	}
	if len(claimed) != 2 {
		t.Fatalf("claimed %d workflows, want exactly 2 (the limit): %v", len(claimed), claimedIDs(claimed))
	}
	for _, wf := range claimed {
		if !inQueueA[wf.ID] {
			t.Errorf("claimed workflow %q, which is not one of queue-a's ready rows -- "+
				"the task_queue filter did not apply", wf.ID)
		}
	}

	// The third queue-a row must still be ready: the limit released it
	// rather than claiming (and thereby hiding) it.
	remaining, err := cross.ClaimWorkflowsAcrossTenants(ctx, "worker-queue-2", 10)
	if err != nil {
		t.Fatalf("ClaimWorkflowsAcrossTenants (remainder): %v", err)
	}
	if len(remaining) != 1 || !inQueueA[remaining[0].ID] {
		t.Fatalf("after claiming 2 of 3 queue-a rows, expected exactly 1 remaining "+
			"queue-a row claimable, got %v", claimedIDs(remaining))
	}
}

// TestClaimWorkflowsAcrossTenants_ExactlyOneOfNConcurrentClaimsWins is the
// FOR UPDATE SKIP LOCKED guarantee, generalized to eight concurrent workers
// -- modeled on schedule_claim_test.go's
// TestClaimDueSchedule_ExactlyOneOfNConcurrentClaimsWins. Removing the
// tenant boundary must not create a new way for two workers to be handed
// the same workflow.
func TestClaimWorkflowsAcrossTenants_ExactlyOneOfNConcurrentClaimsWins(t *testing.T) {
	adminDB, appDB, teardown := crossTenantClaimDB(t)
	defer teardown()
	deployCrossTenantDef(t, adminDB)
	ctx := context.Background()

	wfID := seedWorkflow(t, adminDB, seedWorkflowOpts{tenantID: xtcTenantA})
	cross := NewPostgresStore(appDB)

	const workers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		wins    int
		errs    []error
		release = make(chan struct{})
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-release // start together, to make the race as tight as possible.
			claimed, err := cross.ClaimWorkflowsAcrossTenants(ctx, fmt.Sprintf("worker-%d", i), 1)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			for _, wf := range claimed {
				if wf.ID == wfID {
					wins++
				}
			}
		}(i)
	}
	close(release)
	wg.Wait()

	for _, err := range errs {
		t.Errorf("concurrent claim error: %v", err)
	}
	if wins != 1 {
		t.Errorf("%d of %d concurrent cross-tenant claims won the same workflow, want exactly 1", wins, workers)
	}
}
