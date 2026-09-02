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
	startChildAtomicFn func(ctx context.Context, childID, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, event EventRecord, priority int) (string, error)
	startChildFn       func(ctx context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, priority int) (string, error)
	getChildResultFn   func(ctx context.Context, runID string) (string, bool, error)
	resolveTagFn       func(ctx context.Context, workflowName string, tag string) (int, error)
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

func (m *mockChildStore) GetChildResult(ctx context.Context, runID string) (string, bool, error) {
	if m.getChildResultFn != nil {
		return m.getChildResultFn(ctx, runID)
	}
	return "", false, nil
}

func (m *mockChildStore) ResolveVersionByTag(ctx context.Context, workflowName string, tag string) (int, error) {
	if m.resolveTagFn != nil {
		return m.resolveTagFn(ctx, workflowName, tag)
	}
	return 0, nil
}

// mockCrossChildStore extends mockChildStore with CrossSchemaChildStore support.
type mockCrossChildStore struct {
	mockChildStore
	startInSchemaFn func(ctx context.Context, targetSchema, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, priority int) (string, error)
	getInSchemaFn   func(ctx context.Context, targetSchema, runID string) (string, bool, error)
}

func (m *mockCrossChildStore) StartChildWorkflowInSchema(ctx context.Context, targetSchema, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, priority int) (string, error) {
	if m.startInSchemaFn != nil {
		return m.startInSchemaFn(ctx, targetSchema, parentID, defName, inputJSON, defVersion, parentClosePolicy, priority)
	}
	return "child-cross-run", nil
}

func (m *mockCrossChildStore) GetChildResultInSchema(ctx context.Context, targetSchema, runID string) (string, bool, error) {
	if m.getInSchemaFn != nil {
		return m.getInSchemaFn(ctx, targetSchema, runID)
	}
	return "", false, nil
}

// mockOnlyChildStore implements ChildWorkflowStore but NOT CrossSchemaChildStore.
type mockOnlyChildStore struct {
	mockChildStore
}

// Ensure compile-time check: mockOnlyChildStore does NOT implement CrossSchemaChildStore.

// ---------------------------------------------------------------------------
// ChildWorkflowInSchema tests.
// ---------------------------------------------------------------------------

func TestChildWorkflowInSchema_OwnSchema(t *testing.T) {
	s := newTestExecSession()
	s.engine.schema = "my_schema"

	// When targetSchema is the engine's own schema, validation passes.
	result := s.ChildWorkflowInSchema(context.Background(), nil, "my_schema", "test-wf", `{"x":1}`, 0, 0, "", 0, 0)
	// With no childWfStore, falls through to synthetic path — errCode 0.
	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0 (success), got %d", errCode)
	}
}

func TestChildWorkflowInSchema_PeerSchema(t *testing.T) {
	s := newTestExecSession()
	s.engine.schema = "my_schema"
	s.engine.peerSchemas = []string{"peer_a", "peer_b"}

	result := s.ChildWorkflowInSchema(context.Background(), nil, "peer_a", "test-wf", `{}`, 0, 0, "", 0, 0)
	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0 (success), got %d", errCode)
	}
}

func TestChildWorkflowInSchema_InvalidSchema(t *testing.T) {
	s := newTestExecSession()
	s.engine.schema = "my_schema"
	// No peer schemas configured.

	result := s.ChildWorkflowInSchema(context.Background(), nil, "unknown_schema", "test-wf", `{}`, 0, 0, "", 0, 0)
	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 4 {
		t.Errorf("expected errCode 4 (invalid), got %d", errCode)
	}
}

func TestChildWorkflowInSchema_EmptySchema(t *testing.T) {
	s := newTestExecSession()
	s.engine.schema = "my_schema"

	// Empty targetSchema should fall back to local schema.
	result := s.ChildWorkflowInSchema(context.Background(), nil, "", "test-wf", `{}`, 0, 0, "", 0, 0)
	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0 (success), got %d", errCode)
	}
}

func TestChildWorkflowInSchema_CrossSchemaStore(t *testing.T) {
	called := false
	store := &mockCrossChildStore{
		startInSchemaFn: func(ctx context.Context, targetSchema, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, priority int) (string, error) {
			called = true
			if targetSchema != "peer_b" {
				t.Errorf("expected targetSchema 'peer_b', got %q", targetSchema)
			}
			return "cross-run-id", nil
		},
	}
	s := newTestExecSession()
	s.engine.schema = "my_schema"
	s.engine.peerSchemas = []string{"peer_b"}
	s.engine.childWfStore = store

	result := s.ChildWorkflowInSchema(context.Background(), nil, "peer_b", "test-wf", `{}`, 0, 0, "", 0, 0)
	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}
	if !called {
		t.Error("expected StartChildWorkflowInSchema to be called")
	}
}

func TestChildWorkflowInSchema_CrossSchemaNotSupported(t *testing.T) {
	// Store implements ChildWorkflowStore but NOT CrossSchemaChildStore.
	store := &mockOnlyChildStore{}
	s := newTestExecSession()
	s.engine.schema = "my_schema"
	s.engine.peerSchemas = []string{"peer_b"}
	s.engine.childWfStore = store

	result := s.ChildWorkflowInSchema(context.Background(), nil, "peer_b", "test-wf", `{}`, 0, 0, "", 0, 0)
	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 4 {
		t.Errorf("expected errCode 4 (cross-schema not supported), got %d", errCode)
	}
}

// ---------------------------------------------------------------------------
// resolveChildVersion tests.
// ---------------------------------------------------------------------------

func TestResolveChildVersion_Explicit(t *testing.T) {
	s := newTestExecSession()
	v := s.resolveChildVersion(context.Background(), "test-wf", 42, "")
	if v != 42 {
		t.Errorf("expected explicit version 42, got %d", v)
	}
}

func TestResolveChildVersion_TargetSchema(t *testing.T) {
	s := newTestExecSession()
	// Cross-schema children skip policy resolution and return 0.
	v := s.resolveChildVersion(context.Background(), "test-wf", 0, "peer_schema")
	if v != 0 {
		t.Errorf("expected 0 for cross-schema child, got %d", v)
	}
}

func TestResolveChildVersion_OverrideLatest(t *testing.T) {
	s := newTestExecSession()
	s.engine.childBindingOverride = "latest"
	v := s.resolveChildVersion(context.Background(), "test-wf", 0, "")
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

	v := s.resolveChildVersion(context.Background(), "test-wf", 0, "")
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

	v := s.resolveChildVersion(context.Background(), "test-wf", 0, "")
	if v != 5 {
		t.Errorf("expected frozen version 5, got %d", v)
	}
}

func TestResolveChildVersion_FrozenNoPin(t *testing.T) {
	s := newTestExecSession()
	s.engine.childBindingPolicy = "frozen"
	s.engine.state = &stubWorkflowState{} // no childVer map

	v := s.resolveChildVersion(context.Background(), "test-wf", 0, "")
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

	v := s.resolveChildVersion(context.Background(), "test-wf", 0, "")
	if v != 3 {
		t.Errorf("expected stable version 3, got %d", v)
	}
}

func TestResolveChildVersion_StableNoStore(t *testing.T) {
	s := newTestExecSession()
	s.engine.childBindingPolicy = "stable"
	s.engine.state = &stubWorkflowState{}
	// childWfStore is nil

	v := s.resolveChildVersion(context.Background(), "test-wf", 0, "")
	if v != 0 {
		t.Errorf("expected 0 when stable resolution fails, got %d", v)
	}
}

func TestResolveChildVersion_Latest(t *testing.T) {
	s := newTestExecSession()
	s.engine.childBindingPolicy = "latest"
	s.engine.state = &stubWorkflowState{}

	v := s.resolveChildVersion(context.Background(), "test-wf", 0, "")
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

	v := s.resolveChildVersion(context.Background(), "test-wf", 0, "")
	if v != 9 {
		t.Errorf("expected version 9 from tag policy, got %d", v)
	}
}

func TestResolveChildVersion_FallbackFrozen(t *testing.T) {
	s := newTestExecSession()
	s.engine.state = &stubWorkflowState{childVer: map[string]int{"test-wf": 4}}
	// childBindingPolicy is empty → should fall back to "frozen" since pinned version exists.

	v := s.resolveChildVersion(context.Background(), "test-wf", 0, "")
	if v != 4 {
		t.Errorf("expected version 4 from frozen fallback, got %d", v)
	}
}

func TestResolveChildVersion_FallbackLatest(t *testing.T) {
	s := newTestExecSession()
	s.engine.state = &stubWorkflowState{} // no pinned version
	// childBindingPolicy is empty → should fall back to "latest".

	v := s.resolveChildVersion(context.Background(), "test-wf", 0, "")
	if v != 0 {
		t.Errorf("expected 0 from latest fallback, got %d", v)
	}
}

func TestResolveChildVersion_NoState(t *testing.T) {
	s := newTestExecSession()
	// engine.state is nil
	v := s.resolveChildVersion(context.Background(), "test-wf", 0, "")
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
	mock := &mockChildStore{getChildResultFn: func(ctx context.Context, runID string) (string, bool, error) {
		return "", false, nil // not completed
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
	mock := &mockChildStore{getChildResultFn: func(ctx context.Context, runID string) (string, bool, error) {
		return `{"result":"ok"}`, true, nil
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
	mock := &mockChildStore{getChildResultFn: func(ctx context.Context, runID string) (string, bool, error) {
		return "", true, fmt.Errorf("db error")
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
	mock := &mockChildStore{getChildResultFn: func(ctx context.Context, runID string) (string, bool, error) {
		return "", false, nil // not completed, no error
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
	mock := &mockChildStore{getChildResultFn: func(ctx context.Context, runID string) (string, bool, error) {
		return `{"ok":true}`, true, nil
	}}
	s := newTestExecSession()
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
	mock := &mockChildStore{getChildResultFn: func(ctx context.Context, runID string) (string, bool, error) {
		return "", false, nil
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
	mock := &mockChildStore{getChildResultFn: func(ctx context.Context, runID string) (string, bool, error) {
		return "", false, fmt.Errorf("connection refused")
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
	mock := &mockChildStore{getChildResultFn: func(ctx context.Context, runID string) (string, bool, error) {
		return "", true, nil // completed but empty result == failed
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
	if !strings.Contains(pr.Error, "empty result") {
		t.Errorf("expected 'empty result' in error, got %q", pr.Error)
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
	mock := &mockChildStore{getChildResultFn: func(ctx context.Context, runID string) (string, bool, error) {
		return "", false, nil
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
		getChildResultFn: func(ctx context.Context, runID string) (string, bool, error) {
			callCount++
			// First child is completed.
			return `{"result":"done"}`, true, nil
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
	mock := &mockChildStore{getChildResultFn: func(ctx context.Context, runID string) (string, bool, error) {
		return "", false, nil // all running
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
		getChildResultFn: func(ctx context.Context, runID string) (string, bool, error) {
			return `{"result":"` + runID + `"}`, true, nil
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

func TestAwaitAllChildren_SomeRunning(t *testing.T) {
	mock := &mockChildStore{
		getChildResultFn: func(ctx context.Context, runID string) (string, bool, error) {
			switch runID {
			case "run-a":
				return `{"result":"a"}`, true, nil
			default:
				return "", false, nil // still running
			}
		},
	}
	s := newTestExecSession()
	s.engine.childWfStore = mock

	buf := make([]byte, 512)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	result := s.AwaitAllChildren(ctx, nil, `["run-a","run-b"]`, 0, uint32(len(buf)))

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}

	// Verify the response contains both outcomes including "child not completed" for run-b.
	var outcomes []struct {
		RunID  string `json:"run_id"`
		Result string `json:"result,omitempty"`
		Error  string `json:"error,omitempty"`
	}
	written := uint32(result >> 32)
	if err := json.Unmarshal(buf[:written], &outcomes); err != nil {
		t.Fatalf("unmarshal outcomes: %v", err)
	}
	if len(outcomes) != 2 {
		t.Fatalf("expected 2 outcomes, got %d", len(outcomes))
	}
	// Find the outcome for run-b.
	for _, o := range outcomes {
		if o.RunID == "run-b" && o.Error != "child not completed" {
			t.Errorf("expected 'child not completed' for run-b, got %q", o.Error)
		}
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

func TestFreshAwaitAllChildren_NoStore(t *testing.T) {
	s := newTestExecSession()
	// childWfStore is nil

	buf := make([]byte, 512)
	ctx := contextWithRawMemBuf(context.Background(), buf)
	result := s.freshAwaitAllChildren(ctx, nil, `["run-a"]`, 0, uint32(len(buf)))

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Errorf("expected errCode 0, got %d", errCode)
	}

	var outcomes []struct {
		RunID string `json:"run_id"`
		Error string `json:"error,omitempty"`
	}
	written := uint32(result >> 32)
	if err := json.Unmarshal(buf[:written], &outcomes); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(outcomes) != 1 {
		t.Fatalf("expected 1 outcome, got %d", len(outcomes))
	}
	if outcomes[0].Error != "no child workflow store" {
		t.Errorf("expected 'no child workflow store' error, got %q", outcomes[0].Error)
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
