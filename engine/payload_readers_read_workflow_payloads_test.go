package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// cleat#3245 B1a: every PostgreSQL reader of a workflow_payloads column now
// reads workflow_payloads, not workflow_instances, and the two compaction
// writes step 2 never mirrored (CompactHistory, ClearExpiredCompactionState)
// now are.
//
// Same shape as step 3's diverging tests: the dual-write keeps both copies
// identical, so a test that only drives normal writes cannot tell which copy
// a reader used. Each test below sets the two copies to DIFFERENT values
// behind the store's back, and the answer says which one was read. B1b stops
// writing the workflow_instances copy; every reader here would then have read
// a default or a NULL, which is what these tests are standing in for.

// startPayloadReaderRun deploys a definition and starts one run of it.
func startPayloadReaderRun(t *testing.T, store *PostgresStore, defName string) string {
	t.Helper()
	ctx := context.Background()
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d}, ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
	id, _, err := store.StartNewRun(ctx, "", defName, 1, json.RawMessage(`{"from":"start"}`), defName+"-key", DefaultTenantUUID, 0)
	if err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}
	return id
}

// diverge runs one UPDATE against one table for one id, bypassing every
// store write path (and therefore every mirror).
func diverge(t *testing.T, db *sql.DB, table, set, id string, args ...any) {
	t.Helper()
	q := `UPDATE ` + table + ` SET ` + set + ` WHERE id = $1`
	if _, err := db.Exec(q, append([]any{id}, args...)...); err != nil {
		t.Fatalf("diverge %s (%s): %v", table, set, err)
	}
}

func TestQueryStateReadersReadWorkflowPayloads(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()
	id := startPayloadReaderRun(t, store, "b1a-query-state")

	diverge(t, db, "workflow_payloads", `query_state = '{"k":"payloads"}'`, id)
	diverge(t, db, "workflow_instances", `query_state = '{"k":"instances"}'`, id)

	got, err := store.GetQueryState(ctx, id, "k")
	if err != nil {
		t.Fatalf("GetQueryState: %v", err)
	}
	if got != "payloads" {
		t.Errorf("GetQueryState = %q, want %q -- it read workflow_instances.query_state", got, "payloads")
	}
	all, err := store.ListQueryState(ctx, id)
	if err != nil {
		t.Fatalf("ListQueryState: %v", err)
	}
	if all["k"] != "payloads" {
		t.Errorf("ListQueryState = %v, want k=payloads -- it read workflow_instances.query_state", all)
	}
}

func TestLoadCompactionStateReadsWorkflowPayloads(t *testing.T) {
	store, db := newRunTestStore(t)
	id := startPayloadReaderRun(t, store, "b1a-load-compaction")

	diverge(t, db, "workflow_payloads", `compaction_state = '{"version":1,"compacted_step":7,"events":[]}'`, id)
	diverge(t, db, "workflow_instances", `compaction_state = '{"version":1,"compacted_step":99,"events":[]}'`, id)

	cs, err := store.LoadCompactionState(context.Background(), id)
	if err != nil {
		t.Fatalf("LoadCompactionState: %v", err)
	}
	if cs == nil || cs.CompactedStep != 7 {
		t.Fatalf("LoadCompactionState = %+v, want CompactedStep 7 -- 99 means it read workflow_instances", cs)
	}
}

// CompactHistory deletes event_history below keepStep. If the snapshot it
// writes is not on the copy LoadCompactionState reads, replay of a compacted
// workflow has neither the events nor the snapshot. This is the step-2 gap.
func TestCompactHistoryMirrorsOntoWorkflowPayloads(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()
	id := startPayloadReaderRun(t, store, "b1a-compact-mirror")

	snapshot := []byte(`{"version":1,"compacted_step":3,"events":[]}`)
	if err := store.CompactHistory(ctx, id, snapshot, 3, 4); err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}
	var state sql.NullString
	var step sql.NullInt64
	if err := db.QueryRow(`SELECT compaction_state::text, compaction_step FROM workflow_payloads WHERE id = $1`, id).Scan(&state, &step); err != nil {
		t.Fatalf("read workflow_payloads: %v", err)
	}
	if !state.Valid || !step.Valid || step.Int64 != 3 {
		t.Fatalf("workflow_payloads compaction_state=%v compaction_step=%v, want the snapshot and 3 -- "+
			"CompactHistory wrote workflow_instances only", state, step)
	}
	cs, err := store.LoadCompactionState(ctx, id)
	if err != nil || cs == nil || cs.CompactedStep != 3 {
		t.Fatalf("LoadCompactionState after CompactHistory = %+v, %v; want CompactedStep 3", cs, err)
	}
}

func TestClearExpiredCompactionStateClearsWorkflowPayloads(t *testing.T) {
	store, db := newRunTestStore(t)
	id := startPayloadReaderRun(t, store, "b1a-clear-compaction")

	// Terminal by status (still a workflow_instances read until B3), expired
	// by workflow_payloads.completed_at only: workflow_instances.completed_at
	// is NULL, so a predicate still reading it selects nothing.
	diverge(t, db, "workflow_instances", `status = 'done', completed_at = NULL, compaction_state = '{}'`, id)
	diverge(t, db, "workflow_payloads", `completed_at = now() - interval '2 hours', compaction_state = '{}', compaction_step = 5`, id)

	n, err := store.ClearExpiredCompactionState(context.Background(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("ClearExpiredCompactionState: %v", err)
	}
	if n != 1 {
		t.Fatalf("ClearExpiredCompactionState cleared %d, want 1 -- 0 means the predicate read "+
			"workflow_instances.completed_at, which is NULL here", n)
	}
	var pState, iState sql.NullString
	var pStep sql.NullInt64
	if err := db.QueryRow(`SELECT compaction_state::text, compaction_step FROM workflow_payloads WHERE id = $1`, id).Scan(&pState, &pStep); err != nil {
		t.Fatalf("read workflow_payloads: %v", err)
	}
	if err := db.QueryRow(`SELECT compaction_state::text FROM workflow_instances WHERE id = $1`, id).Scan(&iState); err != nil {
		t.Fatalf("read workflow_instances: %v", err)
	}
	if pState.Valid || pStep.Valid {
		t.Errorf("workflow_payloads still carries compaction_state=%v step=%v -- the clear was not mirrored", pState, pStep)
	}
	if iState.Valid {
		t.Errorf("workflow_instances still carries compaction_state=%v -- the dual-write stopped early", iState)
	}
}

func TestGetChildResultReadsWorkflowPayloads(t *testing.T) {
	store, db := newRunTestStore(t)
	id := startPayloadReaderRun(t, store, "b1a-child-result")

	diverge(t, db, "workflow_instances", `status = 'done', result = '{"from":"instances"}'`, id)
	diverge(t, db, "workflow_payloads", `result = '{"from":"payloads"}'`, id)

	out, err := store.GetChildResult(context.Background(), id)
	if err != nil {
		t.Fatalf("GetChildResult: %v", err)
	}
	if !out.Completed || !strings.Contains(out.Result, "payloads") {
		t.Fatalf("GetChildResult = %+v, want a completed result from workflow_payloads", out)
	}
}

func TestGetChildCompletedAtMsReadsWorkflowPayloads(t *testing.T) {
	store, db := newRunTestStore(t)
	id := startPayloadReaderRun(t, store, "b1a-child-completed-at")

	want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	diverge(t, db, "workflow_instances", `completed_at = NULL`, id)
	diverge(t, db, "workflow_payloads", `completed_at = $2`, id, want)

	ms, ok, err := store.GetChildCompletedAtMs(context.Background(), id)
	if err != nil {
		t.Fatalf("GetChildCompletedAtMs: %v", err)
	}
	if !ok || ms != want.UnixMilli() {
		t.Fatalf("GetChildCompletedAtMs = %d, %v; want %d, true -- false means it read workflow_instances", ms, ok, want.UnixMilli())
	}
}

func TestGetAllowedSignalCallersReadsWorkflowPayloads(t *testing.T) {
	store, db := newRunTestStore(t)
	id := startPayloadReaderRun(t, store, "b1a-allowed-signals")

	diverge(t, db, "workflow_payloads", `allowed_signals = '["from-payloads"]'`, id)
	diverge(t, db, "workflow_instances", `allowed_signals = '["from-instances"]'`, id)

	got, err := store.GetAllowedSignalCallers(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAllowedSignalCallers: %v", err)
	}
	if len(got) != 1 || got[0] != "from-payloads" {
		t.Fatalf("GetAllowedSignalCallers = %v, want [from-payloads]", got)
	}
}

// def_name on workflow_instances carries a foreign key to workflow_defs, so
// the decoy goes on workflow_payloads, which has none: the diverged name is
// one only workflow_payloads holds.
func TestActiveInstanceCountsReadDefNameFromWorkflowPayloads(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()
	id := startPayloadReaderRun(t, store, "b1a-active-counts")
	diverge(t, db, "workflow_payloads", `def_name = 'b1a-diverged-def'`, id)

	if n, err := store.CountActiveInstances(ctx, "b1a-diverged-def", 1); err != nil || n != 1 {
		t.Errorf("CountActiveInstances(diverged) = %d, %v; want 1 -- it read workflow_instances.def_name", n, err)
	}
	if n, err := store.CountActiveInstances(ctx, "b1a-active-counts", 1); err != nil || n != 0 {
		t.Errorf("CountActiveInstances(original) = %d, %v; want 0 -- it read workflow_instances.def_name", n, err)
	}
	counts, err := store.GetActiveInstanceCountsByVersion(ctx)
	if err != nil {
		t.Fatalf("GetActiveInstanceCountsByVersion: %v", err)
	}
	if counts["b1a-diverged-def:1"] != 1 || counts["b1a-active-counts:1"] != 0 {
		t.Errorf("GetActiveInstanceCountsByVersion = %v, want only b1a-diverged-def:1", counts)
	}
	active, err := (&WorkflowLoader{db: db}).ActiveVersions(ctx)
	if err != nil {
		t.Fatalf("ActiveVersions: %v", err)
	}
	if len(active["b1a-diverged-def"]) != 1 || len(active["b1a-active-counts"]) != 0 {
		t.Errorf("ActiveVersions = %v, want only b1a-diverged-def", active)
	}
}

// The claim is what hands a worker the run's input. If it read
// workflow_instances, B1b would make every claimed run start from '{}', and
// nothing in the suite before this test noticed: measured with a trigger that
// forces workflow_instances.input to its default on every write, the whole
// engine suite stayed green on the claim path.
func TestTheClaimHandsBackWorkflowPayloadsColumns(t *testing.T) {
	store, db := newRunTestStore(t)
	id := startPayloadReaderRun(t, store, "b1a-claim-payloads")

	diverge(t, db, "workflow_payloads", `input = '{"from":"payloads"}', trace_id = 'trace-payloads'`, id)
	diverge(t, db, "workflow_instances", `input = '{"from":"instances"}', trace_id = 'trace-instances'`, id)

	wf, err := store.ClaimWorkflow(context.Background(), "b1a-worker")
	if err != nil {
		t.Fatalf("ClaimWorkflow: %v", err)
	}
	if wf == nil || wf.ID != id {
		t.Fatalf("ClaimWorkflow = %+v, want run %s", wf, id)
	}
	if !strings.Contains(string(wf.Input), "payloads") || wf.TraceID != "trace-payloads" {
		t.Fatalf("claim handed back input=%s trace=%q, want workflow_payloads' values", wf.Input, wf.TraceID)
	}
}

func TestTheStickyClaimHandsBackWorkflowPayloadsColumns(t *testing.T) {
	store, db := newRunTestStore(t)
	ctx := context.Background()
	id := startPayloadReaderRun(t, store, "b1a-sticky-claim-payloads")
	if err := store.UpdateStickyWorker(ctx, id, "b1a-sticky-worker"); err != nil {
		t.Fatalf("UpdateStickyWorker: %v", err)
	}

	diverge(t, db, "workflow_payloads", `input = '{"from":"payloads"}', trace_id = 'trace-payloads'`, id)
	diverge(t, db, "workflow_instances", `input = '{"from":"instances"}', trace_id = 'trace-instances'`, id)

	wfs, err := store.ClaimStickyWorkflows(ctx, "b1a-sticky-worker", 10)
	if err != nil {
		t.Fatalf("ClaimStickyWorkflows: %v", err)
	}
	if len(wfs) != 1 || wfs[0].ID != id {
		t.Fatalf("ClaimStickyWorkflows = %v, want run %s", wfs, id)
	}
	if !strings.Contains(string(wfs[0].Input), "payloads") || wfs[0].TraceID != "trace-payloads" {
		t.Fatalf("sticky claim handed back input=%s trace=%q, want workflow_payloads' values", wfs[0].Input, wfs[0].TraceID)
	}
}

// A run with no workflow_payloads row is a broken invariant. The claim must
// fail visibly, not skip the run forever with nothing said.
func TestAClaimOfARunWithNoPayloadsRowFailsLoudly(t *testing.T) {
	store, db := newRunTestStore(t)
	id := startPayloadReaderRun(t, store, "b1a-claim-no-payloads")
	if _, err := db.Exec(`DELETE FROM workflow_payloads WHERE id = $1`, id); err != nil {
		t.Fatalf("delete workflow_payloads row: %v", err)
	}

	wfs, err := store.ClaimWorkflows(context.Background(), "b1a-worker", 10)
	if err == nil {
		t.Fatalf("ClaimWorkflows = %v, nil; want an error for a run with no workflow_payloads row", wfs)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM workflow_instances WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "ready" {
		t.Errorf("status after the failed claim = %q, want ready -- the claim did not roll back", status)
	}
}

func TestDeleteCompletedWorkflowsReadsCompletedAtFromWorkflowPayloads(t *testing.T) {
	store, db := newRunTestStore(t)
	id := startPayloadReaderRun(t, store, "b1a-retention-completed")

	diverge(t, db, "workflow_instances", `status = 'done', completed_at = NULL`, id)
	diverge(t, db, "workflow_payloads", `completed_at = now() - interval '2 hours'`, id)

	n, err := store.DeleteCompletedWorkflows(context.Background(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("DeleteCompletedWorkflows: %v", err)
	}
	if n != 1 {
		t.Fatalf("DeleteCompletedWorkflows deleted %d, want 1 -- 0 means the predicate read "+
			"workflow_instances.completed_at, which is NULL here", n)
	}
}

func TestReadReplacedOutcomeReadsWorkflowPayloadsOnPostgres(t *testing.T) {
	store, db := newRunTestStore(t)
	id := startPayloadReaderRun(t, store, "b1a-replaced-outcome")

	diverge(t, db, "workflow_instances", `status = 'failed', error_msg = 'from-instances'`, id)
	diverge(t, db, "workflow_payloads", `error_msg = 'from-payloads'`, id)

	out, err := readReplacedOutcome(context.Background(), db, DialectPostgres, id, DefaultTenantUUID)
	if err != nil {
		t.Fatalf("readReplacedOutcome: %v", err)
	}
	if out == nil || out.Status != "failed" || out.ErrorMsg != "from-payloads" {
		t.Fatalf("readReplacedOutcome = %+v, want status failed and error_msg from workflow_payloads", out)
	}
}

func TestDeferPhaseOwedReadsCompactionStateFromWorkflowPayloadsOnPostgres(t *testing.T) {
	store, db := newRunTestStore(t)
	id := startPayloadReaderRun(t, store, "b1a-defer-owed")
	owed := func() bool {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_instances WHERE id = $1 AND `+deferPhaseOwedSQLPostgres, id).Scan(&n); err != nil {
			t.Fatalf("evaluate deferPhaseOwedSQLPostgres: %v", err)
		}
		return n == 1
	}

	diverge(t, db, "workflow_payloads", `compaction_state = '{}'`, id)
	diverge(t, db, "workflow_instances", `compaction_state = NULL`, id)
	if !owed() {
		t.Errorf("compacted on workflow_payloads only: predicate says not owed -- it read workflow_instances")
	}
	diverge(t, db, "workflow_payloads", `compaction_state = NULL`, id)
	diverge(t, db, "workflow_instances", `compaction_state = '{}'`, id)
	if owed() {
		t.Errorf("compacted on workflow_instances only: predicate says owed -- it read workflow_instances")
	}
}

// preemptivelySettle (TerminateWorkflow's body) decides one phase or two from
// whether the run owes a defer phase, and a compacted run owes one. With no
// defer events at all, compaction is the only thing that can make it owe.
func TestTerminateTreatsARunCompactedOnWorkflowPayloadsAsOwingADeferPhase(t *testing.T) {
	store, db := newRunTestStore(t)
	id := startPayloadReaderRun(t, store, "b1a-terminate-compacted")

	diverge(t, db, "workflow_payloads", `compaction_state = '{}'`, id)
	diverge(t, db, "workflow_instances", `compaction_state = NULL`, id)

	if err := store.TerminateWorkflow(context.Background(), id, "b1a"); err != nil {
		t.Fatalf("TerminateWorkflow: %v", err)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM workflow_instances WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != statusTerminating {
		t.Fatalf("status = %q, want %q -- the run is compacted on workflow_payloads, so it owes a defer phase; "+
			"a one-phase terminate means the check read workflow_instances", status, statusTerminating)
	}
}
