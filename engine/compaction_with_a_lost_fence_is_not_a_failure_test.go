package engine

import (
	"context"
	"fmt"
	"testing"
)

// cleat#3409: CompactHistory returns ErrFenceLost, having rolled back, when a
// reclaim moved the generation under it. CompactWorkflowHistory is the worker's
// only caller and logs any error it returns at ERROR level, once per candidate
// per tick, so a race ErrFenceLost's own doc calls expected must not come back
// as one. A genuine store error still must.
func TestCompactWorkflowHistoryTreatsALostFenceAsNothingToDo(t *testing.T) {
	events := make([]EventRecord, 1500)
	for i := range events {
		events[i] = EventRecord{Step: i, EventType: EventTypeCall, Service: "svc", Op: fmt.Sprintf("op%d", i)}
	}

	lost := &mockCompactStore{events: events, compactErr: fmt.Errorf("compact history: %w", ErrFenceLost)}
	if err := CompactWorkflowHistory(context.Background(), lost, "wf-lost-fence", 1000, nil); err != nil {
		t.Errorf("a lost fence was returned as a failure: %v", err)
	}
	if lost.compactCount != 1 {
		t.Errorf("CompactHistory calls = %d, want 1: a lost fence is not a deadlock and is not retried within the tick", lost.compactCount)
	}

	broken := &mockCompactStore{events: events, compactErr: fmt.Errorf("store error")}
	if err := CompactWorkflowHistory(context.Background(), broken, "wf-store-error", 1000, nil); err == nil {
		t.Error("an ordinary store error was swallowed along with the lost fence")
	}
}
