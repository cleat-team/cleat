package integrity

import (
	"context"
	"fmt"
	"testing"
	"time"

	host "github.com/cleat-team/cleat/engine"
)

// TestCompactionReducesEventCount verifies that compacting a workflow's event
// history reduces the number of stored events.
func TestCompactionReducesEventCount(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := engine.NewPostgresStore(db)
	ctx := context.Background()
	runID := fmt.Sprintf("int-compact-count-%d", time.Now().UnixNano())
	_, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
		VALUES ($1, 'test', 1, 'ready', '{}', 'default') ON CONFLICT DO NOTHING`, runID)
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	defer func() {
		db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, runID)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)
	}()

	// Insert enough events to trigger compaction.
	// Using threshold=5 means 5/2=2 events are kept when len(events) > 5.
	// With 20 events: compaction deletes events before step 18, keeping 2.
	const numEvents = 20
	const threshold = 5

	var events []engine.EventRecord
	for i := 0; i < numEvents; i++ {
		events = append(events, engine.EventRecord{
			Step:     i,
			EventType: engine.EventTypeCall,
			Service:  "svc",
			Op:       fmt.Sprintf("op-%d", i),
			Request:  `{}`,
			Response: `{"ok":true}`,
		})
	}
	if err := store.AppendEventHistoryBatch(ctx, runID, events); err != nil {
		t.Fatalf("append events: %v", err)
	}

	// Verify initial event count.
	initial, err := store.LoadEventHistory(ctx, runID)
	if err != nil {
		t.Fatalf("initial LoadEventHistory: %v", err)
	}
	if len(initial) != numEvents {
		t.Fatalf("expected %d events initially, got %d", numEvents, len(initial))
	}

	// Compact the history.
	if err := engine.CompactWorkflowHistory(ctx, store, runID, threshold); err != nil {
		t.Fatalf("CompactWorkflowHistory: %v", err)
	}

	// After compaction, event count should be reduced.
	after, err := store.LoadEventHistory(ctx, runID)
	if err != nil {
		t.Fatalf("post-compaction LoadEventHistory: %v", err)
	}

	// With 20 events and threshold=5:
	// keepStep = 20 - 5/2 = 20 - 2 = 18. Events [0:18] are deleted, [18:20] remain.
	expectedRemaining := numEvents - (numEvents - threshold/2)
	if len(after) != expectedRemaining {
		t.Errorf("expected %d events after compaction, got %d", expectedRemaining, len(after))
	}

	// Verify the compaction state was stored.
	cs, err := store.LoadCompactionState(ctx, runID)
	if err != nil {
		t.Fatalf("LoadCompactionState: %v", err)
	}
	if cs == nil {
		t.Fatal("expected non-nil compaction state after compaction")
	}
	if cs.CompactedStep <= 0 {
		t.Errorf("expected compaction_step > 0, got %d", cs.CompactedStep)
	}

	t.Logf("Compaction reduced events from %d to %d (compacted_step=%d, state_events=%d)",
		numEvents, len(after), cs.CompactedStep, len(cs.Events))
}

// TestCompactionPreservesState verifies that after compaction, the remaining
// events match the tail of the original history.
func TestCompactionPreservesState(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := engine.NewPostgresStore(db)
	ctx := context.Background()
	runID := fmt.Sprintf("int-compact-preserve-%d", time.Now().UnixNano())
	_, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
		VALUES ($1, 'test', 1, 'ready', '{}', 'default') ON CONFLICT DO NOTHING`, runID)
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	defer func() {
		db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, runID)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)
	}()

	// Insert events with varied content.
	const numEvents = 15
	const threshold = 5

	var events []engine.EventRecord
	for i := 0; i < numEvents; i++ {
		events = append(events, engine.EventRecord{
			Step:      i,
			EventType: engine.EventTypeCall,
			Service:   "svc",
			Op:        fmt.Sprintf("op-%d", i),
			Request:   fmt.Sprintf(`{"step":%d}`, i),
			Response:  `{"ok":true}`,
		})
	}
	if err := store.AppendEventHistoryBatch(ctx, runID, events); err != nil {
		t.Fatalf("append events: %v", err)
	}

	// Record original history.
	original, err := store.LoadEventHistory(ctx, runID)
	if err != nil {
		t.Fatalf("initial LoadEventHistory: %v", err)
	}

	// Compact.
	if err := engine.CompactWorkflowHistory(ctx, store, runID, threshold); err != nil {
		t.Fatalf("CompactWorkflowHistory: %v", err)
	}

	// Load remaining events.
	remaining, err := store.LoadEventHistory(ctx, runID)
	if err != nil {
		t.Fatalf("post-compaction LoadEventHistory: %v", err)
	}

	// The remaining events should match the tail of the original history.
	// keepStep = numEvents - threshold/2 = 15 - 2 = 13.
	// Events [13:15] remain; events [0:12] are compacted.
	expectedRemaining := numEvents - (numEvents - threshold/2)
	if len(remaining) != expectedRemaining {
		t.Fatalf("expected %d remaining events, got %d", expectedRemaining, len(remaining))
	}

	// Verify each remaining event matches the corresponding original event.
	origTail := original[numEvents-expectedRemaining:]
	for i := range remaining {
		if remaining[i].Step != origTail[i].Step {
			t.Errorf("remaining[%d] Step: expected %d, got %d", i, origTail[i].Step, remaining[i].Step)
		}
		if remaining[i].Service != origTail[i].Service {
			t.Errorf("remaining[%d] Service: expected %s, got %s", i, origTail[i].Service, remaining[i].Service)
		}
		if remaining[i].Op != origTail[i].Op {
			t.Errorf("remaining[%d] Op: expected %s, got %s", i, origTail[i].Op, remaining[i].Op)
		}
		if remaining[i].Request != origTail[i].Request {
			t.Errorf("remaining[%d] Request: expected %s, got %s", i, origTail[i].Request, remaining[i].Request)
		}
	}
}

// TestCompactionIdempotent verifies running compaction twice produces the same
// result — the second compaction should be a no-op.
func TestCompactionIdempotent(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := engine.NewPostgresStore(db)
	ctx := context.Background()
	runID := fmt.Sprintf("int-compact-idem-%d", time.Now().UnixNano())
	_, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
		VALUES ($1, 'test', 1, 'ready', '{}', 'default') ON CONFLICT DO NOTHING`, runID)
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	defer func() {
		db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, runID)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)
	}()

	// Insert many events.
	const numEvents = 25
	const threshold = 5

	var events []engine.EventRecord
	for i := 0; i < numEvents; i++ {
		events = append(events, engine.EventRecord{
			Step:      i,
			EventType: engine.EventTypeCall,
			Service:   "svc",
			Op:        fmt.Sprintf("op-%d", i),
			Request:   `{}`,
			Response:  `{"ok":true}`,
		})
	}
	if err := store.AppendEventHistoryBatch(ctx, runID, events); err != nil {
		t.Fatalf("append events: %v", err)
	}

	// First compaction.
	if err := engine.CompactWorkflowHistory(ctx, store, runID, threshold); err != nil {
		t.Fatalf("first compaction: %v", err)
	}
	afterFirst, err := store.LoadEventHistory(ctx, runID)
	if err != nil {
		t.Fatalf("load after first compaction: %v", err)
	}
	cs1, err := store.LoadCompactionState(ctx, runID)
	if err != nil {
		t.Fatalf("LoadCompactionState after first: %v", err)
	}

	// Second compaction — should be a no-op since event count is now <= threshold.
	if err := engine.CompactWorkflowHistory(ctx, store, runID, threshold); err != nil {
		t.Fatalf("second compaction: %v", err)
	}
	afterSecond, err := store.LoadEventHistory(ctx, runID)
	if err != nil {
		t.Fatalf("load after second compaction: %v", err)
	}
	cs2, err := store.LoadCompactionState(ctx, runID)
	if err != nil {
		t.Fatalf("LoadCompactionState after second: %v", err)
	}

	// Event count must be identical.
	if len(afterFirst) != len(afterSecond) {
		t.Errorf("event count changed after second compaction: %d vs %d",
			len(afterFirst), len(afterSecond))
	}

	// Compaction state must be identical.
	if (cs1 == nil) != (cs2 == nil) {
		t.Errorf("compaction state existence changed: first=%v, second=%v",
			cs1 != nil, cs2 != nil)
	}
	if cs1 != nil && cs2 != nil {
		if cs1.CompactedStep != cs2.CompactedStep {
			t.Errorf("CompactedStep changed: first=%d, second=%d",
				cs1.CompactedStep, cs2.CompactedStep)
		}
		if len(cs1.Events) != len(cs2.Events) {
			t.Errorf("compacted event count changed: first=%d, second=%d",
				len(cs1.Events), len(cs2.Events))
		}
	}
}

// TestCompactionEdgeCases tests compaction with boundary conditions.
func TestCompactionEdgeCases(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := engine.NewPostgresStore(db)
	ctx := context.Background()

	tests := []struct {
		name         string
		numEvents    int
		threshold    int
		expectChange bool // whether compaction should reduce event count
	}{
		{name: "zero events", numEvents: 0, threshold: 5, expectChange: false},
		{name: "single event", numEvents: 1, threshold: 5, expectChange: false},
		{name: "below threshold", numEvents: 4, threshold: 5, expectChange: false},
		{name: "at threshold", numEvents: 5, threshold: 5, expectChange: false}, // == threshold, skip
		{name: "above threshold", numEvents: 10, threshold: 5, expectChange: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runID := fmt.Sprintf("int-compact-edge-%s-%d", tt.name, time.Now().UnixNano())
			_, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
				VALUES ($1, 'test', 1, 'ready', '{}', 'default') ON CONFLICT DO NOTHING`, runID)
			if err != nil {
				t.Fatalf("create workflow: %v", err)
			}
			defer func() {
				db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, runID)
				db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)
			}()

			// Insert events.
			var events []engine.EventRecord
			for i := 0; i < tt.numEvents; i++ {
				events = append(events, engine.EventRecord{
					Step:      i,
					EventType: engine.EventTypeCall,
					Service:   "svc",
					Op:        fmt.Sprintf("op-%d", i),
					Request:   `{}`,
					Response:  `{"ok":true}`,
				})
			}
			if tt.numEvents > 0 {
				if err := store.AppendEventHistoryBatch(ctx, runID, events); err != nil {
					t.Fatalf("append events: %v", err)
				}
			}

			beforeCount := tt.numEvents

			// Compact should not error in any edge case.
			if err := engine.CompactWorkflowHistory(ctx, store, runID, tt.threshold); err != nil {
				t.Fatalf("CompactWorkflowHistory: %v", err)
			}

			after, err := store.LoadEventHistory(ctx, runID)
			if err != nil {
				t.Fatalf("LoadEventHistory: %v", err)
			}

			if tt.expectChange {
				if len(after) >= beforeCount {
					t.Errorf("expected event count to decrease (was %d, now %d)", beforeCount, len(after))
				}
				// Verify compaction state exists.
				cs, err := store.LoadCompactionState(ctx, runID)
				if err != nil {
					t.Errorf("LoadCompactionState: %v", err)
				}
				if cs == nil {
					t.Error("expected compaction state after compaction")
				}
			} else {
				if len(after) != beforeCount {
					t.Errorf("expected event count unchanged (%d), got %d", beforeCount, len(after))
				}
			}
		})
	}
}
