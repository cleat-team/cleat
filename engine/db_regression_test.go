package engine

import (
	"database/sql/driver"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// ContinueAsNew tests
// ---------------------------------------------------------------------------

func TestPostgresStore_ContinueAsNew_Success(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "INSERT INTO workflow_instances (id, def_name",
			data:  [][]driver.Value{{"new-run-uuid"}},
		},
	}, []mockExecResult{
		// "Complete old run" UPDATE must report the fence as held for
		// ContinueAsNew to commit and return the new run ID.
		{match: "SET status = 'done'", affected: 1},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	events := []EventRecord{
		{Step: 0, EventType: "call", Service: "svc", Op: "op"},
	}
	newID, err := store.ContinueAsNew(testCtx, "old-run", "worker-1", 0, "test-wf", 1,
		json.RawMessage(`{}`), events, `{"ok":true}`, map[string]string{"k": "v"}, 0)
	if err != nil {
		t.Fatalf("ContinueAsNew: %v", err)
	}
	if newID != "new-run-uuid" {
		t.Errorf("expected 'new-run-uuid', got %q", newID)
	}
}

func TestPostgresStore_ContinueAsNew_NoEvents(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "INSERT INTO workflow_instances (id, def_name",
			data:  [][]driver.Value{{"new-run-uuid"}},
		},
	}, []mockExecResult{
		// "Complete old run" UPDATE must report the fence as held for
		// ContinueAsNew to commit and return the new run ID.
		{match: "SET status = 'done'", affected: 1},
	})
	defer db.Close()

	store := NewPostgresStore(db)
	newID, err := store.ContinueAsNew(testCtx, "old-run", "worker-1", 0, "test-wf", 1,
		json.RawMessage(`{}`), nil, `{"ok":true}`, nil, 0)
	if err != nil {
		t.Fatalf("ContinueAsNew (no events): %v", err)
	}
	if newID != "new-run-uuid" {
		t.Errorf("expected 'new-run-uuid', got %q", newID)
	}
}

// ---------------------------------------------------------------------------
// FinalizeWorkflowSegment tests
// ---------------------------------------------------------------------------

func TestPostgresStore_FinalizeWorkflowSegment_Done(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		// finalize_workflow_status is called via QueryRow and returns
		// whether the generation fence held.
		{match: "SELECT finalize_workflow_status", data: [][]driver.Value{{true}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.FinalizeWorkflowSegment(testCtx, "wf-1", "worker-1", 0,
		[]EventRecord{{Step: 0, EventType: "call", Service: "svc", Op: "op"}},
		"done", `{"result":"ok"}`, "", "", map[string]string{"k": "v"}, time.Time{})
	if err != nil {
		t.Fatalf("FinalizeWorkflowSegment (done): %v", err)
	}
}

func TestPostgresStore_FinalizeWorkflowSegment_Failed(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT finalize_workflow_status", data: [][]driver.Value{{true}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.FinalizeWorkflowSegment(testCtx, "wf-1", "worker-1", 0,
		[]EventRecord{{Step: 0, EventType: "call", Service: "svc", Op: "op"}},
		"failed", "something broke", "E_TEST", "test_op", map[string]string{}, time.Time{})
	if err != nil {
		t.Fatalf("FinalizeWorkflowSegment (failed): %v", err)
	}
}

func TestPostgresStore_FinalizeWorkflowSegment_Ready(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT finalize_workflow_status", data: [][]driver.Value{{true}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	nextWake := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	err := store.FinalizeWorkflowSegment(testCtx, "wf-1", "worker-1", 0,
		[]EventRecord{{Step: 0, EventType: "sleep", DurationMs: 5000}},
		"ready", "", "", "", nil, nextWake)
	if err != nil {
		t.Fatalf("FinalizeWorkflowSegment (ready): %v", err)
	}
}

func TestPostgresStore_FinalizeWorkflowSegment_UnknownStatus(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.FinalizeWorkflowSegment(testCtx, "wf-1", "worker-1", 0, nil, "invalid", "", "", "", nil, time.Time{})
	if err == nil {
		t.Fatal("expected error for unknown final status")
	}
	if !strings.Contains(err.Error(), "unknown final status") {
		t.Errorf("expected 'unknown final status' error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// StartChildWorkflowAtomic tests
// ---------------------------------------------------------------------------

func TestPostgresStore_StartChildWorkflowAtomic_ExplicitVersion(t *testing.T) {
	childID := "child-uuuid-123"
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "MAX(version)", data: [][]driver.Value{{int64(-1)}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	event := EventRecord{Step: 0, EventType: "child_workflow", ChildName: "child-wf", ChildInput: `{}`, TimestampMs: 1000}
	id, err := store.StartChildWorkflowAtomic(testCtx, childID, "parent-1", "child-wf", `{"x":1}`, 1, "ABANDON", event, 0)
	if err != nil {
		t.Fatalf("StartChildWorkflowAtomic: %v", err)
	}
	if id != childID {
		t.Errorf("expected %q, got %q", childID, id)
	}
}

func TestPostgresStore_StartChildWorkflowAtomic_ResolveVersion(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "MAX(version)", data: [][]driver.Value{{int64(3)}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	event := EventRecord{Step: 0, EventType: "child_workflow", ChildName: "child-wf", ChildInput: `{}`}
	id, err := store.StartChildWorkflowAtomic(testCtx, "", "parent-1", "child-wf", `{"x":1}`, 0, "TERMINATE", event, 0)
	if err != nil {
		t.Fatalf("StartChildWorkflowAtomic (resolve): %v", err)
	}
	if id == "" {
		t.Error("expected non-empty generated child ID")
	}
}

func TestPostgresStore_StartChildWorkflowAtomic_WithChecksumChain(t *testing.T) {
	prevCS := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "MAX(version)", data: [][]driver.Value{{int64(2)}}},
		{match: "COALESCE(checksum", data: [][]driver.Value{{prevCS}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	event := EventRecord{
		Step:       2,
		EventType:  "child_workflow",
		ChildName:  "child-wf",
		ChildInput: `{}`,
	}
	id, err := store.StartChildWorkflowAtomic(testCtx, "child-1", "parent-1", "child-wf", `{}`, 0, "ABANDON", event, 0)
	if err != nil {
		t.Fatalf("StartChildWorkflowAtomic (checksum chain): %v", err)
	}
	if id != "child-1" {
		t.Errorf("expected 'child-1', got %q", id)
	}
}

// ---------------------------------------------------------------------------
// LoadEventHistoryPaginated tests
// ---------------------------------------------------------------------------

// loadHistoryRow builds a 29-column mock row for LoadEventHistoryPaginated.
// Columns: step, event_type, service, operation, request, response, error,
//
//	duration_ms, signal_names, timeout_ms, signal_name, signal_payload,
//	defer_description, defer_id, child_name, child_input, run_id, new_input,
//	plugin_name, plugin_func, plugin_input, plugin_output, plugin_error,
//	payload, promise_name, promise_id, promise_result, promise_error, created_at
func loadHistoryRow(step int, eventType string) []driver.Value {
	row := make([]driver.Value, 29)
	row[0] = int64(step)
	row[1] = eventType
	// All other columns are nil (NULL) by default in Go
	return row
}

func loadHistoryRowWithPayload(step int, eventType string, extra map[int]driver.Value) []driver.Value {
	row := loadHistoryRow(step, eventType)
	for idx, val := range extra {
		row[idx] = val
	}
	return row
}

func TestPostgresStore_LoadEventHistoryPaginated_Empty(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db).WithReadRedactionDisabled(true)
	history, err := store.LoadEventHistoryPaginated(testCtx, "wf-1", 0, 100)
	if err != nil {
		t.Fatalf("LoadEventHistoryPaginated: %v", err)
	}
	if len(history) != 0 {
		t.Errorf("expected empty history, got %d events", len(history))
	}
}

func TestPostgresStore_LoadEventHistoryPaginated_FirstPage(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "FROM event_history",
			data: [][]driver.Value{
				loadHistoryRowWithPayload(0, "call", map[int]driver.Value{
					2: "svc", 3: "op", 4: "{}", 5: "{}",
				}),
				loadHistoryRowWithPayload(1, "sleep", map[int]driver.Value{
					7: int64(5000),
				}),
			},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db).WithReadRedactionDisabled(true)
	history, err := store.LoadEventHistoryPaginated(testCtx, "wf-1", 0, 2)
	if err != nil {
		t.Fatalf("LoadEventHistoryPaginated: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("expected 2 events, got %d", len(history))
	}
	if history[0].Step != 0 || history[0].EventType != "call" {
		t.Errorf("unexpected first event: step=%d type=%s", history[0].Step, history[0].EventType)
	}
	if history[0].Service != "svc" || history[0].Op != "op" {
		t.Errorf("unexpected first event fields: svc=%s op=%s", history[0].Service, history[0].Op)
	}
	if history[1].Step != 1 || history[1].EventType != "sleep" || history[1].DurationMs != 5000 {
		t.Errorf("unexpected second event: step=%d type=%s duration=%d", history[1].Step, history[1].EventType, history[1].DurationMs)
	}
}

func TestPostgresStore_LoadEventHistoryPaginated_SecondPage(t *testing.T) {
	// The mock SQL driver doesn't implement OFFSET/LIMIT, so we configure
	// only the rows that would be returned if the DB handled pagination.
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "FROM event_history",
			data: [][]driver.Value{
				// Only step 1 is returned when OFFSET=1, LIMIT=1.
				loadHistoryRow(1, "sleep"),
			},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db).WithReadRedactionDisabled(true)
	history, err := store.LoadEventHistoryPaginated(testCtx, "wf-1", 1, 1)
	if err != nil {
		t.Fatalf("LoadEventHistoryPaginated: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("expected 1 event after pagination, got %d", len(history))
	}
	if history[0].Step != 1 || history[0].EventType != "sleep" {
		t.Errorf("expected step=1 sleep, got step=%d type=%s", history[0].Step, history[0].EventType)
	}
}

// ---------------------------------------------------------------------------
// VerifyWorkflowEvents tests
// ---------------------------------------------------------------------------

// fullHistoryRow builds a 31-column mock row for LoadEventHistory (called by VerifyWorkflowEvents).
// Column 28 is timestamp_ms (int64, scanned directly into rec.TimestampMs — must be non-nil).
// Column 29 is created_at (scanned into sql.NullTime — nil is fine for "invalid").
// Column 30 is pending, the intent_at IS NOT NULL AND checksum IS NULL expression
// (bool, scanned directly into rec.Pending — must be non-nil). See 1.4 phase D.
func fullHistoryRow(step int, eventType string) []driver.Value {
	row := make([]driver.Value, 31)
	row[0] = int64(step)
	row[1] = eventType
	row[28] = int64(0) // timestamp_ms — must be non-nil (scanned into int64)
	row[30] = false    // pending — must be non-nil (scanned into bool)
	return row
}

// shadowHistoryRow builds a row for the 17-column shadow-column query in
// verifyShadowColumns: step, event_type, service, operation, duration_ms,
// signal_names, timeout_ms, signal_name, defer_description, defer_id,
// child_name, run_id, plugin_name, plugin_func, promise_name, promise_id,
// payload.
func shadowHistoryRow(step int, eventType, service, op, payload string) []driver.Value {
	row := make([]driver.Value, 17)
	row[0] = int64(step)
	row[1] = eventType
	row[2] = service
	row[3] = op
	row[16] = payload
	return row
}

func TestPostgresStore_VerifyWorkflowEvents_EmptyHistory(t *testing.T) {
	db := newNoopDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.VerifyWorkflowEvents(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("VerifyWorkflowEvents (empty history): %v", err)
	}
}

func TestPostgresStore_VerifyWorkflowEvents_NoChecksums(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "operation",
			data:  [][]driver.Value{fullHistoryRow(0, "call")},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.VerifyWorkflowEvents(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("VerifyWorkflowEvents (no checksums): %v", err)
	}
}

func TestPostgresStore_VerifyWorkflowEvents_Valid(t *testing.T) {
	expectedCS := computeEventChecksum(EventRecord{Step: 0, EventType: "call"}, "")
	db := newMockDBForPostgres(t, []mockRowsResult{
		// First, and matched on a substring unique to it: VerifyWorkflowEvents
		// also queries the shadow columns now (see store_event_shadow.go), and
		// that query contains "operation" too. Without a rule of its own it
		// falls through to the LoadEventHistory rule below and is handed
		// 30-column rows for a 17-column scan.
		//
		// The row is deliberately self-consistent -- columns agreeing with
		// payload -- so this still exercises the comparison rather than
		// skipping it.
		{
			match: "promise_id, payload",
			data:  [][]driver.Value{shadowHistoryRow(0, "call", "svc", "op", `{"service":"svc","operation":"op"}`)},
		},
		{
			match: "operation",
			data:  [][]driver.Value{fullHistoryRow(0, "call")},
		},
		{
			match: "checksum",
			data:  [][]driver.Value{{int64(0), expectedCS}},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.VerifyWorkflowEvents(testCtx, "wf-1")
	if err != nil {
		t.Fatalf("VerifyWorkflowEvents (valid): %v", err)
	}
}

func TestPostgresStore_VerifyWorkflowEvents_Mismatch(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{
			match: "operation",
			data: [][]driver.Value{
				fullHistoryRow(0, "call"),
				fullHistoryRow(1, "sleep"),
			},
		},
		{
			match: "checksum",
			data: [][]driver.Value{
				{int64(0), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
				{int64(1), "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
			},
		},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	err := store.VerifyWorkflowEvents(testCtx, "wf-1")
	if err == nil {
		t.Fatal("expected checksum mismatch error")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("expected 'checksum mismatch' error, got: %v", err)
	}
}
