package engine

// cleat#1255. Retention deleted the workflow_instances row and left the
// idempotency key behind, so the key still resolved: a retry was answered
// `already_started` with a workflow_id that 404s on every read path. That is
// worse than having no idempotency at all -- a client that received an error
// would retry, while this one is told its work is already running and stops.
//
// Six tables carry a workflow_id. Five were already handled and one was not:
//
//	workflow_signals, workflow_promises, concurrency_keys,
//	workflow_update_requests        FK to workflow_instances, ON DELETE CASCADE
//	event_history                   no FK -- deleted explicitly, see
//	                                completed_workflows_rls_test.go for why the
//	                                FK was dropped on this dialect
//	idempotency_keys                no FK, and nothing deleted it
//
// The window that reaches it is not exotic. idempotencyCleanupLoop keeps keys
// for a hardcoded seven days while the retention window is operator-set, so any
// window under a week does this to every swept workflow -- and the admin sweep
// endpoint's older_than override reaches it immediately at any configuration.
//
// Counts here come from adminDB, the superuser connection, never from the
// RLS-scoped one: a policy hiding a row renders identically to the row being
// deleted, which is how TestCascadeDelete was burned (see the file above).

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

func TestRetentionDeletesTheIdempotencyKeyWithTheWorkflow(t *testing.T) {
	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	defer adminDB.Close()
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, adminDB)
	defer testutil.CleanupPostgresTestData(t, adminDB)

	ctx := context.Background()
	const defName = "retention-idempotency"
	store := NewPostgresStore(adminDB)

	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	const key = "operator-token-1255"
	id, existed, err := store.StartNewRun(ctx, "", defName, 1, json.RawMessage(`{}`), key,
		DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	if existed {
		t.Fatalf("precondition: first start reported already_started")
	}

	// The key must actually be there, or the post-sweep count reads "deleted"
	// when nothing was ever written and this test proves nothing.
	var keysBefore int
	if err := adminDB.QueryRowContext(ctx,
		`SELECT count(*) FROM idempotency_keys WHERE workflow_id = $1`, id).Scan(&keysBefore); err != nil {
		t.Fatalf("count keys before: %v", err)
	}
	if keysBefore != 1 {
		t.Fatalf("precondition: expected 1 idempotency_keys row for %s, found %d", id, keysBefore)
	}

	if _, err := adminDB.ExecContext(ctx,
		`UPDATE workflow_instances SET status = 'done', completed_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("mark completed: %v", err)
	}

	// A future cutoff, so the sweep reaches a run completed a moment ago
	// without depending on how much wall-clock elapses between two
	// transactions -- now() is transaction-start, not statement time.
	if _, err := store.DeleteCompletedWorkflows(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	var instances, keysAfter int
	if err := adminDB.QueryRowContext(ctx,
		`SELECT count(*) FROM workflow_instances WHERE id = $1`, id).Scan(&instances); err != nil {
		t.Fatalf("count instances after: %v", err)
	}
	if instances != 0 {
		t.Fatalf("precondition: the sweep did not delete the workflow (%d rows left)", instances)
	}
	if err := adminDB.QueryRowContext(ctx,
		`SELECT count(*) FROM idempotency_keys WHERE workflow_id = $1`, id).Scan(&keysAfter); err != nil {
		t.Fatalf("count keys after: %v", err)
	}
	if keysAfter != 0 {
		t.Errorf("the swept workflow left %d idempotency_keys row(s) behind; the key "+
			"outlives the run it names (cleat#1255)", keysAfter)
	}

	// The consequence, asserted directly rather than inferred from the row
	// count: a retry must start a NEW run, not hand back a deleted id.
	id2, existed2, err := store.StartNewRun(ctx, "", defName, 1, json.RawMessage(`{}`), key,
		DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("retry after sweep: %v", err)
	}
	if existed2 {
		t.Errorf("a retry after the sweep was answered already_started with workflow_id %q, "+
			"which the sweep deleted -- the caller is told its work is already running "+
			"and every read of that id 404s", id2)
	}
	var live int
	if err := adminDB.QueryRowContext(ctx,
		`SELECT count(*) FROM workflow_instances WHERE id = $1`, id2).Scan(&live); err != nil {
		t.Fatalf("count retry instance: %v", err)
	}
	if live != 1 {
		t.Errorf("the retry returned workflow_id %q, which has %d rows; a retry must "+
			"name a run that exists", id2, live)
	}
}
