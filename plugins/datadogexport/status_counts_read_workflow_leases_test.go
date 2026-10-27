package datadogexport

import (
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// TestStatusCountsReadStatusFromWorkflowLeases is the only test that runs this
// plugin's PostgreSQL status-count statement against a real schema; the
// behavioural suite's fake driver matches the query text and would pass whatever
// table it named. Since cleat#3245 B5 workflow_instances has no status, so a
// statement pointed back at it is a runtime `column "status" does not exist`.
// The status is one no other test writes, so the count is about this run alone.
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
	testutil.SeedRun(t, db, testutil.RunSeed{ID: id, DefName: "datadog-b3a-def", Status: "b3a-dd-lease-status"})
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, id) })

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
	if counts["b3a-dd-lease-status"] != 1 {
		t.Fatalf("status counts = %v, want b3a-dd-lease-status=1", counts)
	}
}
