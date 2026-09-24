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
// RCSI (READ_COMMITTED_SNAPSHOT) matters because the issue asks for it ON,
// to match how Azure SQL Database runs (#2059, #982) -- but on-prem SQL
// Server defaults it OFF, and customers running that default are exactly who
// a table-lock escalation on event_history would hurt. So this measures
// BOTH, each on its own PRIVATE database rather than the shared CI one:
// ALTER DATABASE affects every session connected to that database, and the
// CI database is not this test's alone to reconfigure -- the same reasoning
// CleanupMSSQLTestData's package-isolation comment gives for a database
// being one suite's own, one level more specific (one database per RCSI
// setting, not merely per suite). See mssqlPrivateRCSIDatabase.
//
// Measured directly before this was written (not assumed): RCSI OFF holds at
// delta=0 across 3 repeated trials of the same 6,000-workflow scenario RCSI
// ON does -- FK validation reads take a locking read anyway, on or off.

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
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
	ctx := context.Background()

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

	rcsiCases := []struct {
		name string
		on   bool
	}{
		{"RCSI_ON", true},   // Azure SQL Database's own default (#2059, #982)
		{"RCSI_OFF", false}, // on-prem SQL Server's own default
	}

	for _, rcsiCase := range rcsiCases {
		t.Run(rcsiCase.name, func(t *testing.T) {
			tid := DefaultTenantUUID
			completedAt := time.Now().Add(-2 * time.Hour)

			for _, arm := range arms {
				t.Run(arm.name, func(t *testing.T) {
					// One private database PER ARM, not one shared across
					// all three: measured directly (not assumed) that
					// sharing one let an EARLIER arm's just-deleted
					// event_history rows -- ghost records, not yet
					// reclaimed by SQL Server's background cleanup task --
					// still be there when the NEXT arm ran seconds later,
					// occasionally pushing IT over the escalation
					// threshold (delta=+1, intermittent, 1 of 4 runs). A
					// database this test just created and is about to drop
					// has had nothing else touch it, so this only measures
					// each sweep's own contribution to the threshold --
					// which is also the realistic case: production
					// retention sweeps for the same table run on
					// independent schedules, not back-to-back against an
					// otherwise-idle table the way three t.Run arms in one
					// process do.
					db, dsn := mssqlPrivateRCSIDatabase(t, raw,
						strings.ToLower(rcsiCase.name)+"_"+strings.ToLower(arm.name), rcsiCase.on)

					var rcsi bool
					if err := db.QueryRowContext(ctx,
						`SELECT is_read_committed_snapshot_on FROM sys.databases WHERE name = DB_NAME()`,
					).Scan(&rcsi); err != nil {
						t.Fatalf("read is_read_committed_snapshot_on: %v", err)
					}
					if rcsi != rcsiCase.on {
						t.Fatalf("precondition: this private database reads READ_COMMITTED_SNAPSHOT=%v, "+
							"want %v -- mssqlPrivateRCSIDatabase's own setup is wrong, not the tree", rcsi, rcsiCase.on)
					}

					// openMSSQLTenantStore and testutil.MSSQLAdminDB both
					// read CLEAT_TEST_MSSQL directly rather than taking a
					// *sql.DB, so this is what points them at the private
					// database instead of whichever one the job
					// configured. t.Setenv restores it after this subtest.
					t.Setenv("CLEAT_TEST_MSSQL", dsn)
					admin := testutil.MSSQLAdminDB(t, db)

					store := openMSSQLTenantStore(t, DefaultTenantUUID)

					if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
						Name: arm.def, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
						ABIVersion: 1, MinVersion: 1,
					}); err != nil {
						t.Fatalf("deploy: %v", err)
					}

					// No prior-run debris to clear here: this database was
					// just created fresh by mssqlPrivateRCSIDatabase, and is
					// dropped at the end of this RCSI case -- unlike the
					// shared CI database this test used to run against,
					// nothing else can have left rows in it.
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

					before := eventHistoryLockPromotions(t, ctx, db)
					deleted, err := arm.sweep(store)
					if err != nil {
						t.Fatalf("%s over %d workflows: %v", arm.name, len(ids), err)
					}
					after := eventHistoryLockPromotions(t, ctx, db)

					if deleted == 0 {
						t.Fatalf("%s reported 0 rows deleted -- the sweep matched nothing, so the "+
							"escalation reading below would measure nothing", arm.name)
					}
					if delta := after - before; delta != 0 {
						t.Errorf("%s (%s): event_history lock promotions rose by %d sweeping %d "+
							"workflows' events (%d rows) in one call -- SQL Server escalated to a "+
							"table lock, which blocks every tenant's event writes until the sweep "+
							"finishes (cleat#2060)", arm.name, rcsiCase.name, delta, len(ids), deleted)
					}
				})
			}
		})
	}
}

// mssqlPrivateRCSIDatabase creates a database named cleat_test_2060_<suffix>
// on the same server CLEAT_TEST_MSSQL points at, sets
// READ_COMMITTED_SNAPSHOT to rcsiOn before anything else connects to it,
// applies the shipped schema, and registers a t.Cleanup that drops it.
//
// It is deliberately its own database rather than a reused shared one:
// ALTER DATABASE SET READ_COMMITTED_SNAPSHOT affects every session connected
// to that database, and CI's shared MSSQL database belongs to every other
// test in this binary -- flipping it mid-suite (or leaving it flipped after)
// would change what every other test measures. Same reasoning as
// CleanupMSSQLTestData's package-isolation comment, one level more specific:
// this needs a database of its own, not merely a suite of its own.
//
// Returns the *sql.DB (sa-authenticated, same as testutil.MSSQLTestDB, since
// it is opened from the same base DSN with only the database name swapped)
// and the DSN, so the caller can t.Setenv("CLEAT_TEST_MSSQL", dsn) before
// anything that reads that variable directly.
func mssqlPrivateRCSIDatabase(t *testing.T, raw *sql.DB, suffix string, rcsiOn bool) (*sql.DB, string) {
	t.Helper()
	ctx := context.Background()
	dbName := "cleat_test_2060_" + suffix

	drop := fmt.Sprintf(
		"IF DB_ID('%s') IS NOT NULL BEGIN ALTER DATABASE [%s] SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE [%s]; END",
		dbName, dbName, dbName)
	if _, err := raw.ExecContext(ctx, drop); err != nil {
		t.Fatalf("drop any %s left by a prior interrupted run: %v", dbName, err)
	}
	if _, err := raw.ExecContext(ctx, "CREATE DATABASE ["+dbName+"]"); err != nil {
		t.Fatalf("create %s: %v", dbName, err)
	}
	t.Cleanup(func() {
		if _, err := raw.ExecContext(context.Background(), drop); err != nil {
			t.Logf("cleanup: drop %s: %v", dbName, err)
		}
	})

	if rcsiOn {
		// Set before anything else connects to dbName, so there is nothing
		// else's session to roll back.
		if _, err := raw.ExecContext(ctx,
			fmt.Sprintf("ALTER DATABASE [%s] SET READ_COMMITTED_SNAPSHOT ON WITH ROLLBACK IMMEDIATE", dbName)); err != nil {
			t.Fatalf("set READ_COMMITTED_SNAPSHOT on %s: %v", dbName, err)
		}
	}

	base := os.Getenv("CLEAT_TEST_MSSQL")
	if base == "" {
		base = "sqlserver://sa:CleatTest123!@localhost:1433?database=cleat"
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse CLEAT_TEST_MSSQL: %v", err)
	}
	q := u.Query()
	q.Set("database", dbName)
	u.RawQuery = q.Encode()
	dsn := u.String()

	db, err := sql.Open("sqlserver", dsn)
	if err != nil {
		t.Fatalf("open %s: %v", dbName, err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping %s: %v", dbName, err)
	}

	testutil.SetupMSSQLFullSchema(t, db)
	return db, dsn
}
