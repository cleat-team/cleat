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
// workflow_leases' own column default (migrations/postgres/016), which
// agrees with workflow_instances' default for every field here -- both
// tables default status to 'ready' and tenant_id to the zero UUID.
type WorkflowLeaseSeed struct {
	TenantID   string
	TaskQueue  string
	Status     string
	AssignedTo string
}

// SeedWorkflowLease inserts the workflow_leases row that must accompany a
// raw-seeded workflow_instances row sharing its id -- mirroring
// StartNewRun's dual-write (engine/store_lifecycle.go's
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
// PostgreSQL only: workflow_leases does not exist on MySQL or SQL Server. A
// fixture that seeds more than one dialect must gate this call itself --
// see tests/crash/dialects_test.go's crashTarget.startWorkflowEntry for the
// pattern. This function does not check the dialect.
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
}
