package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// EventTypeSleep is the event type for DurableSleep events.
// Defined here for test compilation compatibility; sleep events use
// sleepStatus* constants from types.go for the actual result encoding.
const EventTypeSleep = "sleep"

// ---------------------------------------------------------------------------
// Flexible mock for ChildWorkflowStore with per-method function fields.
// ---------------------------------------------------------------------------

type mockChildStore struct {
	startChildAtomicFn      func(ctx context.Context, childID, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, event EventRecord, priority int) (string, error)
	startChildFn            func(ctx context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, priority int) (string, error)
	getChildResultFn        func(ctx context.Context, runID string) (ChildOutcome, error)
	getChildCompletedAtMsFn func(ctx context.Context, runID string) (int64, bool, error)
	resolveTagFn            func(ctx context.Context, workflowName string, tag string) (int, error)
}

func (m *mockChildStore) StartChildWorkflowAtomic(ctx context.Context, childID, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, event EventRecord, priority int) (string, error) {
	if m.startChildAtomicFn != nil {
		return m.startChildAtomicFn(ctx, childID, parentID, defName, inputJSON, defVersion, parentClosePolicy, event, priority)
	}
	return "child-run-atomic", nil
}

func (m *mockChildStore) StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, priority int) (string, error) {
	if m.startChildFn != nil {
		return m.startChildFn(ctx, parentID, defName, inputJSON, defVersion, parentClosePolicy, priority)
	}
	return "child-run-start", nil
}

func (m *mockChildStore) GetChildResult(ctx context.Context, runID string) (ChildOutcome, error) {
	if m.getChildResultFn != nil {
		return m.getChildResultFn(ctx, runID)
	}
	return ChildOutcome{}, nil
}

func (m *mockChildStore) ResolveVersionByTag(ctx context.Context, workflowName string, tag string) (int, error) {
	if m.resolveTagFn != nil {
		return m.resolveTagFn(ctx, workflowName, tag)
	}
	return 0, nil
}

// ---------------------------------------------------------------------------
// resolveChildVersion tests.
// ---------------------------------------------------------------------------

func TestResolveChildVersion_Explicit(t *testing.T) {
	s := newTestExecSession()
	v := s.resolveChildVersion(context.Background(), "test-wf", 42)
	if v != 42 {
		t.Errorf("expected explicit version 42, got %d", v)
	}
}

func TestResolveChildVersion_OverrideLatest(t *testing.T) {
	s := newTestExecSession()
	s.engine.childBindingOverride = "latest"
	v := s.resolveChildVersion(context.Background(), "test-wf", 0)
	if v != 0 {
		t.Errorf("expected 0 for latest override, got %d", v)
	}
}

func TestResolveChildVersion_OverrideTag(t *testing.T) {
	resolveCalled := false
	store := &mockChildStore{
		resolveTagFn: func(ctx context.Context, workflowName string, tag string) (int, error) {
			resolveCalled = true
			if tag != "canary" {
				t.Errorf("expected tag 'canary', got %q", tag)
			}
			return 7, nil
		},
	}
	s := newTestExecSession()
	s.engine.childBindingOverride = "tag:canary"
	s.engine.childWfStore = store

	v := s.resolveChildVersion(context.Background(), "test-wf", 0)
	if v != 7 {
		t.Errorf("expected version 7 from tag override, got %d", v)
	}
	if !resolveCalled {
		t.Error("expected ResolveVersionByTag to be called")
	}
}

func TestResolveChildVersion_Frozen(t *testing.T) {
	s := newTestExecSession()
	s.engine.childBindingPolicy = "frozen"
	s.engine.state = &stubWorkflowState{childVer: map[string]int{"test-wf": 5}}

	v := s.resolveChildVersion(context.Background(), "test-wf", 0)
	if v != 5 {
		t.Errorf("expected frozen version 5, got %d", v)
	}
}

func TestResolveChildVersion_FrozenNoPin(t *testing.T) {
	s := newTestExecSession()
	s.engine.childBindingPolicy = "frozen"
	s.engine.state = &stubWorkflowState{} // no childVer map

	v := s.resolveChildVersion(context.Background(), "test-wf", 0)
	if v != 0 {
		t.Errorf("expected 0 (no pinned version), got %d", v)
	}
}

func TestResolveChildVersion_Stable(t *testing.T) {
	store := &mockChildStore{
		resolveTagFn: func(ctx context.Context, workflowName string, tag string) (int, error) {
			if tag != "stable" {
				t.Errorf("expected tag 'stable', got %q", tag)
			}
			return 3, nil
		},
	}
	s := newTestExecSession()
	s.engine.childBindingPolicy = "stable"
	s.engine.childWfStore = store
	s.engine.state = &stubWorkflowState{}

	v := s.resolveChildVersion(context.Background(), "test-wf", 0)
	if v != 3 {
		t.Errorf("expected stable version 3, got %d", v)
	}
}

func TestResolveChildVersion_StableNoStore(t *testing.T) {
	s := newTestExecSession()
	s.engine.childBindingPolicy = "stable"
	s.engine.state = &stubWorkflowState{}
	// childWfStore is nil

	v := s.resolveChildVersion(context.Background(), "test-wf", 0)
	if v != 0 {
		t.Errorf("expected 0 when stable resolution fails, got %d", v)
	}
}

func TestResolveChildVersion_Latest(t *testing.T) {
	s := newTestExecSession()
	s.engine.childBindingPolicy = "latest"
	s.engine.state = &stubWorkflowState{}

	v := s.resolveChildVersion(context.Background(), "test-wf", 0)
	if v != 0 {
		t.Errorf("expected 0 for latest policy, got %d", v)
	}
}

func TestResolveChildVersion_TagPolicy(t *testing.T) {
	store := &mockChildStore{
		resolveTagFn: func(ctx context.Context, workflowName string, tag string) (int, error) {
			if tag != "beta" {
				t.Errorf("expected tag 'beta', got %q", tag)
			}
			return 9, nil
		},
	}
	s := newTestExecSession()
	s.engine.childBindingPolicy = "tag:beta"
	s.engine.childWfStore = store
	s.engine.state = &stubWorkflowState{}

	v := s.resolveChildVersion(context.Background(), "test-wf", 0)
	if v != 9 {
		t.Errorf("expected version 9 from tag policy, got %d", v)
	}
}

func TestResolveChildVersion_FallbackFrozen(t *testing.T) {
	s := newTestExecSession()
	s.engine.state = &stubWorkflowState{childVer: map[string]int{"test-wf": 4}}
	// childBindingPolicy is empty → should fall back to "frozen" since pinned version exists.

	v := s.resolveChildVersion(context.Background(), "test-wf", 0)
	if v != 4 {
		t.Errorf("expected version 4 from frozen fallback, got %d", v)
	}
}

func TestResolveChildVersion_FallbackLatest(t *testing.T) {
	s := newTestExecSession()
	s.engine.state = &stubWorkflowState{} // no pinned version
	// childBindingPolicy is empty → should fall back to "latest".

	v := s.resolveChildVersion(context.Background(), "test-wf", 0)
	if v != 0 {
		t.Errorf("expected 0 from latest fallback, got %d", v)
	}
}

func TestResolveChildVersion_NoState(t *testing.T) {
	s := newTestExecSession()
	// engine.state is nil
	v := s.resolveChildVersion(context.Background(), "test-wf", 0)
	if v != 0 {
		t.Errorf("expected 0 when state is nil, got %d", v)
	}
}

// ---------------------------------------------------------------------------
// childWorkflowWithVersion tests.
// ---------------------------------------------------------------------------

func TestChildWorkflowWithVersion_ReplayMatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: EventTypeChildWorkflow,
		RunID:     "replay-run-id",
	}}

	result := s.childWorkflowWithVersion(context.Background(), nil, "test-wf", `{}`, 0, 0, "", 0, 0)

	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
	if !s.isReplay {
		t.Error("expected isReplay to remain true")
	}
	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
}

func TestChildWorkflowWithVersion_ReplayMismatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: EventTypeCall, // wrong type
	}}

	result := s.childWorkflowWithVersion(context.Background(), nil, "test-wf", `{}`, 0, 0, "", 0, 0)

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	if s.isReplay {
		t.Error("expected replay to have ended")
	}
	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0 (fresh path), got %d", errCode)
	}
}

func TestChildWorkflowWithVersion_FreshWithStore(t *testing.T) {
	store := &mockChildStore{}
	s := newTestExecSession()
	s.engine.childWfStore = store
	s.workflowID = "parent-wf"

	result := s.childWorkflowWithVersion(context.Background(), nil, "test-wf", `{"x":1}`, 0, 0, "", 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	if len(s.history) < 1 {
		t.Error("expected at least 1 history entry")
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
}

func TestChildWorkflowWithVersion_FreshWithoutStore(t *testing.T) {
	s := newTestExecSession()
	s.workflowID = "parent-wf"

	result := s.childWorkflowWithVersion(context.Background(), nil, "test-wf", `{}`, 0, 0, "", 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	// Without store, a synthetic runID is created.
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
	rec := s.history[0]
	if rec.ChildName != "test-wf" {
		t.Errorf("expected ChildName 'test-wf', got %q", rec.ChildName)
	}
	if rec.RunID == "" {
		t.Error("expected synthetic RunID")
	}
}

func TestChildWorkflowWithVersion_StoreError(t *testing.T) {
	store := &mockChildStore{
		startChildAtomicFn: func(ctx context.Context, childID, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, event EventRecord, priority int) (string, error) {
			return "", fmt.Errorf("store unavailable")
		},
	}
	s := newTestExecSession()
	s.engine.childWfStore = store
	s.workflowID = "parent-wf"

	result := s.childWorkflowWithVersion(context.Background(), nil, "test-wf", `{}`, 0, 0, "", 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	// errCode 3 = not_found / start failed
	if errCode != 3 {
		t.Errorf("expected errCode 3 (start failed), got %d", errCode)
	}
}

// ---------------------------------------------------------------------------
// AwaitChild tests.
// ---------------------------------------------------------------------------

func TestAwaitChild_ReplayCachedResult(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: EventTypeAwaitChild,
		RunID:     "run-1",
		Response:  `{"status":"done"}`,
	}}

	result := s.AwaitChild(context.Background(), nil, "run-1", 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
	if !s.isReplay {
		t.Error("expected isReplay to remain true")
	}
}

func TestAwaitChild_ReplayCachedError(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: EventTypeAwaitChild,
		RunID:     "run-1",
		Err:       "child failed",
	}}

	result := s.AwaitChild(context.Background(), nil, "run-1", 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 1 {
		t.Errorf("expected errCode 1, got %d", errCode)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
}

func TestAwaitChild_ReplayPastEnd(t *testing.T) {
	mock := &mockChildStore{getChildResultFn: func(ctx context.Context, runID string) (ChildOutcome, error) {
		return ChildOutcome{}, nil // not completed
	}}
	s := newTestExecSession()
	s.engine.childWfStore = mock
	s.isReplay = true
	s.history = nil // past end

	result := s.AwaitChild(context.Background(), nil, "run-1", 0, 0)

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	if result != packAwaitChildResultSuspend() {
		t.Errorf("expected suspend sentinel, got %d", result)
	}
	if s.suspendErr == nil {
		t.Fatal("expected suspendErr non-nil")
	}
}

func TestAwaitChild_FreshCompleted(t *testing.T) {
	mock := &mockChildStore{getChildResultFn: func(ctx context.Context, runID string) (ChildOutcome, error) {
		return ChildOutcome{Completed: true, Result: `{"result":"ok"}`}, nil
	}}
	s := newTestExecSession()
	s.engine.childWfStore = mock

	result := s.AwaitChild(context.Background(), nil, "run-1", 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].Response != `{"result":"ok"}` {
		t.Errorf("expected Response %q, got %q", `{"result":"ok"}`, s.history[0].Response)
	}
}

func TestAwaitChild_FreshError(t *testing.T) {
	mock := &mockChildStore{getChildResultFn: func(ctx context.Context, runID string) (ChildOutcome, error) {
		return ChildOutcome{}, fmt.Errorf("db error")
	}}
	s := newTestExecSession()
	s.engine.childWfStore = mock

	result := s.AwaitChild(context.Background(), nil, "run-1", 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 1 {
		t.Errorf("expected errCode 1, got %d", errCode)
	}
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].Err != "db error" {
		t.Errorf("expected Err 'db error', got %q", s.history[0].Err)
	}
}

func TestAwaitChild_FreshNotCompleted(t *testing.T) {
	mock := &mockChildStore{getChildResultFn: func(ctx context.Context, runID string) (ChildOutcome, error) {
		return ChildOutcome{}, nil // not completed, no error
	}}
	s := newTestExecSession()
	s.engine.childWfStore = mock

	result := s.AwaitChild(context.Background(), nil, "run-1", 0, 0)

	if result != packAwaitChildResultSuspend() {
		t.Errorf("expected suspend sentinel, got %d", result)
	}
	if s.suspendErr == nil {
		t.Fatal("expected suspendErr non-nil")
	}
	if !strings.Contains(s.suspendErr.Reason, "await_child(run-1)") {
		t.Errorf("expected 'await_child(run-1)' in reason, got %q", s.suspendErr.Reason)
	}
}

func TestAwaitChild_FreshNoStore(t *testing.T) {
	s := newTestExecSession()
	// childWfStore is nil

	result := s.AwaitChild(context.Background(), nil, "run-1", 0, 0)

	if result != packAwaitChildResultSuspend() {
		t.Errorf("expected suspend sentinel, got %d", result)
	}
	if s.suspendErr == nil {
		t.Fatal("expected suspendErr non-nil")
	}
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
}

// ---------------------------------------------------------------------------
// PollChild tests.
// ---------------------------------------------------------------------------

func TestPollChild_Completed(t *testing.T) {
	// Since #847, "completed" is not a property of now -- it is a property of
	// the parent's durable clock: the child must have completed at or before
	// it. This test used to say only that the child was done, which is the
	// question PollChild stopped asking.
	mock := &mockChildStore{
		getChildResultFn: func(ctx context.Context, runID string) (ChildOutcome, error) {
			return ChildOutcome{Completed: true, Result: `{"ok":true}`}, nil
		},
		getChildCompletedAtMsFn: func(ctx context.Context, runID string) (int64, bool, error) {
			return 1_000, true, nil
		},
	}
	s := newTestExecSession()
	s.nowMs = 2_000 // durable clock is after the child completed
	s.engine.childWfStore = mock

	buf := make([]byte, 256)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	result := s.PollChild(ctx, nil, "run-1", 0, uint32(len(buf)))

	var pr struct {
		Status string `json:"status"`
		Result string `json:"result,omitempty"`
	}
	if err := json.Unmarshal(buf[:result>>32], &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pr.Status != "completed" {
		t.Errorf("expected status 'completed', got %q", pr.Status)
	}
	if pr.Result != `{"ok":true}` {
		t.Errorf("expected result %q, got %q", `{"ok":true}`, pr.Result)
	}
}

func TestPollChild_Running(t *testing.T) {
	mock := &mockChildStore{getChildResultFn: func(ctx context.Context, runID string) (ChildOutcome, error) {
		return ChildOutcome{}, nil
	}}
	s := newTestExecSession()
	s.engine.childWfStore = mock

	buf := make([]byte, 256)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	result := s.PollChild(ctx, nil, "run-1", 0, uint32(len(buf)))

	var pr struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(buf[:result>>32], &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pr.Status != "running" {
		t.Errorf("expected status 'running', got %q", pr.Status)
	}
}

func TestPollChild_Failed(t *testing.T) {
	mock := &mockChildStore{getChildResultFn: func(ctx context.Context, runID string) (ChildOutcome, error) {
		return ChildOutcome{}, fmt.Errorf("connection refused")
	}}
	s := newTestExecSession()
	s.engine.childWfStore = mock

	buf := make([]byte, 256)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	result := s.PollChild(ctx, nil, "run-1", 0, uint32(len(buf)))

	var pr struct {
		Status string `json:"status"`
		Error  string `json:"error,omitempty"`
	}
	if err := json.Unmarshal(buf[:result>>32], &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pr.Status != "failed" {
		t.Errorf("expected status 'failed', got %q", pr.Status)
	}
	if pr.Error != "connection refused" {
		t.Errorf("expected error 'connection refused', got %q", pr.Error)
	}
}

func TestPollChild_NilStore(t *testing.T) {
	s := newTestExecSession()

	buf := make([]byte, 256)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	result := s.PollChild(ctx, nil, "run-1", 0, uint32(len(buf)))

	var pr struct {
		Status string `json:"status"`
		Error  string `json:"error,omitempty"`
	}
	if err := json.Unmarshal(buf[:result>>32], &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pr.Status != "failed" {
		t.Errorf("expected status 'failed', got %q", pr.Status)
	}
	if pr.Error != "no child workflow store" {
		t.Errorf("expected error 'no child workflow store', got %q", pr.Error)
	}
}

func TestPollChild_EmptyResult(t *testing.T) {
	// Since #847, "completed" is not a property of now -- it is a property of
	// the parent's durable clock: the child must have completed at or before
	// it. This test used to say only that the child was done, which is the
	// question PollChild stopped asking.
	//
	// THE PREMISE OF THIS TEST WAS THE DEFECT, and it said so out loud: the
	// mock's comment read "completed but empty result == failed", which is not
	// a fact about a child, it is a description of the guess PollChild made
	// because the store could not tell it whether the child had failed
	// (cleat#1115). A child that succeeds and returns nothing is a child that
	// succeeded. The store now answers directly, the guess is gone, and this
	// asserts the corrected behaviour -- see TestPollChild_ChildFailed for the
	// case the guess was standing in for.
	mock := &mockChildStore{
		getChildResultFn: func(ctx context.Context, runID string) (ChildOutcome, error) {
			return ChildOutcome{Completed: true, Result: ""}, nil
		},
		getChildCompletedAtMsFn: func(ctx context.Context, runID string) (int64, bool, error) {
			return 1_000, true, nil
		},
	}
	s := newTestExecSession()
	s.nowMs = 2_000 // durable clock is after the child completed
	s.engine.childWfStore = mock

	buf := make([]byte, 256)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	result := s.PollChild(ctx, nil, "run-1", 0, uint32(len(buf)))

	var pr struct {
		Status string `json:"status"`
		Error  string `json:"error,omitempty"`
	}
	if err := json.Unmarshal(buf[:result>>32], &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pr.Status != "completed" {
		t.Errorf("a child that completed with an EMPTY result is reported as %q, want "+
			"\"completed\". An empty result is a plausible success value; reporting it as a "+
			"failure is the old guess, wrong in the other direction (cleat#1115).", pr.Status)
	}
	if pr.Error != "" {
		t.Errorf("expected no error for a child that succeeded, got %q", pr.Error)
	}
}

// TestPollChild_ChildFailed is the case the "empty result" guess above stood
// in for, and could not distinguish: a child that genuinely failed, with a
// message of its own.
func TestPollChild_ChildFailed(t *testing.T) {
	mock := &mockChildStore{
		getChildResultFn: func(ctx context.Context, runID string) (ChildOutcome, error) {
			return ChildOutcome{Completed: true, Failed: true, Error: "child blew up"}, nil
		},
		getChildCompletedAtMsFn: func(ctx context.Context, runID string) (int64, bool, error) {
			return 1_000, true, nil
		},
	}
	s := newTestExecSession()
	s.nowMs = 2_000
	s.engine.childWfStore = mock

	buf := make([]byte, 256)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	result := s.PollChild(ctx, nil, "run-1", 0, uint32(len(buf)))

	var pr struct {
		Status string `json:"status"`
		Error  string `json:"error,omitempty"`
	}
	if err := json.Unmarshal(buf[:result>>32], &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pr.Status != "failed" {
		t.Errorf("a child that FAILED is reported as %q, want \"failed\"", pr.Status)
	}
	if pr.Error != "child blew up" {
		t.Errorf("the poll result carries error %q, want the child's own message. A failure "+
			"flag with no message moves the defect rather than fixing it.", pr.Error)
	}
}

// ---------------------------------------------------------------------------
// AwaitAnyChild tests.
// ---------------------------------------------------------------------------

func TestAwaitAnyChild_ReplayCached(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: EventTypeAwaitAnyChild,
		Response:  `{"run_id":"run-1","result":"done"}`,
	}}

	result := s.AwaitAnyChild(context.Background(), nil, `["run-1","run-2"]`, 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
	if !s.isReplay {
		t.Error("expected isReplay to remain true")
	}
}

func TestAwaitAnyChild_ReplayEmptyThenCached(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	// First event: empty Response (suspend), second: cached result (re-execution).
	s.history = []EventRecord{
		{
			Step:      0,
			EventType: EventTypeAwaitAnyChild,
			Response:  "", // empty = suspend
		},
		{
			Step:      1,
			EventType: EventTypeAwaitAnyChild,
			Response:  `{"run_id":"run-2","result":"done"}`,
		},
	}

	result := s.AwaitAnyChild(context.Background(), nil, `["run-1","run-2"]`, 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	// Two events consumed.
	if s.stepCount != 2 {
		t.Errorf("expected stepCount=2, got %d", s.stepCount)
	}
	if !s.isReplay {
		t.Error("expected isReplay to remain true")
	}
}

func TestAwaitAnyChild_ReplayMismatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: EventTypeCall, // wrong type
	}}

	result := s.AwaitAnyChild(context.Background(), nil, `["run-1"]`, 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 1 {
		t.Errorf("expected errCode 1 (divergence), got %d", errCode)
	}
	if !s.isReplay {
		t.Error("expected isReplay to remain true (divergence)")
	}
}

func TestAwaitAnyChild_ReplayPastEnd(t *testing.T) {
	mock := &mockChildStore{getChildResultFn: func(ctx context.Context, runID string) (ChildOutcome, error) {
		return ChildOutcome{}, nil
	}}
	s := newTestExecSession()
	s.engine.childWfStore = mock
	s.isReplay = true
	s.history = nil

	result := s.AwaitAnyChild(context.Background(), nil, `["run-1"]`, 0, 0)

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	if result != packAwaitChildResultSuspend() {
		t.Errorf("expected suspend sentinel, got %d", result)
	}
}

func TestAwaitAnyChild_FreshCompleted(t *testing.T) {
	callCount := 0
	mock := &mockChildStore{
		getChildResultFn: func(ctx context.Context, runID string) (ChildOutcome, error) {
			callCount++
			// First child is completed.
			return ChildOutcome{Completed: true, Result: `{"result":"done"}`}, nil
		},
	}
	s := newTestExecSession()
	s.engine.childWfStore = mock

	result := s.AwaitAnyChild(context.Background(), nil, `["run-1","run-2"]`, 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	if callCount != 1 {
		t.Errorf("expected 1 GetChildResult call, got %d", callCount)
	}
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].Response == "" {
		t.Error("expected non-empty Response in history")
	}
}

func TestAwaitAnyChild_FreshAllRunning(t *testing.T) {
	mock := &mockChildStore{getChildResultFn: func(ctx context.Context, runID string) (ChildOutcome, error) {
		return ChildOutcome{}, nil // all running
	}}
	s := newTestExecSession()
	s.engine.childWfStore = mock

	result := s.AwaitAnyChild(context.Background(), nil, `["run-1"]`, 0, 0)

	if result != packAwaitChildResultSuspend() {
		t.Errorf("expected suspend sentinel, got %d", result)
	}
	if s.suspendErr == nil {
		t.Fatal("expected suspendErr non-nil")
	}
	if !strings.Contains(s.suspendErr.Reason, "await_any_child") {
		t.Errorf("expected 'await_any_child' in reason, got %q", s.suspendErr.Reason)
	}
}

func TestAwaitAnyChild_InvalidJSON(t *testing.T) {
	s := newTestExecSession()

	result := s.AwaitAnyChild(context.Background(), nil, `not-json`, 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 1 {
		t.Errorf("expected errCode 1, got %d", errCode)
	}
}

// ---------------------------------------------------------------------------
// AwaitAllChildren tests.
// ---------------------------------------------------------------------------

func TestAwaitAllChildren_AllCompleted(t *testing.T) {
	mock := &mockChildStore{
		getChildResultFn: func(ctx context.Context, runID string) (ChildOutcome, error) {
			return ChildOutcome{Completed: true, Result: `{"result":"` + runID + `"}`}, nil
		},
	}
	s := newTestExecSession()
	s.engine.childWfStore = mock

	result := s.AwaitAllChildren(context.Background(), nil, `["run-a","run-b"]`, 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
	// Response should be JSON array of outcomes.
	var outcomes []struct {
		RunID  string `json:"run_id"`
		Result string `json:"result,omitempty"`
		Error  string `json:"error,omitempty"`
	}
	if err := json.Unmarshal([]byte(s.history[0].Response), &outcomes); err != nil {
		t.Fatalf("unmarshal outcomes: %v", err)
	}
	if len(outcomes) != 2 {
		t.Fatalf("expected 2 outcomes, got %d", len(outcomes))
	}
}

// TestAwaitAllChildren_SomeRunning asserts that a still-running child SUSPENDS
// the workflow rather than being reported as an outcome.
//
// This test previously asserted the opposite -- errCode 0 with
// Error: "child not completed" for run-b -- which is the behaviour
// IMPROVEMENT-PLAN 3.309 identified as a durable wrong answer. The outcomes are
// marshalled into the recorded EventRecord and replayAwaitAllChildren serves
// rec.Response verbatim forever, so "had not finished when I looked" became
// run-b's permanent result even after run-b completed.
//
// It asserted the code as written and justified neither half, which is why it
// held the defect in place rather than catching it.
func TestAwaitAllChildren_SomeRunning(t *testing.T) {
	mock := &mockChildStore{
		getChildResultFn: func(ctx context.Context, runID string) (ChildOutcome, error) {
			switch runID {
			case "run-a":
				return ChildOutcome{Completed: true, Result: `{"result":"a"}`}, nil
			default:
				return ChildOutcome{}, nil // still running
			}
		},
	}
	s := newTestExecSession()
	s.engine.childWfStore = mock

	buf := make([]byte, 512)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	result := s.AwaitAllChildren(ctx, nil, `["run-a","run-b"]`, 0, uint32(len(buf)))

	if result != packAwaitChildResultSuspend() {
		t.Errorf("a still-running child must suspend: got %#x, want %#x",
			result, packAwaitChildResultSuspend())
	}
	if s.suspendErr == nil {
		t.Fatal("expected suspendErr non-nil")
	}
	if !strings.Contains(s.suspendErr.Reason, "await_all_children(run-b)") {
		t.Errorf("expected 'await_all_children(run-b)' in reason, got %q", s.suspendErr.Reason)
	}

	// The recorded event must carry NO response. That is what makes the replay
	// half fall through to fresh and re-check; a response here would be replayed
	// verbatim forever.
	if len(s.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].Response != "" {
		t.Errorf("suspend record must have an empty response, got %q", s.history[0].Response)
	}
}

// TestAwaitAllChildren_ReplayOfSuspendRecordFallsThroughToFresh is the other
// half of the 3.309 fix, and without it half one is worse than the defect: the
// fresh path would suspend, and replay would then serve the suspend record's
// empty response as the answer -- a durable EMPTY result, which reads as
// success.
func TestAwaitAllChildren_ReplayOfSuspendRecordFallsThroughToFresh(t *testing.T) {
	// The child has completed by the time we replay.
	mock := &mockChildStore{
		getChildResultFn: func(ctx context.Context, runID string) (ChildOutcome, error) {
			return ChildOutcome{Completed: true, Result: `{"result":"done"}`}, nil
		},
	}
	s := newTestExecSession()
	s.engine.childWfStore = mock
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: EventTypeAwaitAllChildren,
		Request:   `["run-a"]`,
		// No Response: this is the suspend marker.
	}}

	buf := make([]byte, 512)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	result := s.AwaitAllChildren(ctx, nil, `["run-a"]`, 0, uint32(len(buf)))

	if result == packAwaitChildResultSuspend() {
		t.Fatal("replay suspended again although the child has since completed")
	}
	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Fatalf("expected errCode 0, got %d", errCode)
	}

	written := uint32(result >> 32)
	if written == 0 {
		t.Fatal("replay returned an EMPTY result -- the suspend record was served verbatim, " +
			"which is the failure this test exists to catch")
	}
	var outcomes []struct {
		RunID  string `json:"run_id"`
		Result string `json:"result,omitempty"`
		Error  string `json:"error,omitempty"`
	}
	if err := json.Unmarshal(buf[:written], &outcomes); err != nil {
		t.Fatalf("unmarshal outcomes: %v", err)
	}
	if len(outcomes) != 1 || outcomes[0].Result != `{"result":"done"}` {
		t.Errorf("expected the child's real result after fall-through, got %+v", outcomes)
	}
}

func TestReplayAwaitAllChildren_Match(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: EventTypeAwaitAllChildren,
		Request:   `["run-a","run-b"]`,
		Response:  `[{"run_id":"run-a","result":"a"},{"run_id":"run-b","result":"b"}]`,
	}}

	result := s.replayAwaitAllChildren(context.Background(), nil, `["run-a","run-b"]`, 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
}

func TestReplayAwaitAllChildren_MismatchType(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: EventTypeCall, // wrong type
	}}

	result := s.replayAwaitAllChildren(context.Background(), nil, `["run-a"]`, 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 1 {
		t.Errorf("expected errCode 1 (divergence), got %d", errCode)
	}
}

func TestReplayAwaitAllChildren_IDsMismatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: EventTypeAwaitAllChildren,
		Request:   `["run-x"]`,
		Response:  `[{"run_id":"run-x","result":"x"}]`,
	}}

	// Pass different run IDs than in history.
	result := s.replayAwaitAllChildren(context.Background(), nil, `["run-y"]`, 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 1 {
		t.Errorf("expected errCode 1 (divergence), got %d", errCode)
	}
}

func TestReplayAwaitAllChildren_PastEnd(t *testing.T) {
	mock := &mockChildStore{}
	s := newTestExecSession()
	s.engine.childWfStore = mock
	s.isReplay = true
	s.history = nil

	result := s.replayAwaitAllChildren(context.Background(), nil, `["run-a"]`, 0, 0)

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
}

func TestFreshAwaitAllChildren_InvalidJSON(t *testing.T) {
	s := newTestExecSession()

	result := s.freshAwaitAllChildren(context.Background(), nil, `not-json`, 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 1 {
		t.Errorf("expected errCode 1, got %d", errCode)
	}
}

// TestFreshAwaitAllChildren_NoStore asserts the consistency that started
// IMPROVEMENT-PLAN 3.309: AwaitChild suspends when no child workflow store is
// configured, and this returned success with "no child workflow store" as the
// child's outcome. That divergence is what made the two calls disagree about an
// identical run ID, and it was reported as a harness artifact before the
// ordinary-path version of it was found.
//
// It asserted errCode 0 and that error string until 2026-09-05.
func TestFreshAwaitAllChildren_NoStore(t *testing.T) {
	s := newTestExecSession()
	// childWfStore is nil

	buf := make([]byte, 512)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	result := s.freshAwaitAllChildren(ctx, nil, `["run-a"]`, 0, uint32(len(buf)))

	if result != packAwaitChildResultSuspend() {
		t.Errorf("with no child store configured AwaitAllChildren must suspend, as AwaitChild does: "+
			"got %#x, want %#x", result, packAwaitChildResultSuspend())
	}
	if s.suspendErr == nil {
		t.Fatal("expected suspendErr non-nil")
	}
}

// ---------------------------------------------------------------------------
// RunDetached tests.
// ---------------------------------------------------------------------------

func TestRunDetached_ReplayMatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:          0,
		EventType:     EventTypeRunDetached,
		DetachedName:  "detached-wf",
		DetachedInput: `{"x":1}`,
	}}

	result := s.RunDetached(context.Background(), nil, "detached-wf", `{"x":1}`)

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
	if !s.isReplay {
		t.Error("expected isReplay to remain true")
	}
}

func TestRunDetached_ReplayMismatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:          0,
		EventType:     EventTypeRunDetached,
		DetachedName:  "other-wf", // different name
		DetachedInput: `{}`,
	}}

	result := s.RunDetached(context.Background(), nil, "detached-wf", `{}`)

	if result != 1 {
		t.Errorf("expected 1 (mismatch), got %d", result)
	}
}

func TestRunDetached_ReplayWrongType(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: EventTypeCall, // wrong type
	}}

	result := s.RunDetached(context.Background(), nil, "detached-wf", `{}`)

	// Replay with wrong event type: advanceReplayStep increments stepCount but
	// EventType doesn't match, so it returns 1.
	if result != 1 {
		t.Errorf("expected 1, got %d", result)
	}
}

func TestRunDetached_FreshWithStore(t *testing.T) {
	startCalled := false
	store := &mockChildStore{
		startChildFn: func(ctx context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, priority int) (string, error) {
			startCalled = true
			return "detached-run-id", nil
		},
	}
	s := newTestExecSession()
	s.engine.childWfStore = store
	s.workflowID = "parent-wf"

	result := s.RunDetached(context.Background(), nil, "detached-wf", `{"x":1}`)

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if !startCalled {
		t.Error("expected StartChildWorkflow to be called")
	}
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeRunDetached {
		t.Errorf("expected EventTypeRunDetached, got %q", s.history[0].EventType)
	}
	if s.history[0].DetachedName != "detached-wf" {
		t.Errorf("expected DetachedName 'detached-wf', got %q", s.history[0].DetachedName)
	}
}

func TestRunDetached_FreshWithoutStore(t *testing.T) {
	s := newTestExecSession()
	s.workflowID = "parent-wf"

	result := s.RunDetached(context.Background(), nil, "detached-wf", `{}`)

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	// Without store, synthetic runID is created.
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeRunDetached {
		t.Errorf("expected EventTypeRunDetached, got %q", s.history[0].EventType)
	}
	if s.history[0].DetachedRunID == "" {
		t.Error("expected synthetic DetachedRunID")
	}
}

func TestChildWorkflowWithOptions_ExplicitVersion(t *testing.T) {
	// Test that ChildWorkflowOptions{Version: 10} resolves to version 10
	opts := ChildWorkflowOptions{Version: 10}
	if opts.Version != 10 {
		t.Errorf("expected Version 10, got %d", opts.Version)
	}
}

func TestChildWorkflowWithOptions_Wrapper(t *testing.T) {
	// ChildWorkflowWithOptions is a thin wrapper over childWorkflowWithVersion.
	// Test that it delegates correctly with version/priority passthrough.
	store := &mockChildStore{}
	s := newTestExecSession()
	s.engine.childWfStore = store
	s.workflowID = "parent-wf"

	result := s.ChildWorkflowWithOptions(context.Background(), nil, "test-wf", `{"x":1}`, 3, 5, "", 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	if len(s.history) < 1 {
		t.Error("expected at least 1 history entry")
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
}

func TestChildWorkflowWithOptions_DefaultVersion(t *testing.T) {
	// version=0 should be passed through as defVersion=0.
	store := &mockChildStore{}
	s := newTestExecSession()
	s.engine.childWfStore = store
	s.workflowID = "parent-wf"

	result := s.ChildWorkflowWithOptions(context.Background(), nil, "test-wf", `{}`, 0, 0, "", 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
}

func TestChildWorkflowWithOptions_NegativePriority(t *testing.T) {
	// Negative priority should pass through (parity with childWorkflowWithVersion).
	store := &mockChildStore{}
	s := newTestExecSession()
	s.engine.childWfStore = store
	s.workflowID = "parent-wf"

	result := s.ChildWorkflowWithOptions(context.Background(), nil, "test-wf", `{}`, 1, -1, "", 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
}

// GetChildCompletedAtMs satisfies the store interface. Added with #847, which
// made PollChild derive its answer from the child's completion instant rather
// than querying live. Returning ok=false means "never completed", which keeps
// every existing test's PollChild answer at "running".
func (m *mockChildStore) GetChildCompletedAtMs(ctx context.Context, runID string) (int64, bool, error) {
	if m.getChildCompletedAtMsFn != nil {
		return m.getChildCompletedAtMsFn(ctx, runID)
	}
	return 0, false, nil
}

// pollChildStatus drives PollChild once and returns the decoded status/error.
func pollChildStatus(t *testing.T, completedAtMs, nowMs int64) (string, string) {
	t.Helper()
	mock := &mockChildStore{
		getChildResultFn: func(ctx context.Context, runID string) (ChildOutcome, error) {
			return ChildOutcome{Completed: true, Result: `{"done":true}`}, nil // complete NOW, on every call
		},
		getChildCompletedAtMsFn: func(ctx context.Context, runID string) (int64, bool, error) {
			return completedAtMs, true, nil
		},
	}
	s := newTestExecSession()
	s.nowMs = nowMs
	s.engine.childWfStore = mock

	buf := make([]byte, 256)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	res := s.PollChild(ctx, nil, "run-1", 0, uint32(len(buf)))
	var pr struct {
		Status string `json:"status"`
		Error  string `json:"error,omitempty"`
	}
	if err := json.Unmarshal(buf[:res>>32], &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return pr.Status, pr.Error
}

// TestPollChildAnswersFromDurableTimeNotFromNow is the regression test for
// #847. The store says "complete" on every call -- that is what a replay sees,
// because by then the child really has finished. The question PollChild must
// answer is not that one. It is whether the child had finished as of the
// parent's durable clock, which is what the first execution observed.
//
// The failure this catches is not a crash. It is a workflow taking the other
// branch of an `if status == "running"` on replay, which produces divergence
// by a route the divergence check cannot attribute, because no poll event
// exists in the history to disagree about.
func TestPollChildAnswersFromDurableTimeNotFromNow(t *testing.T) {
	// The child completed at 5000. The parent's durable clock is 1000, so at
	// the point this call is being replayed the parent had not yet seen it.
	status, errMsg := pollChildStatus(t, 5_000, 1_000)
	if status != "running" {
		t.Fatalf("PollChild answered %q (err %q) for a child that completed at 5000 "+
			"when the parent's durable clock was 1000.\n"+
			"The store reports it complete NOW, and answering from NOW is exactly "+
			"the #847 defect: the original execution saw 'running' and this replay "+
			"would see 'completed', letting the workflow branch differently.",
			status, errMsg)
	}
}

// TestPollChildFlipsOnceDurableTimeReachesCompletion is the other half, and it
// is what stops the fix from being "always answer running", which would pass
// the test above and be useless. Same store state, later durable clock.
func TestPollChildFlipsOnceDurableTimeReachesCompletion(t *testing.T) {
	if status, errMsg := pollChildStatus(t, 5_000, 5_000); status != "completed" {
		t.Errorf("at durable time == completion the child must read completed, got %q (%q)", status, errMsg)
	}
	if status, errMsg := pollChildStatus(t, 5_000, 9_000); status != "completed" {
		t.Errorf("at durable time after completion the child must read completed, got %q (%q)", status, errMsg)
	}
	// One millisecond earlier and it must still be running -- the boundary is
	// <=, and an off-by-one here is a silent determinism hole rather than a
	// visible failure.
	if status, _ := pollChildStatus(t, 5_000, 4_999); status != "running" {
		t.Errorf("one ms before completion must read running, got %q", status)
	}
}

// TestPollChildFailsClosedWithoutACompletionInstant: a store that reports a
// child complete but cannot say when leaves PollChild with no replayable
// answer. It must say so rather than guess, because guessing "completed"
// reinstates #847 silently -- the same reason resolveBackend fails closed on a
// language it does not route.
func TestPollChildFailsClosedWithoutACompletionInstant(t *testing.T) {
	mock := &mockChildStore{
		getChildResultFn: func(ctx context.Context, runID string) (ChildOutcome, error) {
			return ChildOutcome{Completed: true, Result: `{"done":true}`}, nil
		},
		getChildCompletedAtMsFn: func(ctx context.Context, runID string) (int64, bool, error) {
			return 0, false, nil // complete, but no instant
		},
	}
	s := newTestExecSession()
	s.engine.childWfStore = mock
	buf := make([]byte, 256)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	res := s.PollChild(ctx, nil, "run-1", 0, uint32(len(buf)))
	var pr struct {
		Status string `json:"status"`
		Error  string `json:"error,omitempty"`
	}
	if err := json.Unmarshal(buf[:res>>32], &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pr.Status != "failed" || !strings.Contains(pr.Error, "no completion timestamp") {
		t.Errorf("expected a failed status naming the missing timestamp, got %q / %q", pr.Status, pr.Error)
	}
}
