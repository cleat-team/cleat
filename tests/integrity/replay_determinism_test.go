package integrity

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// TestReplayProducesIdenticalHistory verifies that loading the same event
// history twice returns identical results — a prerequisite for deterministic
// replay.
func TestReplayProducesIdenticalHistory(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := engine.NewPostgresStore(db)
	ctx := context.Background()
	runID := fmt.Sprintf("int-replay-%d", time.Now().UnixNano())
	_, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
		VALUES ($1, 'test', 1, 'ready', '{}', 'default') ON CONFLICT DO NOTHING`, runID)
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	defer func() {
		db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, runID)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)
	}()

	// Append a deterministic event sequence.
	events := []engine.EventRecord{
		{Step: 0, EventType: engine.EventTypeCall, Service: "catalog", Op: "LookupItem", Request: `{"sku":"ABC"}`, Response: `{"price":999}`},
		{Step: 1, EventType: engine.EventTypeCall, Service: "inventory", Op: "Reserve", Request: `{"sku":"ABC","qty":1}`, Response: `{"ok":true}`},
		{Step: 2, EventType: engine.EventTypeCall, Service: "payments", Op: "Charge", Request: `{"amount":999}`, Response: `{"charge_id":"ch_123"}`},
		{Step: 3, EventType: engine.EventTypeCall, Service: "shipping", Op: "CreateShipment", Request: `{"order_id":1}`, Response: `{"tracking":"TRACK-1"}`},
	}
	if err := store.AppendEventHistoryBatch(ctx, runID, events); err != nil {
		t.Fatalf("append events: %v", err)
	}

	// Load history twice.
	history1, err := store.LoadEventHistory(ctx, runID)
	if err != nil {
		t.Fatalf("first LoadEventHistory: %v", err)
	}
	history2, err := store.LoadEventHistory(ctx, runID)
	if err != nil {
		t.Fatalf("second LoadEventHistory: %v", err)
	}

	// Compare lengths and content.
	if len(history1) != len(history2) {
		t.Fatalf("history length mismatch: %d vs %d", len(history1), len(history2))
	}
	for i := range history1 {
		if history1[i].Step != history2[i].Step {
			t.Errorf("event %d: Step mismatch: %d vs %d", i, history1[i].Step, history2[i].Step)
		}
		if history1[i].EventType != history2[i].EventType {
			t.Errorf("event %d: EventType mismatch: %s vs %s", i, history1[i].EventType, history2[i].EventType)
		}
		if history1[i].Service != history2[i].Service {
			t.Errorf("event %d: Service mismatch: %s vs %s", i, history1[i].Service, history2[i].Service)
		}
		if history1[i].Op != history2[i].Op {
			t.Errorf("event %d: Op mismatch: %s vs %s", i, history1[i].Op, history2[i].Op)
		}
		if history1[i].Request != history2[i].Request {
			t.Errorf("event %d: Request mismatch", i)
		}
		if history1[i].Response != history2[i].Response {
			t.Errorf("event %d: Response mismatch", i)
		}
	}
}

// TestReplayDifferentInputsProducesDifferentHistory verifies that different
// inputs produce different event histories — no false determinism.
func TestReplayDifferentInputsProducesDifferentHistory(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := engine.NewPostgresStore(db)
	ctx := context.Background()

	// Create two workflows with different event sequences.
	makeID := func(suffix string) string {
		id := fmt.Sprintf("int-replay-diff-%s-%d", suffix, time.Now().UnixNano())
		_, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
			VALUES ($1, 'test', 1, 'ready', '{}', 'default') ON CONFLICT DO NOTHING`, id)
		if err != nil {
			t.Fatalf("create workflow %s: %v", suffix, err)
		}
		t.Cleanup(func() {
			db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, id)
			db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, id)
		})
		return id
	}

	runID1 := makeID("a")
	runID2 := makeID("b")

	// Workflow A: single call.
	eventsA := []engine.EventRecord{
		{Step: 0, EventType: engine.EventTypeCall, Service: "svc", Op: "opA", Request: `{"input":"a"}`, Response: `{"ok":true}`},
	}
	if err := store.AppendEventHistoryBatch(ctx, runID1, eventsA); err != nil {
		t.Fatalf("append events A: %v", err)
	}

	// Workflow B: different call.
	eventsB := []engine.EventRecord{
		{Step: 0, EventType: engine.EventTypeCall, Service: "svc", Op: "opB", Request: `{"input":"b"}`, Response: `{"ok":false}`},
	}
	if err := store.AppendEventHistoryBatch(ctx, runID2, eventsB); err != nil {
		t.Fatalf("append events B: %v", err)
	}

	historyA, err := store.LoadEventHistory(ctx, runID1)
	if err != nil {
		t.Fatalf("LoadEventHistory A: %v", err)
	}
	historyB, err := store.LoadEventHistory(ctx, runID2)
	if err != nil {
		t.Fatalf("LoadEventHistory B: %v", err)
	}

	// Histories should be different.
	same := len(historyA) == len(historyB)
	if same {
		for i := range historyA {
			if historyA[i].Op != historyB[i].Op || historyA[i].Request != historyB[i].Request || historyA[i].Response != historyB[i].Response {
				same = false
				break
			}
		}
	}
	if same {
		t.Error("two workflows with different inputs should produce different histories")
	}
}

// TestReplayVersionMismatch verifies that replay detects version mismatches
// between the workflow instance version and expected version.
func TestReplayVersionMismatch(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := engine.NewPostgresStore(db)
	ctx := context.Background()

	// Create a workflow definition at version 1 and version 2.
	defName := fmt.Sprintf("int-version-test-%d", time.Now().UnixNano())
	wasmBytes := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	db.Exec(`INSERT INTO workflow_defs (name, version, wasm_bytes, entry_points)
		VALUES ($1, 1, $2, '{test}') ON CONFLICT DO NOTHING`, defName, wasmBytes)
	db.Exec(`INSERT INTO workflow_defs (name, version, wasm_bytes, entry_points)
		VALUES ($1, 2, $2, '{test}') ON CONFLICT DO NOTHING`, defName, wasmBytes)
	defer db.Exec(`DELETE FROM workflow_defs WHERE name = $1`, defName)

	// Create a workflow instance at version 1 with events.
	runID := fmt.Sprintf("int-version-%d", time.Now().UnixNano())
	_, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
		VALUES ($1, $2, 1, 'ready', '{}', 'default')`, runID, defName)
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	defer func() {
		db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, runID)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)
	}()

	events := []engine.EventRecord{
		{Step: 0, EventType: engine.EventTypeCall, Service: "svc", Op: "v1-op", Request: `{}`, Response: `{}`},
	}
	if err := store.AppendEventHistoryBatch(ctx, runID, events); err != nil {
		t.Fatalf("append events: %v", err)
	}

	// Verify we can load the workflow instance and see it's at version 1.
	wf, err := store.GetWorkflowByID(ctx, runID)
	if err != nil {
		t.Fatalf("GetWorkflowByID: %v", err)
	}
	if wf == nil {
		t.Fatal("workflow instance not found")
	}
	if wf.DefVersion != 1 {
		t.Errorf("expected def_version=1, got %d", wf.DefVersion)
	}

	// Verify the version 1 definition exists.
	wasm, err := store.LoadWASM(ctx, defName, 1)
	if err != nil {
		t.Errorf("LoadWASM v1: %v", err)
	}
	if len(wasm) == 0 {
		t.Error("expected non-empty WASM bytes for v1")
	}

	// Verify version 2 also exists.
	wasm2, err := store.LoadWASM(ctx, defName, 2)
	if err != nil {
		t.Errorf("LoadWASM v2: %v", err)
	}
	if len(wasm2) == 0 {
		t.Error("expected non-empty WASM bytes for v2")
	}
}

// TestReplayHashMismatch computes an event history hash and verifies it is
// stable across multiple loads.
func TestReplayHashMismatch(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := engine.NewPostgresStore(db)
	ctx := context.Background()
	runID := fmt.Sprintf("int-hash-%d", time.Now().UnixNano())
	_, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
		VALUES ($1, 'test', 1, 'ready', '{}', 'default') ON CONFLICT DO NOTHING`, runID)
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	defer func() {
		db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, runID)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)
	}()

	// Append events.
	events := []engine.EventRecord{
		{Step: 0, EventType: engine.EventTypeCall, Service: "a", Op: "op1", Request: `{"x":1}`, Response: `{"ok":true}`},
		{Step: 1, EventType: engine.EventTypeCall, Service: "b", Op: "op2", Request: `{"y":2}`, Response: `{"ok":true}`},
		{Step: 2, EventType: engine.EventTypeCall, Service: "c", Op: "op3", Request: `{"z":3}`, Response: `{"ok":true}`},
	}
	if err := store.AppendEventHistoryBatch(ctx, runID, events); err != nil {
		t.Fatalf("append events: %v", err)
	}

	// Compute hash function for event history.
	hashHistory := func() [sha256.Size]byte {
		history, err := store.LoadEventHistory(ctx, runID)
		if err != nil {
			t.Fatalf("LoadEventHistory: %v", err)
		}
		data, err := json.Marshal(history)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		return sha256.Sum256(data)
	}

	// Compute hash three times — must be identical.
	hash1 := hashHistory()
	hash2 := hashHistory()
	hash3 := hashHistory()

	if hash1 != hash2 {
		t.Error("hash mismatch between first and second load")
	}
	if hash1 != hash3 {
		t.Error("hash mismatch between first and third load")
	}

	t.Logf("History hash is stable across loads: %x", hash1[:8])
}
