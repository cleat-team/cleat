package engine

// IMPROVEMENT-PLAN.md 3.20. AdminForceComplete and AdminForceFail returned
// "admin force-complete: not implemented yet" on all three dialects while the
// worker's admin API routed to them, so every force-resolve request got a 500
// from an endpoint whose route, confirmation header and ownership check were
// all real.
//
// These run against every configured backend, because a force-resolve is
// almost entirely SQL and the three dialects disagree about placeholders, the
// clock, and whether there is any tenant enforcement under the Go filter at
// all.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

const adminTestOperator = "operator-9d1f"

// storeScopedToTenant returns the same store, on the same connection, scoped
// to a different tenant.
//
// The connection matters. PostgresBackend.SetupForTenant deliberately opens a
// low-privilege role so that RLS is genuinely enforced -- which is right for
// tenant_isolation_test.go and wrong here: on that fixture PostgreSQL blocks a
// cross-tenant force-resolve whether or not the Go code filters tenant_id, so
// the assertion would pass against a store with no filter at all. This keeps
// the backend's own (owner/superuser) connection, where RLS is bypassed, so
// the only thing that can refuse the write is the tenant_id predicate in
// store_admin.go. MySQL and SQL Server have no RLS underneath at any
// privilege level.
func storeScopedToTenant(t *testing.T, store WorkflowStore, tenantID string) WorkflowStore {
	t.Helper()
	switch s := store.(type) {
	case *PostgresStore:
		return s.WithTenant(tenantID)
	case *MySQLStore:
		return s.WithTenant(tenantID)
	case *MSSQLStore:
		return s.WithTenant(tenantID)
	default:
		t.Fatalf("no WithTenant for %T; the cross-tenant case cannot be tested for this backend", store)
		return nil
	}
}

// startClaimedWorkflow creates a workflow and claims it, so the fixture is the
// state force-resolve exists for: a running workflow owned by a worker.
// It returns the workflow ID and the worker that owns it.
func startClaimedWorkflow(t *testing.T, ctx context.Context, store WorkflowStore, name string) (string, string) {
	t.Helper()

	const defName = "admin-force-resolve"
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	wfID := fmt.Sprintf("%s-%d", name, time.Now().UnixNano())
	if _, _, err := store.StartNewRun(ctx, wfID, defName, 1,
		json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}

	workerID := "worker-" + name
	claimed, err := store.ClaimWorkflows(ctx, workerID, 20)
	if err != nil {
		t.Fatalf("ClaimWorkflows: %v", err)
	}
	found := false
	for _, wf := range claimed {
		if wf.ID == wfID {
			found = true
		}
	}
	if !found {
		t.Fatalf("workflow %s was not claimed; got %d workflow(s)", wfID, len(claimed))
	}
	return wfID, workerID
}

func mustGetWorkflow(t *testing.T, ctx context.Context, store WorkflowStore, wfID string) *WorkflowInstance {
	t.Helper()
	wf, err := store.GetWorkflowByID(ctx, wfID)
	if err != nil {
		t.Fatalf("GetWorkflowByID(%s): %v", wfID, err)
	}
	if wf == nil {
		t.Fatalf("workflow %s not found", wfID)
	}
	return wf
}

// lastAdminEvent returns the single admin_action event in a workflow's
// history, failing if there is not exactly one.
func lastAdminEvent(t *testing.T, ctx context.Context, store WorkflowStore, wfID string) EventRecord {
	t.Helper()
	history, err := store.LoadEventHistory(ctx, wfID)
	if err != nil {
		t.Fatalf("LoadEventHistory(%s): %v", wfID, err)
	}
	var found []EventRecord
	for _, rec := range history {
		if rec.EventType == EventTypeAdminAction {
			found = append(found, rec)
		}
	}
	if len(found) != 1 {
		t.Fatalf("history for %s holds %d admin_action events, want exactly 1 (%d events total)",
			wfID, len(found), len(history))
	}
	return found[0]
}

func countAdminEvents(t *testing.T, ctx context.Context, store WorkflowStore, wfID string) int {
	t.Helper()
	history, err := store.LoadEventHistory(ctx, wfID)
	if err != nil {
		t.Fatalf("LoadEventHistory(%s): %v", wfID, err)
	}
	n := 0
	for _, rec := range history {
		if rec.EventType == EventTypeAdminAction {
			n++
		}
	}
	return n
}

// TestAdminForceComplete_ResolvesAndAudits is the headline case: a claimed,
// running workflow is force-completed by an operator, and every part of the
// contract in admin_ops.go's doc comment is checked rather than just the
// absence of an error.
func TestAdminForceComplete_ResolvesAndAudits(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID, workerID := startClaimedWorkflow(t, ctx, store, "afc")

		before := mustGetWorkflow(t, ctx, store, wfID)
		if before.Status != "running" {
			t.Fatalf("fixture: status is %q, want running", before.Status)
		}

		const result = `{"forced":true}`
		if err := store.AdminForceComplete(ctx, wfID, before.Generation, result, adminTestOperator); err != nil {
			t.Fatalf("AdminForceComplete: %v", err)
		}

		after := mustGetWorkflow(t, ctx, store, wfID)
		if after.Status != "done" {
			t.Errorf("status = %q, want done", after.Status)
		}
		if !strings.Contains(after.Result, `"forced"`) {
			t.Errorf("result = %q, want the operator-supplied %s", after.Result, result)
		}
		if after.AssignedTo != "" {
			t.Errorf("assigned_to = %q, want empty: the workflow must not still be owned", after.AssignedTo)
		}
		// The bump is what fences the worker that may still be alive. Without
		// it the status write alone leaves a heartbeat or segment finalize
		// from the old owner able to match on (assigned_to, generation).
		if after.Generation != before.Generation+1 {
			t.Errorf("generation = %d, want %d (bumped)", after.Generation, before.Generation+1)
		}

		audit := lastAdminEvent(t, ctx, store, wfID)
		if audit.Op != adminActionForceComplete {
			t.Errorf("audit event operation = %q, want %q", audit.Op, adminActionForceComplete)
		}
		if audit.Service != adminTestOperator {
			t.Errorf("audit event operator = %q, want %q", audit.Service, adminTestOperator)
		}

		// The audit event is written through appendEventsInTx, so it joins
		// the checksum chain rather than sitting beside it. A raw INSERT
		// would leave the chain unverifiable from this event onwards.
		if err := store.VerifyWorkflowEvents(ctx, wfID); err != nil {
			t.Errorf("VerifyWorkflowEvents after force-complete: %v", err)
		}

		// The worker that owned the run is now fenced out of its own
		// completion, which is the reason force-resolve is safe to use on a
		// workflow whose worker may not actually be dead.
		err := store.CompleteWorkflow(ctx, wfID, workerID, before.Generation, `{"from":"worker"}`, nil)
		if !errors.Is(err, ErrFenceLost) {
			t.Errorf("the previous owner's CompleteWorkflow returned %v, want ErrFenceLost", err)
		}
		if final := mustGetWorkflow(t, ctx, store, wfID); !strings.Contains(final.Result, `"forced"`) {
			t.Errorf("result = %q after the old owner completed; the operator's result was overwritten", final.Result)
		}
	})
}

// TestAdminForceFail_ResolvesAndAudits is the same for the failure side,
// including the error classification the operator supplies.
func TestAdminForceFail_ResolvesAndAudits(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID, _ := startClaimedWorkflow(t, ctx, store, "aff")

		before := mustGetWorkflow(t, ctx, store, wfID)
		if err := store.AdminForceFail(ctx, wfID, before.Generation,
			"stuck behind a deleted queue", "OPERATOR_ABANDONED", adminTestOperator); err != nil {
			t.Fatalf("AdminForceFail: %v", err)
		}

		after := mustGetWorkflow(t, ctx, store, wfID)
		if after.Status != "failed" {
			t.Errorf("status = %q, want failed", after.Status)
		}
		if after.Error != "stuck behind a deleted queue" {
			t.Errorf("error_msg = %q, want the operator's message", after.Error)
		}
		if after.ErrorCode != "OPERATOR_ABANDONED" {
			t.Errorf("error_code = %q, want OPERATOR_ABANDONED", after.ErrorCode)
		}
		if after.ErrorOp != "admin_force_fail" {
			t.Errorf("error_op = %q, want admin_force_fail: the failure did not come from a workflow operation", after.ErrorOp)
		}
		if after.Generation != before.Generation+1 {
			t.Errorf("generation = %d, want %d (bumped)", after.Generation, before.Generation+1)
		}

		audit := lastAdminEvent(t, ctx, store, wfID)
		if audit.Op != adminActionForceFail {
			t.Errorf("audit event operation = %q, want %q", audit.Op, adminActionForceFail)
		}
		if audit.Service != adminTestOperator {
			t.Errorf("audit event operator = %q, want %q", audit.Service, adminTestOperator)
		}
		// The reason carries both halves of the classification, so the audit
		// record says what the operator claimed and not merely that somebody
		// failed the workflow.
		if !strings.Contains(audit.Err, "OPERATOR_ABANDONED") || !strings.Contains(audit.Err, "stuck behind") {
			t.Errorf("audit event reason = %q, want it to carry the operator's code and message", audit.Err)
		}
		if err := store.VerifyWorkflowEvents(ctx, wfID); err != nil {
			t.Errorf("VerifyWorkflowEvents after force-fail: %v", err)
		}
	})
}

// TestAdminForceComplete_ClearsAnEarlierFailure covers the one state only
// force-resolve can produce: an operator repairing a workflow that has already
// failed. Leaving the old error on a row now marked done would mean a `done`
// workflow carrying a failure message, which nothing else in the engine can
// create and every reader of those columns would have to know to ignore.
func TestAdminForceComplete_ClearsAnEarlierFailure(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID, _ := startClaimedWorkflow(t, ctx, store, "acf")

		before := mustGetWorkflow(t, ctx, store, wfID)
		if err := store.AdminForceFail(ctx, wfID, before.Generation,
			"first diagnosis was wrong", "WRONG", adminTestOperator); err != nil {
			t.Fatalf("AdminForceFail: %v", err)
		}
		failed := mustGetWorkflow(t, ctx, store, wfID)
		if failed.Error == "" {
			t.Fatalf("fixture: the workflow was not left with an error message")
		}

		if err := store.AdminForceComplete(ctx, wfID, failed.Generation,
			`{"repaired":true}`, adminTestOperator); err != nil {
			t.Fatalf("AdminForceComplete on a failed workflow: %v", err)
		}

		after := mustGetWorkflow(t, ctx, store, wfID)
		if after.Status != "done" {
			t.Errorf("status = %q, want done", after.Status)
		}
		if after.Error != "" || after.ErrorCode != "" || after.ErrorOp != "" {
			t.Errorf("a completed workflow still carries its old failure: error_msg=%q error_code=%q error_op=%q",
				after.Error, after.ErrorCode, after.ErrorOp)
		}
		// Both actions are on the record; the audit trail is append-only.
		if n := countAdminEvents(t, ctx, store, wfID); n != 2 {
			t.Errorf("%d admin_action events, want 2 (the force-fail and the force-complete)", n)
		}
		if err := store.VerifyWorkflowEvents(ctx, wfID); err != nil {
			t.Errorf("VerifyWorkflowEvents after two admin actions: %v", err)
		}
	})
}

// TestAdminForceResolve_GenerationMismatch pins the case the HTTP layer turns
// into a 409. A stale generation must leave the workflow alone entirely --
// including its history, because a rolled-back status change that still
// recorded an admin_action would be an audit trail that lies in the other
// direction.
func TestAdminForceResolve_GenerationMismatch(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID, _ := startClaimedWorkflow(t, ctx, store, "agm")
		before := mustGetWorkflow(t, ctx, store, wfID)

		stale := before.Generation + 7
		err := store.AdminForceComplete(ctx, wfID, stale, `{}`, adminTestOperator)
		if err == nil {
			t.Fatal("AdminForceComplete with a stale generation succeeded")
		}
		if !strings.Contains(err.Error(), "generation mismatch") {
			t.Errorf("error = %v, want it to say generation mismatch (the HTTP layer matches on that to answer 409)", err)
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("%d", before.Generation)) {
			t.Errorf("error = %v, want it to report the stored generation %d", err, before.Generation)
		}

		after := mustGetWorkflow(t, ctx, store, wfID)
		if after.Status != before.Status || after.Generation != before.Generation {
			t.Errorf("workflow moved on a rejected force-complete: status %q->%q, generation %d->%d",
				before.Status, after.Status, before.Generation, after.Generation)
		}
		if n := countAdminEvents(t, ctx, store, wfID); n != 0 {
			t.Errorf("%d admin_action event(s) recorded for a force-resolve that was refused, want 0", n)
		}
	})
}

// TestAdminForceResolve_UnknownWorkflow pins the 404 case, and pins it as
// distinct from a generation mismatch: the two are different HTTP answers and
// a zero-row UPDATE cannot tell them apart on its own.
func TestAdminForceResolve_UnknownWorkflow(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		missing := fmt.Sprintf("no-such-workflow-%d", time.Now().UnixNano())

		for _, tc := range []struct {
			name string
			call func() error
		}{
			{"force_complete", func() error {
				return store.AdminForceComplete(ctx, missing, 0, `{}`, adminTestOperator)
			}},
			{"force_fail", func() error {
				return store.AdminForceFail(ctx, missing, 0, "boom", "ERR", adminTestOperator)
			}},
		} {
			err := tc.call()
			if err == nil {
				t.Fatalf("%s on a workflow that does not exist succeeded", tc.name)
			}
			if !strings.Contains(err.Error(), "not found") {
				t.Errorf("%s: error = %v, want it to say not found", tc.name, err)
			}
			if strings.Contains(err.Error(), "generation mismatch") {
				t.Errorf("%s: a missing workflow was reported as a generation mismatch: %v", tc.name, err)
			}
		}
	})
}

// TestAdminForceResolve_RefusesAnotherTenant is the store-layer half of the
// ownership gap 1.7 closed at the HTTP layer.
//
// cmd/cleat-worker's callerOwnsTarget is the only place that can compare a
// caller against a workflow, and it stays the primary enforcement point. But
// "the layer above will check" is what left MSSQL's own UPDATE without a
// tenant_id filter in the version of this code that was never merged, and
// AdminForceComplete is exported: any embedder that calls it directly gets
// whatever the store enforces and nothing else.
func TestAdminForceResolve_RefusesAnotherTenant(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID, _ := startClaimedWorkflow(t, ctx, store, "axt")
		before := mustGetWorkflow(t, ctx, store, wfID)

		other := storeScopedToTenant(t, store, "cccccccc-cccc-cccc-cccc-cccccccccccc")

		err := other.AdminForceComplete(ctx, wfID, before.Generation, `{"stolen":true}`, "operator-from-elsewhere")
		if err == nil {
			t.Fatal("a store scoped to another tenant force-completed this tenant's workflow")
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("error = %v, want not found -- reporting a generation mismatch would confirm the workflow exists", err)
		}

		after := mustGetWorkflow(t, ctx, store, wfID)
		if after.Status != before.Status || after.Generation != before.Generation {
			t.Errorf("another tenant's force-complete moved the workflow: status %q->%q, generation %d->%d",
				before.Status, after.Status, before.Generation, after.Generation)
		}
		if strings.Contains(after.Result, "stolen") {
			t.Errorf("result = %q: another tenant's result was written", after.Result)
		}
		if n := countAdminEvents(t, ctx, store, wfID); n != 0 {
			t.Errorf("%d admin_action event(s) written by another tenant, want 0", n)
		}
	})
}

// TestAdminForceResolve_AuditCollisionRollsBack covers the branch that exists
// because every dialect's event append is an upsert that leaves an existing
// row alone. If another writer takes the step number the audit event was going
// to use, the append is a silent no-op -- so without this check a force-resolve
// would commit the status change with no audit record and return success.
//
// The collision is produced deterministically rather than by racing two
// transactions: the row is planted at the step the audit append will choose,
// under a different tenant, so it is invisible to the tenant-scoped
// MAX(step) query but still collides on the (workflow_id, step) primary key.
// That is the same situation the concurrent writer creates, without depending
// on an interleaving that a test can only wait for.
//
// PostgreSQL only: this needs a second, raw connection to the same database to
// plant the row, and the branch it exercises is identical on all three.
func TestAdminForceResolve_AuditCollisionRollsBack(t *testing.T) {
	backend := &PostgresBackend{}
	store, teardown := backend.Setup(t)
	defer teardown()

	ctx := context.Background()
	wfID, _ := startClaimedWorkflow(t, ctx, store, "acr")
	before := mustGetWorkflow(t, ctx, store, wfID)

	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()

	var nextStep int
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(step), -1) + 1 FROM event_history WHERE workflow_id = $1 AND tenant_id = $2`,
		wfID, DefaultTenantUUID).Scan(&nextStep); err != nil {
		t.Fatalf("read next step: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO event_history (workflow_id, step, event_type, service, operation, response, tenant_id)
		VALUES ($1, $2, 'call', 'someone-else', 'not-an-admin-action', '', $3)
	`, wfID, nextStep, "dddddddd-dddd-dddd-dddd-dddddddddddd"); err != nil {
		t.Fatalf("plant colliding event: %v", err)
	}

	err := store.AdminForceComplete(ctx, wfID, before.Generation, `{"forced":true}`, adminTestOperator)
	if err == nil {
		t.Fatal("force-complete succeeded even though its audit event was displaced")
	}
	if !strings.Contains(err.Error(), "audit") {
		t.Errorf("error = %v, want it to name the audit event as the reason", err)
	}

	after := mustGetWorkflow(t, ctx, store, wfID)
	if after.Status != before.Status || after.Generation != before.Generation {
		t.Errorf("the status change committed without an audit record: status %q->%q, generation %d->%d",
			before.Status, after.Status, before.Generation, after.Generation)
	}
}

// TestAdminActionEventPayloadRoundTrip guards the half of the audit record
// that no dialect test can see.
//
// eventRecordToPayload had no admin_action arm, so the payload was "{}" --
// and computeEventChecksum hashes payload alone. The chain would still have
// verified while operator and action, which then live only in the columns,
// could be edited afterwards with nothing to detect it.
//
// No dialect test above can see that, on any backend, which is why this one
// exists. verifyShadowColumns looks like it would catch it and does not:
// populateFromPayload only overwrites the keys a payload carries, so a key
// the payload omits keeps the column's own value and compares equal. An empty
// payload reads as agreement. Removing the arm from eventRecordToPayload
// leaves every database test in this file green and fails only here.
func TestAdminActionEventPayloadRoundTrip(t *testing.T) {
	rec := EventRecordFromEvent(AdminActionEvent{
		step:     3,
		Action:   adminActionForceFail,
		Operator: adminTestOperator,
		Reason:   "OPERATOR_ABANDONED: stuck",
	})

	payload, err := eventRecordToPayload(rec)
	if err != nil {
		t.Fatalf("eventRecordToPayload: %v", err)
	}
	for _, want := range []string{adminActionForceFail, adminTestOperator, "OPERATOR_ABANDONED"} {
		if !strings.Contains(string(payload), want) {
			t.Errorf("payload %s does not carry %q, so the checksum does not cover it", payload, want)
		}
	}

	// Reconstruction must agree with the columns, or verifyShadowColumns
	// reports every admin action as a tampered row.
	got := EventRecord{Step: rec.Step, EventType: rec.EventType}
	populateFromPayload(&got, payload)
	if got.Service != rec.Service || got.Op != rec.Op || got.Err != rec.Err {
		t.Errorf("round trip: got service=%q op=%q err=%q, want service=%q op=%q err=%q",
			got.Service, got.Op, got.Err, rec.Service, rec.Op, rec.Err)
	}
}

// TestForceComplete_ResultMustBeJSON covers the validation in admin_ops.go.
// The result column is JSON-typed on every dialect, so a non-JSON result is a
// bad request rather than three different driver errors reported as a 500.
func TestForceComplete_ResultMustBeJSON(t *testing.T) {
	store := &adminOpsTestStore{}

	if err := ForceComplete(context.Background(), store, "wf-1", 0, "op", "not json"); err == nil ||
		!strings.Contains(err.Error(), "must be valid JSON") {
		t.Errorf("expected a 'must be valid JSON' error, got: %v", err)
	}
	// An omitted result is not an error: it means "no result", which is JSON
	// null, and is what the HTTP handler sends when the body has no result.
	if err := ForceComplete(context.Background(), store, "wf-1", 0, "op", ""); err != nil {
		t.Errorf("an empty result should be accepted as null, got: %v", err)
	}
}
