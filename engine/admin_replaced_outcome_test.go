package engine

import (
	"strings"
	"testing"
)

// TestAnAdminEventWithoutAReplacedOutcomeHashesUnchanged is the constraint the
// whole design turns on, and it is not a style preference.
//
// computeEventChecksum hashes the event PAYLOAD. Every admin_action event ever
// written -- every force-complete, every force-fail, every re-replay of a run
// that had no outcome to lose -- was hashed without a replaced_outcome key. An
// UNCONDITIONAL key would give all of them a different checksum, and
// VerifyWorkflowEvents would report every existing history as corrupt.
//
// So the field must be absent, not empty, when there is nothing to record.
func TestAnAdminEventWithoutAReplacedOutcomeHashesUnchanged(t *testing.T) {
	// Built through EventRecordFromEvent, which is where the "did this erase
	// anything" decision lives -- asserting on a hand-built EventRecord would
	// bypass the gate and test a state the real path cannot produce.
	ev := AdminActionEvent{step: 3, Action: "re_replay",
		Operator: "operator@example.com", Reason: "transient, retrying"}

	withNil := EventRecordFromEvent(ev)

	// A run that was merely stopped: a status, and nothing erased.
	ev.Replaced = &AdminReplacedOutcome{Status: "stopped"}
	withEmpty := EventRecordFromEvent(ev)

	a := computeEventChecksum(withNil, "prev")
	b := computeEventChecksum(withEmpty, "prev")
	if a != b {
		t.Errorf("an empty ReplacedOutcome changed the checksum: %s vs %s.\n\n"+
			"Every admin_action event already in every history was hashed "+
			"without this key. If an empty one emits it, VerifyWorkflowEvents "+
			"reports all of them corrupt.", a, b)
	}

	payload, err := eventRecordToPayload(withEmpty)
	if err != nil {
		t.Fatalf("eventRecordToPayload: %v", err)
	}
	if strings.Contains(string(payload), "replaced_") {
		t.Errorf("a status with nothing erased emitted a payload key: %s", payload)
	}
}

// TestAReplacedOutcomeChangesTheChecksum is the other half: when there IS
// something to record, it must be inside the hash. An audit field outside the
// checksum is editable in the columns afterwards with VerifyWorkflowEvents
// still calling the workflow clean -- which is the reason this is written into
// the history rather than beside it.
func TestAReplacedOutcomeChangesTheChecksum(t *testing.T) {
	base := EventRecord{Step: 3, EventType: EventTypeAdminAction,
		Service: "operator@example.com", Op: "re_replay"}
	with := base
	with.ReplacedStatus, with.ReplacedErrorMsg, with.ReplacedErrorCode = "failed", "connection refused", "transient"
	if computeEventChecksum(base, "") == computeEventChecksum(with, "") {
		t.Error("the replaced outcome is outside the checksum, so it can be " +
			"edited in the columns afterwards and still verify clean")
	}
}

// TestTheReplacedOutcomeRoundTrips: what the re-replay captured must be
// readable again, or the audit records it and no one can get it back.
func TestTheReplacedOutcomeRoundTrips(t *testing.T) {
	want := &AdminReplacedOutcome{
		Status:      "failed",
		ErrorMsg:    "pq: connection refused",
		ErrorCode:   "transient",
		ErrorOp:     "http_call",
		CompletedAt: "2026-09-10T14:03:02.5Z",
	}
	rec := EventRecord{Step: 7, EventType: EventTypeAdminAction,
		Service: "op", Op: "re_replay",
		ReplacedStatus: want.Status, ReplacedErrorMsg: want.ErrorMsg,
		ReplacedErrorCode: want.ErrorCode, ReplacedErrorOp: want.ErrorOp,
		ReplacedCompletedAt: want.CompletedAt}

	payload, err := eventRecordToPayload(rec)
	if err != nil {
		t.Fatalf("eventRecordToPayload: %v", err)
	}
	got := EventRecord{EventType: EventTypeAdminAction}
	populateFromPayload(&got, payload)
	gotOutcome := AdminReplacedOutcome{
		Status: got.ReplacedStatus, ErrorMsg: got.ReplacedErrorMsg,
		ErrorCode: got.ReplacedErrorCode, ErrorOp: got.ReplacedErrorOp,
		CompletedAt: got.ReplacedCompletedAt}
	if gotOutcome != *want {
		t.Errorf("round trip changed it:\n got %+v\nwant %+v\npayload: %s", gotOutcome, *want, payload)
	}
}

// TestAPartialOutcomeOmitsWhatItDoesNotHave -- a run that failed with a message
// but no code should not gain an empty error_code in its audit record.
func TestAPartialOutcomeOmitsWhatItDoesNotHave(t *testing.T) {
	rec := EventRecord{Step: 1, EventType: EventTypeAdminAction, Op: "re_replay",
		ReplacedStatus: "failed", ReplacedErrorMsg: "boom"}
	payload, err := eventRecordToPayload(rec)
	if err != nil {
		t.Fatalf("eventRecordToPayload: %v", err)
	}
	s := string(payload)
	if !strings.Contains(s, "replaced_error_msg") {
		t.Errorf("the message it did have is missing: %s", s)
	}
	for _, absent := range []string{"replaced_error_code", "replaced_error_op", "replaced_completed_at"} {
		if strings.Contains(s, absent) {
			t.Errorf("%q was emitted for a value the row did not carry: %s", absent, s)
		}
	}
}

// TestIsEmptyTreatsStatusAloneAsNothingToRecord.
//
// Status is always present -- every row has one -- so if it counted, EVERY
// re-replay would emit a key and the checksum-stability property above would be
// lost for the ordinary case. The question the field answers is "what outcome
// did this erase", and a run with no error and no completion erased nothing.
func TestIsEmptyTreatsStatusAloneAsNothingToRecord(t *testing.T) {
	if !(&AdminReplacedOutcome{Status: "stopped"}).IsEmpty() {
		t.Error("a status with no error and no completion is nothing to record; " +
			"counting it would make every re-replay emit a payload key")
	}
	if (&AdminReplacedOutcome{Status: "failed", ErrorMsg: "x"}).IsEmpty() {
		t.Error("an outcome with an error message is something to record")
	}
	var nilOutcome *AdminReplacedOutcome
	if !nilOutcome.IsEmpty() {
		t.Error("a nil outcome must be empty, not a panic")
	}
}

// TestEveryFieldSurvivesTheConversionToAnEventRecord.
//
// Added because a falsification found the gap: deleting one assignment from
// EventRecordFromEvent's admin arm left every test above still passing. They
// set the EventRecord fields directly, so the CONVERSION -- the step that
// actually runs in production -- was covered by nothing.
//
// A field dropped there is silent: the audit event is written, it is
// well-formed, and one of the five things it exists to record is simply absent.
func TestEveryFieldSurvivesTheConversionToAnEventRecord(t *testing.T) {
	want := AdminReplacedOutcome{
		Status:      "failed",
		ErrorMsg:    "pq: connection refused",
		ErrorCode:   "transient",
		ErrorOp:     "http_call",
		CompletedAt: "2026-09-10T14:03:02.5Z",
	}
	rec := EventRecordFromEvent(AdminActionEvent{
		step: 2, Action: "re_replay", Operator: "op", Replaced: &want,
	})
	got := AdminReplacedOutcome{
		Status: rec.ReplacedStatus, ErrorMsg: rec.ReplacedErrorMsg,
		ErrorCode: rec.ReplacedErrorCode, ErrorOp: rec.ReplacedErrorOp,
		CompletedAt: rec.ReplacedCompletedAt,
	}
	if got != want {
		t.Errorf("the conversion dropped or changed a field:\n got %+v\nwant %+v", got, want)
	}
}
