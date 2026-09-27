package engine

// cleat#1324. The dead-letter sweep left the idempotency key behind on
// PostgreSQL and MySQL. It is cleat#1255 surviving in the sibling method:
// #1255 fixed DeleteCompletedWorkflows on both dialects and did not touch
// DeleteDeadLetteredWorkflows, which has the same shape and deletes
// event_history for the same stated reason.
//
// The dead-letter case is the worse of the two. A key that outlives its run
// answers every retry `already_started` with a workflow_id that 404s on every
// read path -- and a dead-lettered run is precisely the one a caller has reason
// to retry, because the work did not happen. The key is a permanent lock: there
// is no request that gets the work done under that token for as long as the row
// lives (seven days by schema default, thirty on the TTL this store sets).
//
// WHY THIS TEST IS SHAPED LIKE THIS, AND NOT LIKE THE #1255 ONE. The #1255
// regression test asserts one table on one arm on one dialect, so it was green
// throughout, and so were the two facts it was silent about: the other arm, and
// MySQL. SQL Server has been immune since #1265 not because someone remembered
// it, but because both arms there call one deleteWorkflowsBatch over one
// mssqlWorkflowChildTables list -- the child set cannot diverge between them.
// PostgreSQL and MySQL still have two hand-written delete sequences each, so
// what closes this class here is a test over the PRODUCT of arm and child
// table, in the shape of mssql_retention_children_test.go.
//
// That product is what makes the guard worth more than the fix: it fails for a
// seventh table added to one arm and not the other, on either dialect, without
// anyone thinking of this issue again.
//
// Counting is done through the same connection that owns the data, which for
// these two dialects is the test's superuser handle. On PostgreSQL an RLS
// policy hiding a row renders identically to the row being deleted -- the trap
// mssql_retention_children_test.go documents at length for FILTER predicates --
// so the parent-is-gone precondition below is not decoration: without it a
// sweep that deleted nothing produces six passing child assertions.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// retentionChildTables is every CORE table with a workflow_id column. It is the
// same six as mssqlRetentionChildTables; event_awaiters and workflow_blob_refs
// carry one too but belong to plugins/eventtriggers and plugins/blobstore,
// which own their own migrations and their own cleanup.
var retentionChildTables = []string{
	"event_history",
	"idempotency_keys",
	"concurrency_keys",
	"workflow_signals",
	"workflow_promises",
	"workflow_update_requests",
}

// retentionSweeper is the two methods under test. Both dialects' stores satisfy
// it, which is what lets one table-driven body cover both.
type retentionSweeper interface {
	DeployWorkflowDef(ctx context.Context, def *WorkflowDef) error
	DeleteCompletedWorkflows(ctx context.Context, olderThan time.Time) (int64, error)
	DeleteDeadLetteredWorkflows(ctx context.Context, olderThan time.Time) (int64, error)
}

func TestRetentionDeletesEveryChildRowOnPostgresAndMySQL(t *testing.T) {
	for _, d := range []struct {
		dialect testutil.Dialect
		ph      func(int) string
		store   func(*sql.DB) retentionSweeper
		cleanup func(*testing.T, *sql.DB)
	}{
		{
			dialect: testutil.DialectPostgres,
			ph:      func(i int) string { return fmt.Sprintf("$%d", i) },
			store:   func(db *sql.DB) retentionSweeper { return NewPostgresStore(db) },
			cleanup: testutil.CleanupPostgresTestData,
		},
		{
			dialect: testutil.DialectMySQL,
			ph:      func(int) string { return "?" },
			store:   func(db *sql.DB) retentionSweeper { return NewMySQLStore(db) },
			cleanup: testutil.CleanupMySQLTestData,
		},
	} {
		t.Run(string(d.dialect), func(t *testing.T) {
			db := testutil.TestDB(t, d.dialect)
			defer db.Close()
			testutil.SetupFullSchema(t, db, d.dialect)
			d.cleanup(t, db)
			defer d.cleanup(t, db)

			for _, tc := range []struct {
				name   string
				status string
				sweep  func(retentionSweeper, context.Context, time.Time) (int64, error)
			}{
				{
					name:   "completed",
					status: "done",
					sweep: func(s retentionSweeper, ctx context.Context, cutoff time.Time) (int64, error) {
						return s.DeleteCompletedWorkflows(ctx, cutoff)
					},
				},
				{
					// The arm cleat#1255 did not reach. Both or neither: this
					// one deletes event_history explicitly on PostgreSQL for
					// the identical "no FK, nothing else removes it" reason,
					// so a table that needs the same treatment needs it here.
					name:   "dead-lettered",
					status: "dead_lettered",
					sweep: func(s retentionSweeper, ctx context.Context, cutoff time.Time) (int64, error) {
						return s.DeleteDeadLetteredWorkflows(ctx, cutoff)
					},
				},
			} {
				t.Run(tc.name, func(t *testing.T) {
					ctx := context.Background()
					store := d.store(db)
					wfID := fmt.Sprintf("retention-%s-%s-%d", d.dialect, tc.name, time.Now().UnixNano())
					completedAt := time.Now().UTC().Add(-48 * time.Hour)

					seedRetentionFixture(t, ctx, db, store, d.ph, wfID, tc.status, completedAt)

					// PRECONDITION ONE: every child row is really there. A seed
					// that silently did not land makes all six assertions below
					// pass while measuring nothing.
					for _, tbl := range retentionChildTables {
						if n := countChildRows(t, ctx, db, d.ph, tbl, wfID); n != 1 {
							t.Fatalf("PRECONDITION FAILED: seeded %s has %d rows, want 1 -- "+
								"nothing below measures anything", tbl, n)
						}
					}

					if _, err := tc.sweep(store, ctx, time.Now().UTC().Add(-1*time.Hour)); err != nil {
						t.Fatalf("%s sweep: %v", tc.name, err)
					}

					// PRECONDITION TWO: the parent is gone. A sweep that
					// deleted nothing leaves every child in place for a reason
					// that has nothing to do with this defect.
					var parents int
					if err := db.QueryRowContext(ctx,
						`SELECT COUNT(*) FROM workflow_instances WHERE id = `+d.ph(1), wfID).Scan(&parents); err != nil {
						t.Fatalf("count the parent row: %v", err)
					}
					if parents != 0 {
						t.Fatalf("PRECONDITION FAILED: the workflow row survived the %s sweep, "+
							"so the child counts below say nothing about child deletion", tc.name)
					}

					for _, tbl := range retentionChildTables {
						if n := countChildRows(t, ctx, db, d.ph, tbl, wfID); n != 0 {
							t.Errorf("%s/%s: %d row(s) survive in %s after the workflow was "+
								"deleted.\n\nThe instance row was the only thing that made "+
								"them reachable, and no later sweep looks at anything but "+
								"workflow_instances.status, so they are permanent. For "+
								"idempotency_keys specifically the surviving row still "+
								"RESOLVES: a retry is answered already_started with a "+
								"workflow_id that 404s (cleat#1324).",
								d.dialect, tc.name, n, tbl)
						}
					}
				})
			}
		})
	}
}

// seedRetentionFixture writes one workflow and one row in each child table.
// The two dialects' schemas for these six tables are column-for-column
// identical, so only the placeholder syntax varies.
func seedRetentionFixture(t *testing.T, ctx context.Context, db *sql.DB, store retentionSweeper,
	ph func(int) string, wfID, status string, completedAt time.Time) {
	t.Helper()

	// Through the store rather than a hand-written INSERT: workflow_defs.
	// entry_points is a text[] on PostgreSQL and JSON on MySQL, and one literal
	// cannot satisfy both. Nothing here is testing that column.
	const defName = "retention-children-def"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("deploy workflow def: %v", err)
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO workflow_instances (id, def_name, def_version, status, input, tenant_id, completed_at)
		 VALUES (`+ph(1)+`, `+ph(2)+`, 1, `+ph(3)+`, '{}', `+ph(4)+`, `+ph(5)+`)`,
		wfID, defName, status, DefaultTenantUUID, completedAt); err != nil {
		t.Fatalf("seed workflow_instances: %v", err)
	}

	expires := time.Now().UTC().Add(7 * 24 * time.Hour)
	for _, s := range []struct {
		table string
		stmt  string
		args  []any
	}{
		{"event_history",
			`INSERT INTO event_history (workflow_id, step, event_type, tenant_id)
			 VALUES (` + ph(1) + `, 0, 'call', ` + ph(2) + `)`,
			[]any{wfID, DefaultTenantUUID}},
		{"idempotency_keys",
			`INSERT INTO idempotency_keys (key_hash, workflow_id, expires_at, tenant_id)
			 VALUES (` + ph(1) + `, ` + ph(2) + `, ` + ph(3) + `, ` + ph(4) + `)`,
			[]any{retentionKeyHash(wfID + "-key"), wfID, expires, DefaultTenantUUID}},
		{"concurrency_keys",
			`INSERT INTO concurrency_keys (key_hash, key_text, workflow_id, expires_at, tenant_id)
			 VALUES (` + ph(1) + `, ` + ph(2) + `, ` + ph(3) + `, ` + ph(4) + `, ` + ph(5) + `)`,
			[]any{retentionKeyHash(wfID + "-ck"), wfID + "-ck", wfID, expires, DefaultTenantUUID}},
		{"workflow_signals",
			`INSERT INTO workflow_signals (workflow_id, signal_name, payload, tenant_id)
			 VALUES (` + ph(1) + `, 'sig', '{}', ` + ph(2) + `)`,
			[]any{wfID, DefaultTenantUUID}},
		{"workflow_promises",
			`INSERT INTO workflow_promises (workflow_id, promise_id, promise_name, tenant_id)
			 VALUES (` + ph(1) + `, ` + ph(2) + `, 'p', ` + ph(3) + `)`,
			[]any{wfID, wfID + "-promise", DefaultTenantUUID}},
		// request_id is NOT NULL as of cleat#1416 and has no default -- MySQL
		// refuses DEFAULT (UUID()) under statement-based binlogging, so no
		// dialect has one. Seeds name it explicitly.
		{"workflow_update_requests",
			`INSERT INTO workflow_update_requests (workflow_id, request_id, update_name, tenant_id)
			 VALUES (` + ph(1) + `, ` + ph(2) + `, 'upd', ` + ph(3) + `)`,
			[]any{wfID, "ureq-" + wfID, DefaultTenantUUID}},
	} {
		if _, err := db.ExecContext(ctx, s.stmt, s.args...); err != nil {
			t.Fatalf("seed %s: %v", s.table, err)
		}
	}
}

func countChildRows(t *testing.T, ctx context.Context, db *sql.DB,
	ph func(int) string, table, wfID string) int {
	t.Helper()
	var n int
	//nolint:gosec // table comes from retentionChildTables, a literal list
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM "+table+" WHERE workflow_id = "+ph(1), wfID).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// retentionKeyHash is a real 32-byte digest. Both key_hash columns are
// bytea on PostgreSQL and VARBINARY(32) on MySQL, and MySQL rejects a longer
// value outright rather than truncating it.
func retentionKeyHash(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
