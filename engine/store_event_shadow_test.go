package engine

import (
	"context"
	"strings"
	"testing"
)

// event_history keeps each event in two places -- the individual columns and
// the payload JSONB -- and only payload is checksummed. These tests drive a
// real database because the whole point is what the *stored* row looks like
// after a targeted UPDATE; sqlmock would supply both sides of the comparison
// and could never disagree with itself.

// TestVerifyWorkflowEvents_DetectsShadowColumnTampering is the case named in
// IMPROVEMENT-PLAN 2.32. Before verifyShadowColumns this UPDATE was completely
// invisible: the checksum covers payload, replay reads payload, and the
// dashboard reads the column.
func TestVerifyWorkflowEvents_DetectsShadowColumnTampering(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	ctx := context.Background()
	runID := appendChainWorkflow(t, store)

	if err := store.AppendEventHistoryBatch(ctx, runID, []EventRecord{
		chainEvent(0, "start"),
		chainEvent(1, "process"),
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := store.VerifyWorkflowEvents(ctx, runID); err != nil {
		t.Fatalf("verify before tampering: %v", err)
	}

	if _, err := db.Exec(`UPDATE event_history SET operation = 'tampered'
		WHERE workflow_id = $1 AND step = 1`, runID); err != nil {
		t.Fatalf("tamper operation column: %v", err)
	}

	err := store.VerifyWorkflowEvents(ctx, runID)
	if err == nil {
		t.Fatal("VerifyWorkflowEvents accepted an event whose operation column no longer matches its payload")
	}
	for _, want := range []string{"step 1", "operation", "tampered", "process"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q; it has to name the row and both values to be actionable", err, want)
		}
	}
}

// TestVerifyWorkflowEvents_ShadowCheckIsNotVacuous guards the shape this whole
// session kept finding: a check that passes because it compares nothing.
// verifyShadowColumns can only see a divergence on keys the payload carries,
// so if the writer ever stopped populating `operation` the check above would
// still pass and would silently be testing nothing.
func TestVerifyWorkflowEvents_ShadowCheckIsNotVacuous(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	ctx := context.Background()
	runID := appendChainWorkflow(t, store)

	if err := store.AppendEventHistoryBatch(ctx, runID, []EventRecord{chainEvent(0, "start")}); err != nil {
		t.Fatalf("append: %v", err)
	}

	var payload string
	if err := db.QueryRow(`SELECT payload::text FROM event_history WHERE workflow_id = $1 AND step = 0`,
		runID).Scan(&payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	for _, want := range []string{`"operation"`, `"service"`} {
		if !strings.Contains(payload, want) {
			t.Fatalf("payload %s carries no %s key, so the shadow-column check has nothing to compare "+
				"and TestVerifyWorkflowEvents_DetectsShadowColumnTampering is passing vacuously", payload, want)
		}
	}
}

// TestVerifyWorkflowEvents_IgnoresRowsWithoutPayload pins the deliberate
// exemption. Rows written before the payload column existed have nothing
// authoritative to compare against, and the checksum arm skips them for the
// same reason -- so verification must not start reporting the entire
// pre-migration history as corrupt.
func TestVerifyWorkflowEvents_IgnoresRowsWithoutPayload(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	ctx := context.Background()
	runID := appendChainWorkflow(t, store)

	if err := store.AppendEventHistoryBatch(ctx, runID, []EventRecord{chainEvent(0, "start")}); err != nil {
		t.Fatalf("append: %v", err)
	}
	// A genuine pre-migration row has neither a payload nor a checksum. Both
	// have to go: nulling only the payload changes what the checksum is
	// computed over, so the chain arm fires first and the row never reaches
	// the shadow-column arm this test is about.
	if _, err := db.Exec(`UPDATE event_history SET payload = NULL, checksum = NULL, operation = 'whatever'
		WHERE workflow_id = $1 AND step = 0`, runID); err != nil {
		t.Fatalf("null payload and checksum: %v", err)
	}

	if err := store.VerifyWorkflowEvents(ctx, runID); err != nil {
		t.Errorf("verification rejected a row with no payload to compare against: %v", err)
	}
}

// TestVerifyWorkflowEvents_ShadowCheckSurvivesCleanHistory is the false-positive
// guard. Every mirrored column has to round-trip through eventRecordToPayload
// and populateFromPayload identically, or verification starts failing on
// untampered data -- which would be worse than the gap it closes.
func TestVerifyWorkflowEvents_ShadowCheckSurvivesCleanHistory(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	ctx := context.Background()
	runID := appendChainWorkflow(t, store)

	// One event of each shape that carries mirrored metadata.
	events := []EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "start", Request: `{}`, Response: `{}`, DurationMs: 12},
		{Step: 1, EventType: EventTypeAwaitSignals, SignalNames: "go,stop", TimeoutMs: 5000},
		{Step: 2, EventType: EventTypeSignalReceived, SignalName: "go", SignalPayload: `{"v":1}`},
		{Step: 3, EventType: EventTypeChildWorkflow, ChildName: "child", ChildInput: `{}`, RunID: "run-1"},
		{Step: 4, EventType: EventTypePluginCall, PluginName: "slack-notify", PluginFunc: "send_message"},
		{Step: 5, EventType: EventTypeCreatePromise, PromiseName: "p1", PromiseID: "id-1"},
		{Step: 6, EventType: EventTypeDefer, DeferDescription: "cleanup", DeferID: "d-1"},
	}
	if err := store.AppendEventHistoryBatch(ctx, runID, events); err != nil {
		t.Fatalf("append: %v", err)
	}

	if err := store.VerifyWorkflowEvents(ctx, runID); err != nil {
		t.Errorf("verification rejected an untampered history: %v", err)
	}
}
