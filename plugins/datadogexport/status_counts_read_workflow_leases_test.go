package datadogexport

import (
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// TestStatusCountsReadStatusFromWorkflowLeases pins cleat#3245 B3a for this
// plugin: on PostgreSQL the per-tenant status counts it exports read
// workflow_leases. One run's two copies disagree, with statuses no other test
// writes, so a statement still reading workflow_instances reports the other
// copy's status.
func TestStatusCountsReadStatusFromWorkflowLeases(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectPostgres)
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)

	id := fmt.Sprintf("datadog-b3a-%d", time.Now().UnixNano())
	if _, err := db.Exec(`
		INSERT INTO workflow_defs (name, version, wasm_bytes)
		VALUES ('datadog-b3a-def', 1, '\x00'::bytea)
		ON CONFLICT (tenant_id, name, version) DO NOTHING`); err != nil {
		t.Fatalf("seed workflow_defs: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO workflow_instances (id, def_name, def_version, status)
		VALUES ($1, 'datadog-b3a-def', 1, 'b3a-dd-instance-status')`, id); err != nil {
		t.Fatalf("seed workflow_instances: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, id) })
	testutil.SyncWorkflowLease(t, db, id)
	if _, err := db.Exec(`UPDATE workflow_leases SET status = 'b3a-dd-lease-status' WHERE id = $1`, id); err != nil {
		t.Fatalf("diverge workflow_leases: %v", err)
	}

	rows, err := db.Query(statusCountsSQL.For(plugin.DialectPostgres), engine.DefaultTenantUUID)
	if err != nil {
		t.Fatalf("status counts: %v", err)
	}
	defer rows.Close()
	counts := map[string]int64{}
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		counts[status] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if counts["b3a-dd-lease-status"] != 1 || counts["b3a-dd-instance-status"] != 0 {
		t.Fatalf("status counts = %v, want b3a-dd-lease-status=1 and no b3a-dd-instance-status -- it read workflow_instances.status", counts)
	}
}
