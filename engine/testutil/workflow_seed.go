package testutil

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
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
	cols := []string{"id"}
	args := []any{id}
	add := func(col, val string) {
		if val == "" {
			return
		}
		cols = append(cols, col)
		args = append(args, val)
	}
	add("tenant_id", seed.TenantID)
	add("task_queue", seed.TaskQueue)
	add("status", seed.Status)
	add("assigned_to", seed.AssignedTo)

	placeholders := make([]string, len(cols))
	for i := range cols {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}
	query := fmt.Sprintf(`
		INSERT INTO workflow_leases (%s)
		VALUES (%s)
		ON CONFLICT (id) DO NOTHING
	`, strings.Join(cols, ", "), strings.Join(placeholders, ", "))

	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("seeding %s's workflow_leases row: %v", id, err)
	}
}
