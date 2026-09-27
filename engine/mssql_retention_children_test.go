package engine

// cleat#1265. On SQL Server, retention deleted the workflow_instances row and
// left every child table behind -- all six, including the entire event history.
//
// SQL Server declares no foreign keys to workflow_instances (0, against 5 on
// each of the other dialects), so nothing cascades, and both sweeps relied on a
// cascade that does not exist. The orphaned rows are not merely retained: the
// instance row was the only thing that made them reachable, and no later sweep
// looks at anything but workflow_instances.status, so they are permanent.
//
// TWO PRECONDITIONS, and the audit that found this was wrong twice without
// them -- both times reporting the reassuring answer:
//
//  1. COUNT THROUGH AN ADMIN CONNECTION. SQL Server enforces tenant isolation
//     with RLS FILTER predicates, so a row that exists but does not match the
//     session's tenant counts as ZERO on an ordinary connection. A surviving
//     child would read as cleaned up.
//
//  2. ASSERT THE PARENT IS ACTUALLY GONE before believing any child count. A
//     sweep run on a store with no session context deletes nothing, and every
//     child then "survives" for a reason that has nothing to do with the bug.
//     That is why the store here comes from the FACTORY, as production does --
//     its connector applies sp_set_session_context to every connection.
//
// Both failure modes produce "everything looks fine", which is why the
// preconditions are asserted rather than assumed.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// mssqlRetentionChildTables is every CORE table with a workflow_id column.
// event_awaiters and workflow_blob_refs also have one where those plugins are
// installed; they belong to plugins/eventtriggers and plugins/blobstore, which
// clean them themselves, and the engine's retention does not reach into a
// plugin's table.
var mssqlRetentionChildTables = []string{
	"event_history",
	"idempotency_keys",
	"concurrency_keys",
	"workflow_signals",
	"workflow_promises",
	"workflow_update_requests",
}

func TestMSSQLRetentionDeletesEveryChildRow(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL is not set")
	}
	for _, tc := range []struct {
		name   string
		status string
		sweep  func(*MSSQLStore, context.Context, time.Time) (int64, error)
	}{
		{
			name:   "completed",
			status: "done",
			sweep: func(s *MSSQLStore, ctx context.Context, cutoff time.Time) (int64, error) {
				return s.DeleteCompletedWorkflows(ctx, cutoff)
			},
		},
		{
			// The dead-letter sweep carried the same wrong premise -- it was
			// the one the completed sweep's comment cited as its "verified
			// reference". Both or neither.
			name:   "dead-lettered",
			status: "dead_lettered",
			sweep: func(s *MSSQLStore, ctx context.Context, cutoff time.Time) (int64, error) {
				return s.DeleteDeadLetteredWorkflows(ctx, cutoff)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			tenant := DefaultTenantUUID
			store := openMSSQLTenantStore(t, tenant)
			admin := testutil.MSSQLAdminDB(t, store.db)

			wfID := fmt.Sprintf("mssql-retention-%s-%d", tc.name, time.Now().UnixNano())
			completedAt := time.Now().UTC().Add(-48 * time.Hour)

			t.Cleanup(func() {
				bg := context.Background()
				for _, tbl := range mssqlRetentionChildTables {
					_, _ = admin.ExecContext(bg, //nolint:gosec // tbl is from the literal list above
						"DELETE FROM "+tbl+" WHERE workflow_id = @p1", wfID)
				}
				_, _ = admin.ExecContext(bg, `DELETE FROM workflow_instances WHERE id = @p1`, wfID)
			})

			seedMSSQLRetentionFixture(t, ctx, admin, wfID, tenant, tc.status, completedAt)

			// PRECONDITION ONE: every child row is really there, counted
			// through the admin connection. A seed the FILTER predicate hides
			// would make every assertion below pass trivially.
			for _, tbl := range mssqlRetentionChildTables {
				if n := countMSSQLChildRows(t, ctx, admin, tbl, wfID); n != 1 {
					t.Fatalf("PRECONDITION FAILED: seeded %s has %d rows, want 1 -- nothing "+
						"below measures anything", tbl, n)
				}
			}

			if _, err := tc.sweep(store, ctx, time.Now().UTC().Add(-1*time.Hour)); err != nil {
				t.Fatalf("%s sweep: %v", tc.name, err)
			}

			// PRECONDITION TWO: the parent is gone. A sweep that deleted
			// nothing leaves every child in place for a reason unrelated to
			// this defect.
			var parents int
			if err := admin.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM workflow_instances WHERE id = @p1`, wfID).Scan(&parents); err != nil {
				t.Fatalf("count the parent row: %v", err)
			}
			if parents != 0 {
				t.Fatalf("PRECONDITION FAILED: the workflow row survived the %s sweep, so the "+
					"child counts below say nothing about child deletion", tc.name)
			}

			for _, tbl := range mssqlRetentionChildTables {
				if n := countMSSQLChildRows(t, ctx, admin, tbl, wfID); n != 0 {
					t.Errorf("%s: %d rows survive in %s after the workflow was deleted.\n\n"+
						"Nothing cascades on SQL Server, so a child left here is unreachable "+
						"and permanent: the instance row was the only thing that made it "+
						"findable, and no later sweep looks at anything but "+
						"workflow_instances.status (cleat#1265).", tc.name, n, tbl)
				}
			}
		})
	}
}

// seedMSSQLRetentionFixture writes one workflow and one row in each child
// table, through the admin connection so the RLS policies cannot silently
// discard a write.
func seedMSSQLRetentionFixture(t *testing.T, ctx context.Context, admin *sql.DB,
	wfID, tenant, status string, completedAt time.Time) {
	t.Helper()

	if _, err := admin.ExecContext(ctx, `
		INSERT INTO workflow_defs (name, version, wasm_bytes, entry_points, abi_version, min_version, tenant_id)
		SELECT @p1, 1, 0x0061736d, '[]', 1, 1, @p2
		WHERE NOT EXISTS (SELECT 1 FROM workflow_defs WHERE name = @p1 AND version = 1 AND tenant_id = @p2)
	`, "mssql-retention-def", tenant); err != nil {
		t.Fatalf("seed workflow_defs: %v", err)
	}
	if _, err := admin.ExecContext(ctx, `
		INSERT INTO workflow_instances (id, def_name, def_version, status, input, tenant_id, completed_at)
		VALUES (@p1, @p2, 1, @p3, '{}', @p4, @p5)
	`, wfID, "mssql-retention-def", status, tenant, completedAt); err != nil {
		t.Fatalf("seed workflow_instances: %v", err)
	}

	seeds := []struct {
		table string
		stmt  string
		args  []any
	}{
		{"event_history", `INSERT INTO event_history (workflow_id, step, event_type, tenant_id) VALUES (@p1, 0, 'call', @p2)`, []any{wfID, tenant}},
		{"idempotency_keys", `INSERT INTO idempotency_keys (key_hash, workflow_id, expires_at, tenant_id) VALUES (@p1, @p2, DATEADD(DAY, 7, SYSUTCDATETIME()), @p3)`, []any{mssqlKeyHash(wfID + "-key"), wfID, tenant}},
		{"concurrency_keys", `INSERT INTO concurrency_keys (key_hash, key_text, workflow_id, expires_at, tenant_id) VALUES (@p1, @p2, @p3, DATEADD(HOUR, 1, SYSUTCDATETIME()), @p4)`, []any{mssqlKeyHash(wfID + "-ck"), wfID + "-ck", wfID, tenant}},
		{"workflow_signals", `INSERT INTO workflow_signals (workflow_id, signal_name, payload, tenant_id) VALUES (@p1, 'sig', '{}', @p2)`, []any{wfID, tenant}},
		{"workflow_promises", `INSERT INTO workflow_promises (workflow_id, promise_id, promise_name, tenant_id) VALUES (@p1, @p2, 'p', @p3)`, []any{wfID, wfID + "-promise", tenant}},
		{"workflow_update_requests", `INSERT INTO workflow_update_requests (workflow_id, request_id, update_name, tenant_id) VALUES (@p1, 'ureq-'+@p1, 'upd', @p2)`, []any{wfID, tenant}},
	}
	for _, s := range seeds {
		if _, err := admin.ExecContext(ctx, s.stmt, s.args...); err != nil {
			t.Fatalf("seed %s: %v", s.table, err)
		}
	}
}

// mssqlKeyHash is a real 32-byte digest: both key_hash columns are
// VARBINARY(32), and a longer value is rejected outright rather than truncated
// -- "String or binary data would be truncated", which is how the first version
// of this fixture failed.
func mssqlKeyHash(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

func countMSSQLChildRows(t *testing.T, ctx context.Context, admin *sql.DB, table, wfID string) int {
	t.Helper()
	var n int
	//nolint:gosec // table comes from mssqlRetentionChildTables, a literal list
	if err := admin.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM "+table+" WHERE workflow_id = @p1", wfID).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}
