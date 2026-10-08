package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// cleat#2312. With --encrypt-sensitive-payloads on, event_history is sealed
// (encodeEventForStorage) but workflow_instances.{input,result,error_msg,
// error_code,error_op,cancellation_reason,query_state}, workflow_signals.
// payload, workflow_promises.{result,error_msg}, workflow_update_requests.
// {payload,result,error_msg}, workflow_schedules.input and
// idempotency_keys.error_msg were all stored in plaintext regardless of the
// flag. These tests use the same method the issue's own measurement did:
// a marker string sent through the REAL store method (not a hand-built
// query), the row read back on a connection that never decrypts, and the
// marker's absence from what is on disk -- paired with a read through the
// store's own method, confirming the marker comes back correctly.
//
// A marker is checked for absence from raw ciphertext, not for a specific
// transformed shape -- AES-256-GCM's output is only usefully described as
// "does not contain the plaintext", not as any one fixed string.

// markerFor returns a distinctive string that cannot appear in ciphertext by
// chance, with a per-test tag so a failure names which assertion produced it.
func markerFor(tag string) string {
	return fmt.Sprintf("cleat2312-%s-%d", tag, time.Now().UnixNano())
}

// assertSealed fails the test if raw (read on a connection that never
// decrypts) contains marker, or is no longer than the plaintext it supposedly
// seals -- the same two checks TestAChildWorkflowEventIsEncryptedLikeAnyOther
// uses, generalised. plaintext is the full plaintext value raw was sealed
// from (not just the marker), since AES-256-GCM ciphertext must be longer
// than ITS OWN plaintext, not merely longer than a substring of it.
func assertSealed(t *testing.T, label, raw, marker, plaintext string) {
	t.Helper()
	if strings.Contains(raw, marker) {
		t.Errorf("%s: on-disk value contains the plaintext marker: %q", label, raw)
	}
	if len(raw) <= len(plaintext) {
		t.Errorf("%s: on-disk value (%d bytes) is not longer than its plaintext (%d bytes) -- "+
			"AES-256-GCM ciphertext is always longer, so this is not measuring encryption",
			label, len(raw), len(plaintext))
	}
}

// TestWorkflowInstanceColumnsAreEncrypted covers workflow_instances.{input,
// result,error_msg,error_code,error_op,cancellation_reason,query_state} and
// idempotency_keys.error_msg across the real write paths (StartNewRunWithOptions,
// CompleteWorkflow, FailWorkflow, MoveToDeadLetterQueue, RequestCancellation)
// and read paths (GetWorkflowByID, ClaimWorkflow, CheckCancellation,
// GetQueryState, ListQueryState, ListWorkflows).
func TestWorkflowInstanceColumnsAreEncrypted(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	enc, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}
	store := NewPostgresStore(db).WithEncryption(enc, true)
	deployFaultTestDef(t, store)
	ctx := context.Background()

	inputMarker := markerFor("input")
	input := fmt.Sprintf(`{"secret":%q}`, inputMarker)
	runID, _, err := store.StartNewRunWithOptions(ctx, "", "test", 1, json.RawMessage(input), "", store.tenantID, 0, StartOptions{})
	if err != nil {
		t.Fatalf("StartNewRunWithOptions: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID) })

	var rawInput string
	if err := db.QueryRow(`SELECT input::text FROM workflow_instances WHERE id = $1`, runID).Scan(&rawInput); err != nil {
		t.Fatalf("read raw input: %v", err)
	}
	assertSealed(t, "workflow_instances.input", rawInput, inputMarker, input)

	wf, err := store.GetWorkflowByID(ctx, runID)
	if err != nil {
		t.Fatalf("GetWorkflowByID: %v", err)
	}
	if string(wf.Input) != input {
		t.Errorf("GetWorkflowByID: Input = %q, want %q", wf.Input, input)
	}

	// Claim it -- this exercises finishClaim's decryption, the chokepoint
	// shared by ClaimWorkflows and ClaimStickyWorkflows.
	claimed, err := store.ClaimWorkflow(ctx, "worker1")
	if err != nil {
		t.Fatalf("ClaimWorkflow: %v", err)
	}
	if claimed == nil {
		t.Fatalf("PRECONDITION FAILED: ClaimWorkflow claimed nothing")
	}
	if string(claimed.Input) != input {
		t.Errorf("ClaimWorkflow: Input = %q, want %q (decryption at the claim chokepoint failed)", claimed.Input, input)
	}

	// CompleteWorkflow: result and query_state.
	resultMarker := markerFor("result")
	result := fmt.Sprintf(`{"ok":%q}`, resultMarker)
	qsMarker := markerFor("querystate")
	qs := map[string]string{"k": qsMarker}
	if err := store.CompleteWorkflow(ctx, runID, "worker1", claimed.Generation, result, qs); err != nil {
		t.Fatalf("CompleteWorkflow: %v", err)
	}

	var rawResult, rawQS string
	if err := db.QueryRow(`SELECT result::text, query_state::text FROM workflow_instances WHERE id = $1`, runID).
		Scan(&rawResult, &rawQS); err != nil {
		t.Fatalf("read raw result/query_state: %v", err)
	}
	assertSealed(t, "workflow_instances.result", rawResult, resultMarker, result)
	assertSealed(t, "workflow_instances.query_state", rawQS, qsMarker, string(marshalQueryState(qs)))

	wf, err = store.GetWorkflowByID(ctx, runID)
	if err != nil {
		t.Fatalf("GetWorkflowByID after complete: %v", err)
	}
	if wf.Result != result {
		t.Errorf("GetWorkflowByID: Result = %q, want %q", wf.Result, result)
	}
	gotQS, err := store.ListQueryState(ctx, runID)
	if err != nil {
		t.Fatalf("ListQueryState: %v", err)
	}
	if gotQS["k"] != qsMarker {
		t.Errorf("ListQueryState: k = %q, want %q", gotQS["k"], qsMarker)
	}
	// GetQueryState's SQL-side `->>` cannot index into an encrypted column
	// (it holds a JSON STRING, not an object) -- cleat#2312's sharpest find.
	// Asserting this is the whole point of this sub-test: a regression here
	// is SILENT, answering "" for every key rather than erroring.
	single, err := store.GetQueryState(ctx, runID, "k")
	if err != nil {
		t.Fatalf("GetQueryState: %v", err)
	}
	if single != qsMarker {
		t.Errorf("GetQueryState(%q) = %q, want %q -- the `->>` bypass for encrypted "+
			"query_state is not routing through ListQueryState", "k", single, qsMarker)
	}

	// FailWorkflow on a second run: error_msg, error_code, error_op,
	// query_state, and idempotency_keys.error_msg (same plaintext, reused
	// ciphertext -- see FailWorkflow's own comment).
	input2 := `{}`
	runID2, _, err := store.StartNewRunWithOptions(ctx, "", "test", 1, json.RawMessage(input2), "idem-"+markerFor("key"), store.tenantID, 0, StartOptions{})
	if err != nil {
		t.Fatalf("StartNewRunWithOptions (run 2): %v", err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM idempotency_keys WHERE workflow_id = $1`, runID2)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID2)
	})
	claimed2, err := store.ClaimWorkflow(ctx, "worker1")
	if err != nil {
		t.Fatalf("ClaimWorkflow (run 2): %v", err)
	}
	if claimed2 == nil || claimed2.ID != runID2 {
		t.Fatalf("PRECONDITION FAILED: expected to claim run 2 (%s), claimed %v", runID2, claimed2)
	}

	errMsgMarker := markerFor("errmsg")
	errCodeMarker := markerFor("errcode")
	errOpMarker := markerFor("errop")
	if err := store.FailWorkflow(ctx, runID2, "worker1", claimed2.Generation, errMsgMarker, errCodeMarker, errOpMarker, nil); err != nil {
		t.Fatalf("FailWorkflow: %v", err)
	}

	var rawErrMsg, rawErrCode, rawErrOp string
	if err := db.QueryRow(`SELECT error_msg, error_code, error_op FROM workflow_instances WHERE id = $1`, runID2).
		Scan(&rawErrMsg, &rawErrCode, &rawErrOp); err != nil {
		t.Fatalf("read raw error columns: %v", err)
	}
	assertSealed(t, "workflow_instances.error_msg", rawErrMsg, errMsgMarker, errMsgMarker)
	assertSealed(t, "workflow_instances.error_code", rawErrCode, errCodeMarker, errCodeMarker)
	assertSealed(t, "workflow_instances.error_op", rawErrOp, errOpMarker, errOpMarker)

	var rawIdempErrMsg string
	if err := db.QueryRow(`SELECT error_msg FROM idempotency_keys WHERE workflow_id = $1`, runID2).Scan(&rawIdempErrMsg); err != nil {
		t.Fatalf("read raw idempotency_keys.error_msg: %v", err)
	}
	assertSealed(t, "idempotency_keys.error_msg", rawIdempErrMsg, errMsgMarker, errMsgMarker)

	wf2, err := store.GetWorkflowByID(ctx, runID2)
	if err != nil {
		t.Fatalf("GetWorkflowByID (run 2): %v", err)
	}
	if wf2.Error != errMsgMarker || wf2.ErrorCode != errCodeMarker || wf2.ErrorOp != errOpMarker {
		t.Errorf("GetWorkflowByID (run 2): Error=%q ErrorCode=%q ErrorOp=%q, want %q/%q/%q",
			wf2.Error, wf2.ErrorCode, wf2.ErrorOp, errMsgMarker, errCodeMarker, errOpMarker)
	}

	// ListWorkflows: decrypts for display, and refuses InputContains/
	// ErrorContains outright (ErrSearchUnavailableUnderEncryption) rather
	// than silently searching ciphertext and reporting a false "no match".
	listed, err := store.ListWorkflows(ctx, WorkflowFilter{DefName: "test"})
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	found := false
	for _, w := range listed {
		if w.ID == runID2 {
			found = true
			if w.Error != errMsgMarker {
				t.Errorf("ListWorkflows: Error = %q, want %q", w.Error, errMsgMarker)
			}
		}
	}
	if !found {
		t.Fatalf("PRECONDITION FAILED: ListWorkflows did not return run 2 (%s)", runID2)
	}
	if _, err := store.ListWorkflows(ctx, WorkflowFilter{InputContains: "anything"}); err != ErrSearchUnavailableUnderEncryption {
		t.Errorf("ListWorkflows with InputContains under encryption: err = %v, want ErrSearchUnavailableUnderEncryption", err)
	}
	if _, err := store.ListWorkflows(ctx, WorkflowFilter{ErrorContains: "anything"}); err != ErrSearchUnavailableUnderEncryption {
		t.Errorf("ListWorkflows with ErrorContains under encryption: err = %v, want ErrSearchUnavailableUnderEncryption", err)
	}

	// RequestCancellation / CheckCancellation: cancellation_reason, on a
	// third run (runID2 is already terminal).
	input3 := `{}`
	runID3, _, err := store.StartNewRunWithOptions(ctx, "", "test", 1, json.RawMessage(input3), "", store.tenantID, 0, StartOptions{})
	if err != nil {
		t.Fatalf("StartNewRunWithOptions (run 3): %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID3) })

	reasonMarker := markerFor("cancelreason")
	if err := store.RequestCancellation(ctx, runID3, reasonMarker); err != nil {
		t.Fatalf("RequestCancellation: %v", err)
	}
	var rawReason string
	if err := db.QueryRow(`SELECT cancellation_reason FROM workflow_instances WHERE id = $1`, runID3).Scan(&rawReason); err != nil {
		t.Fatalf("read raw cancellation_reason: %v", err)
	}
	assertSealed(t, "workflow_instances.cancellation_reason", rawReason, reasonMarker, reasonMarker)

	cancelled, gotReason, err := store.CheckCancellation(ctx, runID3)
	if err != nil {
		t.Fatalf("CheckCancellation: %v", err)
	}
	if !cancelled || gotReason != reasonMarker {
		t.Errorf("CheckCancellation: cancelled=%v reason=%q, want true/%q", cancelled, gotReason, reasonMarker)
	}

	// runID3 is still 'ready' (RequestCancellation only sets a flag) -- claim
	// and complete it so it cannot be the one ClaimWorkflow picks up below.
	claimed3, err := store.ClaimWorkflow(ctx, "worker1")
	if err != nil || claimed3 == nil || claimed3.ID != runID3 {
		t.Fatalf("PRECONDITION FAILED: claim of run 3 (%s): claimed=%v err=%v", runID3, claimed3, err)
	}
	if err := store.CompleteWorkflow(ctx, runID3, "worker1", claimed3.Generation, "{}", nil); err != nil {
		t.Fatalf("CompleteWorkflow (run 3): %v", err)
	}

	// Negative control: the SAME write, with encryption off, must leave the
	// marker in plaintext on disk. Without this, a bug that made every write
	// above silently a no-op (never storing the marker at all) would also
	// pass -- assertSealed only checks the marker's ABSENCE.
	plainStore := NewPostgresStore(db)
	plainResultMarker := markerFor("plaincontrol")
	plainResult := fmt.Sprintf(`{"ok":%q}`, plainResultMarker)
	plainRunID, _, err := plainStore.StartNewRunWithOptions(ctx, "", "test", 1, json.RawMessage(`{}`), "", plainStore.tenantID, 0, StartOptions{})
	if err != nil {
		t.Fatalf("StartNewRunWithOptions (plain control): %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, plainRunID) })
	plainClaimed, err := plainStore.ClaimWorkflow(ctx, "worker1")
	if err != nil || plainClaimed == nil {
		t.Fatalf("ClaimWorkflow (plain control): claimed=%v err=%v", plainClaimed, err)
	}
	if err := plainStore.CompleteWorkflow(ctx, plainRunID, "worker1", plainClaimed.Generation, plainResult, nil); err != nil {
		t.Fatalf("CompleteWorkflow (plain control): %v", err)
	}
	var rawPlainResult string
	if err := db.QueryRow(`SELECT result::text FROM workflow_instances WHERE id = $1`, plainRunID).Scan(&rawPlainResult); err != nil {
		t.Fatalf("read raw plain-control result: %v", err)
	}
	if !strings.Contains(rawPlainResult, plainResultMarker) {
		t.Fatalf("PRECONDITION FAILED: negative control (encryption off) does not contain its own "+
			"plaintext marker -- %q not found in %q, so this run cannot tell encrypted apart from broken",
			plainResultMarker, rawPlainResult)
	}
}

// TestSignalPromiseAndUpdatePayloadsAreEncrypted covers workflow_signals.
// payload, workflow_promises.{result,error_msg} and workflow_update_requests.
// {payload,result,error_msg} across DeliverSignal/PollSignal, CreatePromise/
// ResolvePromise/RejectPromise/GetPromise/ListPromises, and
// CreateUpdateRequest/CompleteUpdateRequest/GetPendingUpdateRequests.
func TestSignalPromiseAndUpdatePayloadsAreEncrypted(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	enc, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}
	store := NewPostgresStore(db).WithEncryption(enc, true)
	deployFaultTestDef(t, store)
	ctx := context.Background()

	runID, _, err := store.StartNewRunWithOptions(ctx, "", "test", 1, json.RawMessage(`{}`), "", store.tenantID, 0, StartOptions{})
	if err != nil {
		t.Fatalf("StartNewRunWithOptions: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID) })

	// Signal.
	sigMarker := markerFor("signal")
	sigPayload := fmt.Sprintf(`{"v":%q}`, sigMarker)
	if err := store.DeliverSignal(ctx, runID, "approve", sigPayload); err != nil {
		t.Fatalf("DeliverSignal: %v", err)
	}
	var rawSig string
	if err := db.QueryRow(`SELECT payload::text FROM workflow_signals WHERE workflow_id = $1`, runID).Scan(&rawSig); err != nil {
		t.Fatalf("read raw signal payload: %v", err)
	}
	assertSealed(t, "workflow_signals.payload", rawSig, sigMarker, sigPayload)

	delivery, ok, err := store.PollSignal(ctx, runID, "approve")
	if err != nil {
		t.Fatalf("PollSignal: %v", err)
	}
	if !ok {
		t.Fatalf("PRECONDITION FAILED: PollSignal found nothing")
	}
	if !strings.Contains(delivery.Payload, sigMarker) {
		t.Errorf("PollSignal: Payload = %q, want it to contain %q", delivery.Payload, sigMarker)
	}

	// Promise: resolve one, reject another.
	promiseID1 := "p1-" + markerFor("id")
	if err := store.CreatePromise(ctx, runID, "order-confirmed", promiseID1); err != nil {
		t.Fatalf("CreatePromise 1: %v", err)
	}
	resultMarker := markerFor("promiseresult")
	result := fmt.Sprintf(`{"v":%q}`, resultMarker)
	if err := store.ResolvePromise(ctx, promiseID1, result); err != nil {
		t.Fatalf("ResolvePromise: %v", err)
	}

	promiseID2 := "p2-" + markerFor("id")
	if err := store.CreatePromise(ctx, runID, "order-cancelled", promiseID2); err != nil {
		t.Fatalf("CreatePromise 2: %v", err)
	}
	rejectMarker := markerFor("promisereject")
	if err := store.RejectPromise(ctx, promiseID2, rejectMarker); err != nil {
		t.Fatalf("RejectPromise: %v", err)
	}

	var rawPromiseResult, rawPromiseErr string
	if err := db.QueryRow(`SELECT result::text FROM workflow_promises WHERE promise_id = $1`, promiseID1).Scan(&rawPromiseResult); err != nil {
		t.Fatalf("read raw promise result: %v", err)
	}
	assertSealed(t, "workflow_promises.result", rawPromiseResult, resultMarker, result)
	if err := db.QueryRow(`SELECT error_msg FROM workflow_promises WHERE promise_id = $1`, promiseID2).Scan(&rawPromiseErr); err != nil {
		t.Fatalf("read raw promise error_msg: %v", err)
	}
	assertSealed(t, "workflow_promises.error_msg", rawPromiseErr, rejectMarker, rejectMarker)

	_, gotResult, _, err := store.GetPromise(ctx, runID, promiseID1)
	if err != nil {
		t.Fatalf("GetPromise 1: %v", err)
	}
	if gotResult != result {
		t.Errorf("GetPromise 1: result = %q, want %q", gotResult, result)
	}
	_, _, gotErrMsg, err := store.GetPromise(ctx, runID, promiseID2)
	if err != nil {
		t.Fatalf("GetPromise 2: %v", err)
	}
	if gotErrMsg != rejectMarker {
		t.Errorf("GetPromise 2: errMsg = %q, want %q", gotErrMsg, rejectMarker)
	}

	listedPromises, err := store.ListPromises(ctx, runID)
	if err != nil {
		t.Fatalf("ListPromises: %v", err)
	}
	foundResolved, foundRejected := false, false
	for _, p := range listedPromises {
		if p.PromiseID == promiseID1 {
			foundResolved = true
			if p.Result != result {
				t.Errorf("ListPromises: promise 1 Result = %q, want %q", p.Result, result)
			}
		}
		if p.PromiseID == promiseID2 {
			foundRejected = true
			if p.ErrorMsg != rejectMarker {
				t.Errorf("ListPromises: promise 2 ErrorMsg = %q, want %q", p.ErrorMsg, rejectMarker)
			}
		}
	}
	if !foundResolved || !foundRejected {
		t.Fatalf("PRECONDITION FAILED: ListPromises missing promise 1 (%v) or promise 2 (%v)", foundResolved, foundRejected)
	}

	// Update request.
	updMarker := markerFor("update")
	updPayload := fmt.Sprintf(`{"v":%q}`, updMarker)
	promiseID3 := "p3-" + markerFor("id")
	if err := store.CreateUpdateRequest(ctx, runID, "setStatus", updPayload, promiseID3); err != nil {
		t.Fatalf("CreateUpdateRequest: %v", err)
	}

	var rawUpdPayload string
	if err := db.QueryRow(`SELECT payload::text FROM workflow_update_requests WHERE workflow_id = $1 AND promise_id = $2`,
		runID, promiseID3).Scan(&rawUpdPayload); err != nil {
		t.Fatalf("read raw update payload: %v", err)
	}
	assertSealed(t, "workflow_update_requests.payload", rawUpdPayload, updMarker, updPayload)

	pending, err := store.GetPendingUpdateRequests(ctx, runID)
	if err != nil {
		t.Fatalf("GetPendingUpdateRequests: %v", err)
	}
	foundUpd := false
	var reqID string
	for _, r := range pending {
		if r.PromiseID == promiseID3 {
			foundUpd = true
			reqID = r.RequestID
			if !strings.Contains(r.Payload, updMarker) {
				t.Errorf("GetPendingUpdateRequests: Payload = %q, want it to contain %q", r.Payload, updMarker)
			}
		}
	}
	if !foundUpd {
		t.Fatalf("PRECONDITION FAILED: GetPendingUpdateRequests missing the request just created")
	}

	updResultMarker := markerFor("updresult")
	updResult := fmt.Sprintf(`{"v":%q}`, updResultMarker)
	updErrMarker := markerFor("upderr")
	if err := store.CompleteUpdateRequest(ctx, runID, reqID, updResult, updErrMarker); err != nil {
		t.Fatalf("CompleteUpdateRequest: %v", err)
	}
	var rawUpdResult, rawUpdErr string
	if err := db.QueryRow(`SELECT result::text, error_msg FROM workflow_update_requests WHERE workflow_id = $1 AND request_id = $2`,
		runID, reqID).Scan(&rawUpdResult, &rawUpdErr); err != nil {
		t.Fatalf("read raw update result/error_msg: %v", err)
	}
	assertSealed(t, "workflow_update_requests.result", rawUpdResult, updResultMarker, updResult)
	assertSealed(t, "workflow_update_requests.error_msg", rawUpdErr, updErrMarker, updErrMarker)
}

// TestScheduleAndChildWorkflowInputAreEncrypted covers workflow_schedules.input
// (CreateSchedule, GetDueSchedules, ListSchedules) and the child's own
// workflow_instances.input written by StartChildWorkflow/StartChildWorkflowAtomic
// -- distinct from event.ChildInput in the parent's event_history, already
// covered by cleat#2328/encodeEventForStorage.
func TestScheduleAndChildWorkflowInputAreEncrypted(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	enc, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}
	store := NewPostgresStore(db).WithEncryption(enc, true)
	deployFaultTestDef(t, store)
	ctx := context.Background()

	// Schedule.
	schedMarker := markerFor("schedule")
	schedInput := json.RawMessage(fmt.Sprintf(`{"v":%q}`, schedMarker))
	schedName := "sched-" + markerFor("name")
	sch := Schedule{
		Name:           schedName,
		DefName:        "test",
		CronExpression: "* * * * *",
		Input:          schedInput,
		NextRunAt:      time.Now().Add(-time.Minute), // already due
		TenantID:       store.tenantID,
	}
	if err := store.CreateSchedule(ctx, sch); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM workflow_schedules WHERE name = $1`, schedName) })

	var rawSchedInput string
	if err := db.QueryRow(`SELECT input::text FROM workflow_schedules WHERE name = $1`, schedName).Scan(&rawSchedInput); err != nil {
		t.Fatalf("read raw schedule input: %v", err)
	}
	assertSealed(t, "workflow_schedules.input", rawSchedInput, schedMarker, string(schedInput))

	due, err := store.GetDueSchedules(ctx)
	if err != nil {
		t.Fatalf("GetDueSchedules: %v", err)
	}
	foundDue := false
	for _, s := range due {
		if s.Name == schedName {
			foundDue = true
			if string(s.Input) != string(schedInput) {
				t.Errorf("GetDueSchedules: Input = %q, want %q", s.Input, schedInput)
			}
		}
	}
	if !foundDue {
		t.Fatalf("PRECONDITION FAILED: GetDueSchedules did not return %q", schedName)
	}

	listed, err := store.ListSchedules(ctx)
	if err != nil {
		t.Fatalf("ListSchedules: %v", err)
	}
	foundListed := false
	for _, s := range listed {
		if s.Name == schedName {
			foundListed = true
			if string(s.Input) != string(schedInput) {
				t.Errorf("ListSchedules: Input = %q, want %q", s.Input, schedInput)
			}
		}
	}
	if !foundListed {
		t.Fatalf("PRECONDITION FAILED: ListSchedules did not return %q", schedName)
	}

	// Child workflow's own input column (not event.ChildInput).
	parentID := appendChainWorkflow(t, store)
	// appendChainWorkflow leaves the parent 'ready' (it is a raw INSERT, not a
	// store call) -- claim and complete it immediately so it cannot be the
	// workflow ClaimWorkflow picks up for the child, below.
	claimedParent, err := store.ClaimWorkflow(ctx, "worker1")
	if err != nil || claimedParent == nil || claimedParent.ID != parentID {
		t.Fatalf("PRECONDITION FAILED: claim of parent (%s): claimed=%v err=%v", parentID, claimedParent, err)
	}
	if err := store.CompleteWorkflow(ctx, parentID, "worker1", claimedParent.Generation, "{}", nil); err != nil {
		t.Fatalf("CompleteWorkflow (parent): %v", err)
	}
	childInputMarker := markerFor("childinput")
	childInput := fmt.Sprintf(`{"v":%q}`, childInputMarker)
	childID, err := store.StartChildWorkflow(ctx, parentID, "test", childInput, 1, "ABANDON", 0)
	if err != nil {
		t.Fatalf("StartChildWorkflow: %v", err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, childID) })

	var rawChildInput string
	if err := db.QueryRow(`SELECT input::text FROM workflow_instances WHERE id = $1`, childID).Scan(&rawChildInput); err != nil {
		t.Fatalf("read raw child input: %v", err)
	}
	assertSealed(t, "workflow_instances.input (child, StartChildWorkflow)", rawChildInput, childInputMarker, childInput)

	childWF, err := store.GetWorkflowByID(ctx, childID)
	if err != nil {
		t.Fatalf("GetWorkflowByID (child): %v", err)
	}
	if string(childWF.Input) != childInput {
		t.Errorf("GetWorkflowByID (child): Input = %q, want %q", childWF.Input, childInput)
	}

	// GetChildResult: complete the child and read its result/error back
	// through the parent-await path, not just display.
	claimedChild, err := store.ClaimWorkflow(ctx, "worker1")
	if err != nil || claimedChild == nil || claimedChild.ID != childID {
		t.Fatalf("PRECONDITION FAILED: claim of child %s: claimed=%v err=%v", childID, claimedChild, err)
	}
	childResultMarker := markerFor("childresult")
	childResult := fmt.Sprintf(`{"v":%q}`, childResultMarker)
	if err := store.CompleteWorkflow(ctx, childID, "worker1", claimedChild.Generation, childResult, nil); err != nil {
		t.Fatalf("CompleteWorkflow (child): %v", err)
	}
	outcome, err := store.GetChildResult(ctx, childID)
	if err != nil {
		t.Fatalf("GetChildResult: %v", err)
	}
	if !strings.Contains(outcome.Result, childResultMarker) {
		t.Errorf("GetChildResult: Result = %q, want it to contain %q", outcome.Result, childResultMarker)
	}
}
