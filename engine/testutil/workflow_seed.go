package testutil

import (
	"database/sql"
	"testing"
)

// These three match workflow_leases' and workflow_instances' own column
// defaults (migrations/postgres/001 and 016) exactly -- COALESCE below falls
// back to these literals rather than the table's DEFAULT clause because a
// column given an explicit (even empty-string) argument never reaches its
// DEFAULT. Re-derive if either migration's defaults ever change.
const (
	defaultLeaseTenantID  = "00000000-0000-0000-0000-000000000000"
	defaultLeaseTaskQueue = "default"
	defaultLeaseStatus    = "ready"
)

// WorkflowLeaseSeed describes the workflow_leases row to create alongside a
// raw-seeded workflow_instances row of the same id. A zero-value field takes
// workflow_leases' own column default (migrations/postgres/016, folded
// into migrations/postgres/001_schema.sql by #3358), which
// agrees with workflow_instances' default for every field here -- both
// tables default status to 'ready' and tenant_id to the zero UUID.
type WorkflowLeaseSeed struct {
	TenantID   string
	TaskQueue  string
	Status     string
	AssignedTo string
}

// SeedWorkflowLease inserts the workflow_leases AND workflow_payloads rows
// that must accompany a raw-seeded workflow_instances row sharing its id --
// mirroring StartNewRun's dual-write (engine/store_lifecycle.go's
// insertLeaseAndPayloadRows). Call it immediately after the
// workflow_instances INSERT, with the same id and whatever of
// TenantID/TaskQueue/Status/AssignedTo that INSERT set explicitly.
//
// cleat#3245 Phase 3: ClaimWorkflows' own mirror write (claimLeaseRows) is a
// plain UPDATE, which is a silent no-op against a row that was never
// inserted here -- so a fixture that skips this call does not fail loudly,
// it fails the moment something reads the candidate set from
// workflow_leases instead of workflow_instances (piece A's
// ReapStaleInstances/ReapStaleInstancesExcept/ListStaleHolders cutover, and
// piece B's claim-path cutover next). See cleat#3245's tracking issue for
// the three rounds this was found the hard way before becoming a helper.
//
// The workflow_payloads row was missing from this function until Phase 3
// step 3 piece F, which is the first thing to make its absence loud: piece
// F's ListWorkflows/CountWorkflows/GetWorkflowByID read workflow_leases
// JOIN workflow_payloads, an INNER join, so a lease with no matching
// payload row is invisible to all three -- every one of this function's 25+
// callers across engine/, tests/ and plugins/ seeded a lease-only row and
// would otherwise have reported every seeded workflow "not found" the
// moment any of their tests reached GetWorkflowByID. This function's own
// doc comment already called itself a "dual-write" mirror before piece F;
// it only became one here.
//
// PostgreSQL only: workflow_leases and workflow_payloads do not exist on
// MySQL or SQL Server. A fixture that seeds more than one dialect must gate
// this call itself -- see tests/crash/dialects_test.go's
// crashTarget.startWorkflowEntry for the pattern. This function does not
// check the dialect.
func SeedWorkflowLease(t *testing.T, db *sql.DB, id string, seed WorkflowLeaseSeed) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO workflow_leases (id, tenant_id, task_queue, status, assigned_to)
		VALUES (
			$1,
			COALESCE(NULLIF($2, ''), $6)::uuid,
			COALESCE(NULLIF($3, ''), $7),
			COALESCE(NULLIF($4, ''), $8),
			NULLIF($5, '')
		)
		ON CONFLICT (id) DO NOTHING
	`,
		id, seed.TenantID, seed.TaskQueue, seed.Status, seed.AssignedTo,
		defaultLeaseTenantID, defaultLeaseTaskQueue, defaultLeaseStatus,
	); err != nil {
		t.Fatalf("seeding %s's workflow_leases row: %v", id, err)
	}
	// Same tenant_id COALESCE as the workflow_leases insert above, so the two
	// rows agree -- the join piece F added has no predicate to notice if they
	// did not, but RLS does.
	//
	// def_name/def_version come from the caller's own workflow_instances
	// INSERT via subquery, not from this function's arguments -- it takes
	// neither. Both are NOT a nullable column on the Go side
	// (WorkflowInstance.DefName/.DefVersion scan into plain string/int), so
	// leaving them unset here is not merely incomplete, it is a Scan error
	// ("converting NULL to string is unsupported") the first time
	// GetWorkflowByID or ListWorkflows reads the row -- measured via
	// tests/scale's TestMaxConcurrentWorkflows while fixing the "not found"
	// half of this same gap.
	if _, err := db.Exec(`
		INSERT INTO workflow_payloads (id, tenant_id, input, def_name, def_version)
		SELECT $1, COALESCE(NULLIF($2, ''), $3)::uuid, '{}', wi.def_name, wi.def_version
		FROM workflow_instances wi WHERE wi.id = $1
		ON CONFLICT (id) DO NOTHING
	`,
		id, seed.TenantID, defaultLeaseTenantID,
	); err != nil {
		t.Fatalf("seeding %s's workflow_payloads row: %v", id, err)
	}
}
