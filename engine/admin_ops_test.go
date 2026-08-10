package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestAdminActionEventConversion(t *testing.T) {
	e := AdminActionEvent{
		step:     5,
		Action:   "force_complete",
		Operator: "test-operator-123",
		Reason:   "testing conversion",
	}

	rec := EventRecordFromEvent(e)
	if rec.Step != 5 {
		t.Errorf("Step: got %d, want 5", rec.Step)
	}
	if rec.EventType != EventTypeAdminAction {
		t.Errorf("EventType: got %s, want %s", rec.EventType, EventTypeAdminAction)
	}
	if rec.Service != "test-operator-123" {
		t.Errorf("Service: got %s, want test-operator-123", rec.Service)
	}
	if rec.Op != "force_complete" {
		t.Errorf("Op: got %s, want force_complete", rec.Op)
	}
	if rec.Err != "testing conversion" {
		t.Errorf("Err: got %s, want testing conversion", rec.Err)
	}

	// Round-trip.
	e2 := EventFromRecord(rec)
	if e2 == nil {
		t.Fatal("EventFromRecord returned nil")
	}
	ae2, ok := e2.(AdminActionEvent)
	if !ok {
		t.Fatalf("EventFromRecord returned %T, want AdminActionEvent", e2)
	}
	if ae2.Step() != 5 {
		t.Errorf("round-trip Step: got %d, want 5", ae2.Step())
	}
	if ae2.Action != "force_complete" {
		t.Errorf("round-trip Action: got %s, want force_complete", ae2.Action)
	}
	if ae2.Operator != "test-operator-123" {
		t.Errorf("round-trip Operator: got %s, want test-operator-123", ae2.Operator)
	}
	if ae2.Reason != "testing conversion" {
		t.Errorf("round-trip Reason: got %s, want testing conversion", ae2.Reason)
	}
}

// adminOpsTestStore is a minimal WorkflowStore stub for testing admin ops.
type adminOpsTestStore struct {
	WorkflowStore    // embed to satisfy interface; only override what we need
	forceCompleteErr error
	forceFailErr     error
	reReplayErr      error
}

func (s *adminOpsTestStore) AdminForceComplete(ctx context.Context, workflowID string, generation int64, result string, operator string) error {
	return s.forceCompleteErr
}

func (s *adminOpsTestStore) AdminForceFail(ctx context.Context, workflowID string, generation int64, errorMsg, errorCode string, operator string) error {
	return s.forceFailErr
}

func (s *adminOpsTestStore) AdminReReplay(ctx context.Context, workflowID string, generation int64, operator string) error {
	return s.reReplayErr
}

func TestForceComplete_Validation(t *testing.T) {
	store := &adminOpsTestStore{}

	err := ForceComplete(context.Background(), store, "", 0, "op", "result")
	if err == nil || !strings.Contains(err.Error(), "workflow ID is required") {
		t.Errorf("expected 'workflow ID is required' error, got: %v", err)
	}

	err = ForceComplete(context.Background(), store, "wf-1", -1, "op", "result")
	if err == nil || !strings.Contains(err.Error(), "generation must be >= 0") {
		t.Errorf("expected 'generation must be >= 0' error, got: %v", err)
	}
}

func TestForceFail_Validation(t *testing.T) {
	store := &adminOpsTestStore{}

	err := ForceFail(context.Background(), store, "", 0, "op", "msg", "code")
	if err == nil || !strings.Contains(err.Error(), "workflow ID is required") {
		t.Errorf("expected 'workflow ID is required' error, got: %v", err)
	}
}

func TestReReplay_Validation(t *testing.T) {
	store := &adminOpsTestStore{}

	err := ReReplay(context.Background(), store, "", 0, "op")
	if err == nil || !strings.Contains(err.Error(), "workflow ID is required") {
		t.Errorf("expected 'workflow ID is required' error, got: %v", err)
	}
}

func TestForceComplete_OperatorDefault(t *testing.T) {
	// Empty operator should default to "unknown" and not fail.
	store := &adminOpsTestStore{}
	err := ForceComplete(context.Background(), store, "wf-1", 0, "", "{}")
	if err != nil {
		t.Errorf("expected success with empty operator, got: %v", err)
	}
}

func TestAdminOps_ErrorPropagation(t *testing.T) {
	store := &adminOpsTestStore{
		forceCompleteErr: errors.New("admin force-complete: generation mismatch for workflow wf-1"),
	}

	err := ForceComplete(context.Background(), store, "wf-1", 5, "op", "{}")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "generation mismatch") {
		t.Errorf("expected generation mismatch error, got: %v", err)
	}
}

func TestAdminOps_UnknownWorkflow(t *testing.T) {
	store := &adminOpsTestStore{
		forceFailErr: errors.New("admin force-fail: workflow wf-999 not found"),
	}

	err := ForceFail(context.Background(), store, "wf-999", 5, "op", "boom", "ERR")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' error, got: %v", err)
	}
}
