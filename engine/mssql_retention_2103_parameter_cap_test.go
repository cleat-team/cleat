package engine

// cleat#2103. deleteExpiredEventsOnce's mark-swept UPDATE built one @pN
// parameter per swept workflow id, unchunked. SQL Server caps a statement at
// 2100 parameters, so a retention run that swept more than ~2100 distinct
// workflows in one pass crashed the whole sweep outright (Msg 8003) instead
// of deleting fewer rows -- and crashed the retry too, since the same
// candidate set is still there next attempt. The issue's own reproduction:
// 2000 workflows swept cleanly, 2500 crashed with none of the 2500 marked.
//
// The fix in cleat#2060 bounds DELETE TOP (mssqlEventRowChunk) FROM
// event_history to at most mssqlEventRowChunk event rows removed per
// statement. For a one-event-per-workflow batch like this test's, that
// already keeps the number of distinct workflow ids passed to the
// mark-swept UPDATE under the 2100 cap, so this test's green result is
// largely a side effect of #2060's row bound -- see mssqlEventRowChunk's
// doc comment. chunkedMarkHistorySwept also chunks its own UPDATE at
// mssqlIDChunk regardless, so the cap holds even if mssqlEventRowChunk is
// ever raised independently of it.
//
// TestMSSQLMarkSweptChunksAtTheParameterCapEvenWithoutTheRowBound (below)
// isolates chunkedMarkHistorySwept's own chunking, in case mssqlEventRowChunk
// is ever raised past 2100 and this test's margin disappears with it.

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

// mssql2103WorkflowCount is chosen to be comfortably past the 2100-parameter
// cap and past mssqlEventRowChunk's 2000, so a single DeleteExpiredEvents
// call must loop more than once through deleteExpiredEventsOnce's outer for
// to sweep every workflow -- exercising exactly the multi-batch path #2103
// broke.
const mssql2103WorkflowCount = 2500

func seedMSSQL2103Workflows(t *testing.T, ctx context.Context, admin *sql.DB, def, tenantID string, ids []string, completedAt time.Time) {
	t.Helper()
	const batch = 200
	for start := 0; start < len(ids); start += batch {
		end := start + batch
		if end > len(ids) {
			end = len(ids)
		}
		part := ids[start:end]

		var wiSQL strings.Builder
		wiSQL.WriteString("INSERT INTO workflow_instances (id, def_name, def_version, status, completed_at, tenant_id) VALUES ")
		wiArgs := make([]any, 0, len(part)+3)
		var ehSQL strings.Builder
		ehSQL.WriteString("INSERT INTO event_history (workflow_id, step, tenant_id) VALUES ")
		ehArgs := make([]any, 0, len(part)+1)
		for i, id := range part {
			if i > 0 {
				wiSQL.WriteString(", ")
				ehSQL.WriteString(", ")
			}
			idParam := fmt.Sprintf("id%d", i)
			wiSQL.WriteString("(@" + idParam + ", @def_name, 1, 'done', @completed_at, @tenant_id)")
			ehSQL.WriteString("(@" + idParam + ", 1, @tenant_id)")
			wiArgs = append(wiArgs, sql.Named(idParam, id))
			ehArgs = append(ehArgs, sql.Named(idParam, id))
		}
		wiArgs = append(wiArgs,
			sql.Named("def_name", def), sql.Named("completed_at", completedAt), sql.Named("tenant_id", tenantID))
		ehArgs = append(ehArgs, sql.Named("tenant_id", tenantID))

		if _, err := admin.ExecContext(ctx, wiSQL.String(), wiArgs...); err != nil {
			t.Fatalf("seed workflow_instances[%d:%d]: %v", start, end, err)
		}
		if _, err := admin.ExecContext(ctx, ehSQL.String(), ehArgs...); err != nil {
			t.Fatalf("seed event_history[%d:%d]: %v", start, end, err)
		}
	}
}

func TestMSSQLDeleteExpiredEventsSurvivesMoreThan2100Workflows(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set")
	}
	raw := testutil.MSSQLTestDB(t)
	testutil.SetupMSSQLFullSchema(t, raw)
	admin := testutil.MSSQLAdminDB(t, raw)
	store := openMSSQLTenantStore(t, DefaultTenantUUID)
	ctx := context.Background()

	const def = "mssql-2103-param-cap"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: def, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	tid := DefaultTenantUUID
	completedAt := time.Now().Add(-2 * time.Hour)

	// A prior run of this test that errored out mid-sweep (as the falsified
	// mutant does, deliberately) leaves its workflow_instances rows behind:
	// DeleteExpiredEvents never deletes the parent, only marks it swept, and
	// this run never reached that UPDATE. Clear anything this def name owns
	// before seeding, or a later count below double-counts stale debris from
	// an earlier failure as if this run had seeded it -- admin bypasses RLS,
	// so a plain DELETE here is not itself scoped without setting session
	// context, which MSSQLAdminDB already does.
	if _, err := admin.ExecContext(ctx, `DELETE FROM workflow_instances WHERE def_name = @p1`, def); err != nil {
		t.Fatalf("clear stale rows from a prior run: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.Background(),
			`DELETE FROM workflow_instances WHERE def_name = @p1`, def); err != nil {
			t.Logf("cleanup: clear %s rows: %v", def, err)
		}
	})

	ids := make([]string, mssql2103WorkflowCount)
	for i := range ids {
		ids[i] = fmt.Sprintf("mssql-2103-%05d", i)
	}
	seedMSSQL2103Workflows(t, ctx, admin, def, tid, ids, completedAt)

	// Precondition: every seeded workflow actually holds an unswept event, or
	// a post-sweep zero below would mean "never seeded" rather than "swept".
	var seededEvents int
	if err := admin.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM event_history eh JOIN workflow_instances wi ON wi.id = eh.workflow_id
		 WHERE wi.def_name = @p1`, def).Scan(&seededEvents); err != nil {
		t.Fatalf("count seeded events: %v", err)
	}
	if seededEvents != len(ids) {
		t.Fatalf("precondition: seeded %d event_history rows, want %d -- the sweep below "+
			"would measure nothing", seededEvents, len(ids))
	}

	deleted, err := store.DeleteExpiredEvents(ctx, time.Now())
	if err != nil {
		t.Fatalf("DeleteExpiredEvents over %d workflows: %v -- cleat#2103 is the "+
			"2100-parameter cap in the mark-swept UPDATE", len(ids), err)
	}
	if deleted != int64(len(ids)) {
		t.Errorf("DeleteExpiredEvents reported %d rows deleted, want %d", deleted, len(ids))
	}

	var remainingEvents int
	if err := admin.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM event_history eh JOIN workflow_instances wi ON wi.id = eh.workflow_id
		 WHERE wi.def_name = @p1`, def,
	).Scan(&remainingEvents); err != nil {
		t.Fatalf("count remaining events: %v", err)
	}
	if remainingEvents != 0 {
		t.Errorf("%d event_history row(s) survived the sweep", remainingEvents)
	}

	var unswept int
	if err := admin.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workflow_instances WHERE tenant_id = @p1 AND def_name = @p2 AND history_swept_at IS NULL`,
		tid, def).Scan(&unswept); err != nil {
		t.Fatalf("count unswept workflows: %v", err)
	}
	if unswept != 0 {
		t.Errorf("%d of %d workflows were never marked history_swept_at -- the sweep did "+
			"not actually reach every workflow it deleted events for", unswept, len(ids))
	}
}

// TestMSSQLMarkSweptChunksAtTheParameterCapEvenWithoutTheRowBound calls
// chunkedMarkHistorySwept directly with more ids than the 2100-parameter cap
// allows in one statement, isolated from mssqlEventRowChunk's own margin
// above. If mssqlEventRowChunk is ever raised past 2100, this is the test
// that still catches #2103's original crash; the one above would stop
// catching it.
//
// The ids do not need to name real workflows: the UPDATE's WHERE clause just
// has to carry more than 2100 of them in one statement to exercise the cap,
// and workflow_instances is written to (0 rows matched) rather than read, so
// no seed data is required.
func TestMSSQLMarkSweptChunksAtTheParameterCapEvenWithoutTheRowBound(t *testing.T) {
	if os.Getenv("CLEAT_TEST_MSSQL") == "" {
		t.Skip("CLEAT_TEST_MSSQL not set")
	}
	raw := testutil.MSSQLTestDB(t)
	testutil.SetupMSSQLFullSchema(t, raw)
	store := openMSSQLTenantStore(t, DefaultTenantUUID)
	ctx := context.Background()

	const n = 2500 // > SQL Server's 2100-parameter cap, and > mssqlIDChunk's 2000
	if n <= mssqlIDChunk {
		t.Fatalf("test setup: n=%d must exceed mssqlIDChunk=%d to exercise chunking", n, mssqlIDChunk)
	}
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("mssql-2103-unit-%05d", i)
	}

	tx, err := store.beginTxWithContext(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	if err := store.chunkedMarkHistorySwept(ctx, tx, ids); err != nil {
		t.Fatalf("chunkedMarkHistorySwept with %d ids: %v -- this is the unchunked "+
			"parameter-cap crash cleat#2103 reported", n, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
