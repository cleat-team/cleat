package engine

// cleat#2060. SQL Server escalates row and page locks to a table lock once a
// single statement holds roughly 5,000 locks on one object. `event_history`
// is not partitioned, so a successful escalation takes a table lock that
// blocks every tenant's event writes until the statement finishes.
//
// All three retention sweeps deleted up to 10,000 workflows' events in ONE
// statement before this fix -- at five events per workflow (this issue's own
// working number) that crosses the ~5,000-lock threshold at roughly 1,000
// workflows, well inside a single batch. The row bound at
// mssqlEventRowChunk (2,000 rows per DELETE, see engine/mssql_schedules.go)
// fixes this by construction: 2,000 stays under the threshold regardless of
// how many distinct workflows those rows belong to.
//
// This is the acceptance criterion's regression test: it asserts
// sys.dm_db_index_operational_stats records zero NEW lock escalations on
// event_history across a sweep sized past the threshold, for each of the
// three retention arms (DeleteExpiredEvents, DeleteCompletedWorkflows,
// DeleteDeadLetteredWorkflows).
//
// Escalation is measured as a DELTA taken immediately before and after each
// sweep, never as an absolute count: index_lock_promotion_count is a
// cumulative, server-lifetime counter with no per-test reset short of
// restarting the instance, and this database is not exclusive to this test
// -- an earlier test's activity (or another session's, on a shared
// container) can leave it non-zero before this test ever runs.
//
// RCSI (READ_COMMITTED_SNAPSHOT) is a test-environment precondition, not
// something this test turns on itself: the issue asks for it because Azure
// SQL Database runs that way (#2059, #982), and turning it on mid-suite
// would affect every other test sharing this connection. See
// engine/testutil for where it belongs if it needs to become a fixture
// default; here it is asserted rather than set, so a database that does not
// have it on fails loudly rather than silently measuring the wrong
// configuration.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// mssql2060WorkflowCount, at mssql2060EventsPerWorkflow events each, puts an
// UNBOUNDED delete of their events at roughly 5x SQL Server's ~5,000-lock
// escalation threshold -- comfortably past it, not merely at its edge, so a
// regression that widens mssqlEventRowChunk partway back toward "unbounded"
// still gets caught.
const (
	mssql2060WorkflowCount     = 6000
	mssql2060EventsPerWorkflow = 5
)

// eventHistoryLockPromotions reads the cumulative, server-lifetime lock
// escalation count for event_history. Callers difference two readings taken
// around one sweep; see the file comment for why an absolute reading is not
// meaningful here.
func eventHistoryLockPromotions(t *testing.T, ctx context.Context, admin *sql.DB) int64 {
	t.Helper()
	var n sql.NullInt64
	if err := admin.QueryRowContext(ctx, `
		SELECT SUM(index_lock_promotion_count)
		FROM sys.dm_db_index_operational_stats(DB_ID(), OBJECT_ID('event_history'), NULL, NULL)
	`).Scan(&n); err != nil {
		t.Fatalf("read index_lock_promotion_count: %v", err)
	}
	return n.Int64
}

// seedMSSQL2060Workflows seeds len(ids) terminal workflows under def, each
// owning mssql2060EventsPerWorkflow event_history rows, batched to stay well
// under SQL Server's 2100-parameter cap per statement.
func seedMSSQL2060Workflows(t *testing.T, ctx context.Context, admin *sql.DB, def, tenantID, status string, ids []string, completedAt time.Time) {
	t.Helper()
	const batch = 150
	for start := 0; start < len(ids); start += batch {
		end := start + batch
		if end > len(ids) {
			end = len(ids)
		}
		part := ids[start:end]

		var wiSQL strings.Builder
		wiSQL.WriteString("INSERT INTO workflow_instances (id, def_name, def_version, status, completed_at, tenant_id) VALUES ")
		wiArgs := make([]any, 0, len(part)+4)
		var ehSQL strings.Builder
		ehSQL.WriteString("INSERT INTO event_history (workflow_id, step, tenant_id) VALUES ")
		ehArgs := make([]any, 0, len(part)+1)
		for i, id := range part {
			if i > 0 {
				wiSQL.WriteString(", ")
			}
			idParam := fmt.Sprintf("id%d", i)
			wiSQL.WriteString("(@" + idParam + ", @def_name, 1, @status, @completed_at, @tenant_id)")
			wiArgs = append(wiArgs, sql.Named(idParam, id))
			for step := 1; step <= mssql2060EventsPerWorkflow; step++ {
				if i > 0 || step > 1 {
					ehSQL.WriteString(", ")
				}
				ehSQL.WriteString(fmt.Sprintf("(@%s, %d, @tenant_id)", idParam, step))
			}
			ehArgs = append(ehArgs, sql.Named(idParam, id))
		}
		wiArgs = append(wiArgs,
			sql.Named("def_name", def), sql.Named("status", status),
			sql.Named("completed_at", completedAt), sql.Named("tenant_id", tenantID))
		ehArgs = append(ehArgs, sql.Named("tenant_id", tenantID))

		if _, err := admin.ExecContext(ctx, wiSQL.String(), wiArgs...); err != nil {
			t.Fatalf("seed workflow_instances[%d:%d]: %v", start, end, err)
		}
		if _, err := admin.ExecContext(ctx, ehSQL.String(), ehArgs...); err != nil {
			t.Fatalf("seed event_history[%d:%d]: %v", start, end, err)
		}
	}
}

func TestMSSQLRetentionSweepsCauseNoLockEscalation(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set")
	}
	raw := testutil.MSSQLTestDB(t)
	testutil.SetupMSSQLFullSchema(t, raw)
	admin := testutil.MSSQLAdminDB(t, raw)
	ctx := context.Background()

	var rcsi bool
	if err := raw.QueryRowContext(ctx,
		`SELECT is_read_committed_snapshot_on FROM sys.databases WHERE name = DB_NAME()`,
	).Scan(&rcsi); err != nil {
		t.Fatalf("read is_read_committed_snapshot_on: %v", err)
	}
	if !rcsi {
		t.Fatalf("this database has READ_COMMITTED_SNAPSHOT off -- cleat#2060 asks for it on, " +
			"to match how Azure SQL Database runs (#2059, #982); " +
			"ALTER DATABASE <db> SET READ_COMMITTED_SNAPSHOT ON WITH ROLLBACK IMMEDIATE")
	}

	tid := DefaultTenantUUID
	completedAt := time.Now().Add(-2 * time.Hour)

	arms := []struct {
		name   string
		def    string
		status string
		sweep  func(*MSSQLStore) (int64, error)
	}{
		{
			name: "DeleteExpiredEvents", def: "mssql-2060-expired", status: "done",
			sweep: func(s *MSSQLStore) (int64, error) { return s.DeleteExpiredEvents(ctx, time.Now()) },
		},
		{
			name: "DeleteCompletedWorkflows", def: "mssql-2060-completed", status: "done",
			sweep: func(s *MSSQLStore) (int64, error) { return s.DeleteCompletedWorkflows(ctx, time.Now()) },
		},
		{
			name: "DeleteDeadLetteredWorkflows", def: "mssql-2060-deadletter", status: "dead_lettered",
			sweep: func(s *MSSQLStore) (int64, error) { return s.DeleteDeadLetteredWorkflows(ctx, time.Now()) },
		},
	}

	for _, arm := range arms {
		t.Run(arm.name, func(t *testing.T) {
			store := openMSSQLTenantStore(t, DefaultTenantUUID)

			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name: arm.def, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1, MinVersion: 1,
			}); err != nil {
				t.Fatalf("deploy: %v", err)
			}

			// Clear any debris a prior, interrupted run of this arm left
			// behind (same reasoning as cleat#2103's test: a sweep that
			// errors mid-way leaves workflow_instances rows in place).
			if _, err := admin.ExecContext(ctx, `DELETE FROM workflow_instances WHERE def_name = @p1`, arm.def); err != nil {
				t.Fatalf("clear stale rows from a prior run: %v", err)
			}
			t.Cleanup(func() {
				if _, err := admin.ExecContext(context.Background(),
					`DELETE FROM workflow_instances WHERE def_name = @p1`, arm.def); err != nil {
					t.Logf("cleanup: clear %s rows: %v", arm.def, err)
				}
			})

			ids := make([]string, mssql2060WorkflowCount)
			for i := range ids {
				ids[i] = fmt.Sprintf("mssql-2060-%s-%05d", arm.name, i)
			}
			seedMSSQL2060Workflows(t, ctx, admin, arm.def, tid, arm.status, ids, completedAt)

			// Precondition: the seed actually landed mssql2060EventsPerWorkflow
			// rows per workflow, or a post-sweep zero delta would mean
			// "nothing to escalate over" rather than "bounded, so it didn't".
			var seededEvents int
			if err := admin.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM event_history eh JOIN workflow_instances wi ON wi.id = eh.workflow_id
				 WHERE wi.def_name = @p1`, arm.def).Scan(&seededEvents); err != nil {
				t.Fatalf("count seeded events: %v", err)
			}
			want := len(ids) * mssql2060EventsPerWorkflow
			if seededEvents != want {
				t.Fatalf("precondition: seeded %d event_history rows, want %d -- the sweep "+
					"below would measure nothing", seededEvents, want)
			}

			before := eventHistoryLockPromotions(t, ctx, raw)
			deleted, err := arm.sweep(store)
			if err != nil {
				t.Fatalf("%s over %d workflows: %v", arm.name, len(ids), err)
			}
			after := eventHistoryLockPromotions(t, ctx, raw)

			if deleted == 0 {
				t.Fatalf("%s reported 0 rows deleted -- the sweep matched nothing, so the "+
					"escalation reading below would measure nothing", arm.name)
			}
			if delta := after - before; delta != 0 {
				t.Errorf("%s: event_history lock promotions rose by %d sweeping %d workflows' "+
					"events (%d rows) in one call -- SQL Server escalated to a table lock, "+
					"which blocks every tenant's event writes until the sweep finishes "+
					"(cleat#2060)", arm.name, delta, len(ids), deleted)
			}
		})
	}
}
