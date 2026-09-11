package engine

// cleat#1265. On SQL Server a retention sweep deleted the workflow_instances
// row and left every table it owned behind -- measured at 6 of 6, including the
// whole of event_history.
//
// SQL Server declares no foreign keys to workflow_instances (0, against 5 for
// each of the other dialects), so nothing cascades and DeleteCompletedWorkflows
// issued a bare DELETE FROM workflow_instances. Deleting the instance row also
// removes what made those child rows reachable, so they were not merely retained
// but unreachable AND permanent, while the falling instance count told the
// operator retention was working.
//
// TWO PRECONDITIONS ARE LOAD-BEARING HERE, and both of them caught this test
// lying during development:
//
//  1. Counts go through MSSQLAdminDB, which bypasses the RLS FILTER predicates.
//     SQL Server enforces tenancy with filter predicates, so a row that exists
//     but does not match the session's context counts as ZERO. An INSERT
//     reported RowsAffected=1 and the row was invisible on the same connection.
//     A seed counted through the ordinary pool reads as "no row to orphan".
//
//  2. The store comes from openMSSQLTenantStore, i.e. through the factory, as
//     production builds it. NewMSSQLStore on a plain pool sets no session
//     context, so its DELETE matches nothing: the sweep removes 0 rows and
//     every child "survives" trivially. engine/store_backends_test.go names
//     this -- "a scope that exists in the process and not in the database".
//
// So the test asserts the seeds are present BEFORE the sweep and the parent is
// gone AFTER it. Without both, a green run means nothing was measured.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

func TestMSSQLRetentionDeletesTheRowsTheWorkflowOwns(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set")
	}
	raw := testutil.MSSQLTestDB(t)
	testutil.SetupMSSQLFullSchema(t, raw)
	admin := testutil.MSSQLAdminDB(t, raw)
	store := openMSSQLTenantStore(t, DefaultTenantUUID)
	ctx := context.Background()

	const def = "mssql-retention-children"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: def, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	uniq := time.Now().Format("150405.000000000")
	key := "mssql-retention-" + uniq
	id, _, err := store.StartNewRun(ctx, "", def, 1, json.RawMessage(`{}`), key, DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	tid := DefaultTenantUUID

	// idempotency_keys is seeded by StartNewRun above; the rest are written
	// directly so the sweep has something to orphan in every table.
	seeds := map[string]struct {
		stmt string
		args []any
	}{
		"event_history":            {`INSERT INTO event_history (workflow_id, step, event_type, service, operation, tenant_id) VALUES (@p1,1,'call','s','o',@p2)`, []any{id, tid}},
		"idempotency_keys":         {"", nil},
		"concurrency_keys":         {`INSERT INTO concurrency_keys (key_hash, key_text, workflow_id, expires_at, tenant_id) VALUES (HASHBYTES('SHA2_256',@p1), @p1, @p2, DATEADD(hour,1,SYSUTCDATETIME()), @p3)`, []any{key, id, tid}},
		"workflow_signals":         {`INSERT INTO workflow_signals (workflow_id, signal_name, payload, tenant_id) VALUES (@p1,'sig','{}',@p2)`, []any{id, tid}},
		"workflow_promises":        {`INSERT INTO workflow_promises (workflow_id, promise_name, promise_id, tenant_id) VALUES (@p1,'pn',@p3,@p2)`, []any{id, tid, "pid-" + uniq}},
		"workflow_update_requests": {`INSERT INTO workflow_update_requests (workflow_id, update_name, tenant_id, priority, payload, status) VALUES (@p1,'upd',@p2,0,'{}','pending')`, []any{id, tid}},
	}

	countFor := func(table string) int {
		t.Helper()
		var n int
		if err := admin.QueryRowContext(ctx,
			fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE workflow_id = @p1", table), id).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}

	// Precondition 1: every table must actually hold a row, or its post-sweep
	// zero would mean "never seeded" and read exactly like "correctly deleted".
	for _, table := range mssqlWorkflowChildTables {
		s := seeds[table]
		if s.stmt != "" {
			if _, err := admin.ExecContext(ctx, s.stmt, s.args...); err != nil {
				t.Fatalf("seed %s: %v", table, err)
			}
		}
		if n := countFor(table); n == 0 {
			t.Fatalf("precondition: %s holds no row to orphan, so this test would pass "+
				"without measuring anything", table)
		}
	}

	if _, err := admin.ExecContext(ctx,
		`UPDATE workflow_instances SET status='done', completed_at=SYSUTCDATETIME() WHERE id=@p1`, id); err != nil {
		t.Fatalf("mark completed: %v", err)
	}
	if _, err := store.DeleteCompletedWorkflows(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	// Precondition 2: the parent must be gone. A sweep that matched nothing
	// leaves every child in place for a reason that has nothing to do with
	// cascade behaviour.
	var parent int
	if err := admin.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workflow_instances WHERE id=@p1`, id).Scan(&parent); err != nil {
		t.Fatalf("parent count: %v", err)
	}
	if parent != 0 {
		t.Fatalf("precondition: the sweep left the workflow in place (%d rows), so the "+
			"child assertions below would measure nothing", parent)
	}

	for _, table := range mssqlWorkflowChildTables {
		if n := countFor(table); n != 0 {
			t.Errorf("%s kept %d row(s) for a workflow retention deleted. SQL Server has "+
				"no FK to workflow_instances, so nothing cascades and this table must be "+
				"deleted explicitly (cleat#1265)", table, n)
		}
	}
}
