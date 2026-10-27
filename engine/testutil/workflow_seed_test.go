package testutil

import (
	"fmt"
	"testing"
	"time"
)

func uniqueLeaseTestID(label string) string {
	return fmt.Sprintf("testutil-lease-%s-%d", label, time.Now().UnixNano())
}

// TestWorkflowLeasesRequiresAMatchingInstanceRow proves workflow_leases' FK onto
// workflow_instances is real, which is what makes SeedRun's three-row layout
// meaningful rather than a write into tables with no constraints.
func TestWorkflowLeasesRequiresAMatchingInstanceRow(t *testing.T) {
	db := TestDB(t, DialectPostgres)
	id := uniqueLeaseTestID("orphan")

	_, err := db.Exec(`INSERT INTO workflow_leases (id) VALUES ($1)`, id)
	if err == nil {
		t.Fatal("inserting a workflow_leases row with no matching workflow_instances row succeeded, want a foreign key violation")
	}
}
