package engine

// cleat#1324, asserted as the consequence rather than as a row count.
//
// retention_children_pg_mysql_test.go proves the row is gone. This proves what
// the row being there DID: a caller holding the original Idempotency-Key was
// answered `already_started` with a workflow_id that no longer exists, and no
// request it could make would get the work done under that token until the key
// expired on its own -- seven days by schema default.
//
// The dead-letter path is where that matters most, and the run below goes
// through the real MoveToDeadLetterQueue rather than an UPDATE for one specific
// reason: that method WRITES to idempotency_keys on its way past, setting
// error_msg on the very row nothing later deletes. The key was never forgotten
// on this path -- it was touched and left.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// deadLetterStore is the slice of the store interface this test drives.
type deadLetterStore interface {
	retentionSweeper
	StartNewRun(ctx context.Context, runID, defName string, version int, input json.RawMessage,
		idempotencyKey, tenantID string, priority int) (string, bool, error)
	MoveToDeadLetterQueue(ctx context.Context, workflowID, workerID string, generation int64,
		errMsg, errorCode, errorOp string) error
}

func TestDeadLetterRetentionFreesTheIdempotencyKey(t *testing.T) {
	for _, d := range []struct {
		dialect testutil.Dialect
		ph      func(int) string
		store   func(*sql.DB) deadLetterStore
		cleanup func(*testing.T, *sql.DB)
	}{
		{
			dialect: testutil.DialectPostgres,
			ph:      func(i int) string { return fmt.Sprintf("$%d", i) },
			store:   func(db *sql.DB) deadLetterStore { return NewPostgresStore(db) },
			cleanup: testutil.CleanupPostgresTestData,
		},
		{
			dialect: testutil.DialectMySQL,
			ph:      func(int) string { return "?" },
			store:   func(db *sql.DB) deadLetterStore { return NewMySQLStore(db) },
			cleanup: testutil.CleanupMySQLTestData,
		},
	} {
		t.Run(string(d.dialect), func(t *testing.T) {
			db := testutil.TestDB(t, d.dialect)
			defer db.Close()
			testutil.SetupFullSchema(t, db, d.dialect)
			d.cleanup(t, db)
			defer d.cleanup(t, db)

			ctx := context.Background()
			store := d.store(db)
			const defName = "dlq-retention-idempotency"

			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1, MinVersion: 1,
			}); err != nil {
				t.Fatalf("deploy: %v", err)
			}

			key := fmt.Sprintf("operator-token-1324-%s", d.dialect)
			id, existed, err := store.StartNewRun(ctx, "", defName, 1, json.RawMessage(`{}`), key,
				DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("first start: %v", err)
			}
			if existed {
				t.Fatalf("precondition: first start reported already_started")
			}

			// MoveToDeadLetterQueue is fenced on (assigned_to, generation), so
			// the run has to be owned before it can be dead-lettered. Claiming
			// it by hand keeps this test about retention rather than about the
			// poll loop.
			const workerID = "worker-1324"
			if _, err := db.ExecContext(ctx,
				`UPDATE workflow_instances SET assigned_to = `+d.ph(1)+` WHERE id = `+d.ph(2),
				workerID, id); err != nil {
				t.Fatalf("claim: %v", err)
			}
			var generation int64
			if err := db.QueryRowContext(ctx,
				`SELECT generation FROM workflow_instances WHERE id = `+d.ph(1), id).Scan(&generation); err != nil {
				t.Fatalf("read generation: %v", err)
			}
			if err := store.MoveToDeadLetterQueue(ctx, id, workerID, generation,
				"retries exhausted", "E_EXHAUSTED", "call"); err != nil {
				t.Fatalf("move to dead letter queue: %v", err)
			}

			var status string
			var completedAt sql.NullTime
			if err := db.QueryRowContext(ctx,
				`SELECT status, completed_at FROM workflow_instances WHERE id = `+d.ph(1),
				id).Scan(&status, &completedAt); err != nil {
				t.Fatalf("read status: %v", err)
			}
			if status != "dead_lettered" || !completedAt.Valid {
				t.Fatalf("precondition: want dead_lettered with completed_at set, got %q valid=%v -- "+
					"the dead-letter sweep selects on both, so it would not reach this run",
					status, completedAt.Valid)
			}

			// The key is there AND the dead-letter path wrote to it. If this
			// read came back empty the sweep below would have nothing to leak
			// and the assertions after it would prove nothing.
			var keyErr sql.NullString
			if err := db.QueryRowContext(ctx,
				`SELECT error_msg FROM idempotency_keys WHERE workflow_id = `+d.ph(1),
				id).Scan(&keyErr); err != nil {
				t.Fatalf("precondition: read the idempotency key: %v", err)
			}
			if keyErr.String != "retries exhausted" {
				t.Fatalf("precondition: idempotency_keys.error_msg = %q, want %q",
					keyErr.String, "retries exhausted")
			}

			// A future cutoff, so the sweep reaches a run dead-lettered a
			// moment ago without depending on how much wall-clock elapses
			// between two transactions.
			if _, err := store.DeleteDeadLetteredWorkflows(ctx, time.Now().UTC().Add(time.Hour)); err != nil {
				t.Fatalf("sweep: %v", err)
			}

			var instances int
			if err := db.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM workflow_instances WHERE id = `+d.ph(1), id).Scan(&instances); err != nil {
				t.Fatalf("count instances after: %v", err)
			}
			if instances != 0 {
				t.Fatalf("precondition: the sweep did not delete the workflow (%d rows left), so "+
					"nothing below is about a key outliving its run", instances)
			}

			// The consequence, stated as the caller experiences it: the retry
			// must start a NEW run. Answered `already_started` here, the caller
			// is told work that never happened is already running, and has no
			// way to ask for it again under this token.
			id2, existed2, err := store.StartNewRun(ctx, "", defName, 1, json.RawMessage(`{}`), key,
				DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("retry after sweep: %v", err)
			}
			if existed2 {
				t.Errorf("a retry after the dead-letter sweep was answered already_started with "+
					"workflow_id %q, which the sweep deleted -- every read of that id 404s, and "+
					"the run it names was dead-lettered, so the work never happened (cleat#1324)", id2)
			}
			var live int
			if err := db.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM workflow_instances WHERE id = `+d.ph(1), id2).Scan(&live); err != nil {
				t.Fatalf("count retry instance: %v", err)
			}
			if live != 1 {
				t.Errorf("the retry returned workflow_id %q, which has %d rows; a retry must "+
					"name a run that exists", id2, live)
			}
		})
	}
}
