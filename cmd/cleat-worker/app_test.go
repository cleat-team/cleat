package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

func TestHandleHealthz(t *testing.T) {
	ms := &mockStore{}
	api := newTestAPIServer(ms)
	mux := http.NewServeMux()
	registerRoutes(mux, api)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var body map[string]bool
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !body["ok"] {
		t.Error("expected ok: true")
	}
}

func TestHandleDeadLettersList_Success(t *testing.T) {
	ms := &mockStore{}
	ms.listWorkflowsFn = func(_ context.Context, filter engine.WorkflowFilter) ([]engine.WorkflowInstance, error) {
		return []engine.WorkflowInstance{
			{ID: "dl-1", DefName: "test", DefVersion: 1, Status: "dead_lettered"},
			{ID: "dl-2", DefName: "test", DefVersion: 1, Status: "dead_lettered"},
		}, nil
	}
	api := newTestAPIServer(ms)
	mux := http.NewServeMux()
	registerRoutes(mux, api)

	req := httptest.NewRequest(http.MethodGet, "/api/dead-letters", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var workflows []engine.WorkflowInstance
	if err := json.NewDecoder(w.Body).Decode(&workflows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(workflows) != 2 {
		t.Fatalf("expected 2 workflows, got %d", len(workflows))
	}
	if workflows[0].ID != "dl-1" || workflows[1].ID != "dl-2" {
		t.Errorf("unexpected workflow IDs: %v", workflows)
	}
}

func TestHandleDeadLettersList_MethodNotAllowed(t *testing.T) {
	ms := &mockStore{}
	api := newTestAPIServer(ms)
	mux := http.NewServeMux()
	registerRoutes(mux, api)

	req := httptest.NewRequest(http.MethodPost, "/api/dead-letters", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestHandleDeadLettersList_StoreError(t *testing.T) {
	ms := &mockStore{}
	ms.listWorkflowsFn = func(_ context.Context, filter engine.WorkflowFilter) ([]engine.WorkflowInstance, error) {
		return nil, errors.New("db connection lost")
	}
	api := newTestAPIServer(ms)
	mux := http.NewServeMux()
	registerRoutes(mux, api)

	req := httptest.NewRequest(http.MethodGet, "/api/dead-letters", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}

	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(body["error"], "db connection lost") {
		t.Errorf("expected error about db connection lost, got %v", body)
	}
}

func TestHandleDeadLettersList_Empty(t *testing.T) {
	ms := &mockStore{}
	ms.listWorkflowsFn = func(_ context.Context, filter engine.WorkflowFilter) ([]engine.WorkflowInstance, error) {
		return nil, nil
	}
	api := newTestAPIServer(ms)
	mux := http.NewServeMux()
	registerRoutes(mux, api)

	req := httptest.NewRequest(http.MethodGet, "/api/dead-letters", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var workflows []engine.WorkflowInstance
	if err := json.NewDecoder(w.Body).Decode(&workflows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(workflows) != 0 {
		t.Errorf("expected empty list, got %d items", len(workflows))
	}
}

func TestHandleDeadLetterReprocess_Success(t *testing.T) {
	ms := &mockStore{}
	ms.getWorkflowByIDFn = func(_ context.Context, id string) (*engine.WorkflowInstance, error) {
		return &engine.WorkflowInstance{
			ID: id, DefName: "test-def", DefVersion: 1, Status: "dead_lettered",
			Input: json.RawMessage(`{"key":"value"}`),
		}, nil
	}
	ms.listVersionsFn = func(_ context.Context, defName string) ([]int, error) {
		return []int{3}, nil
	}
	ms.startNewRunFn = func(_ context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey, tenantID string, priority int) (string, bool, error) {
		return "new-run-abc", false, nil
	}
	api := newTestAPIServer(ms)
	mux := http.NewServeMux()
	registerRoutes(mux, api)

	req := httptest.NewRequest(http.MethodPost, "/api/dead-letters/wf-1/reprocess", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d", w.Code)
	}

	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["id"] != "new-run-abc" {
		t.Errorf("expected id 'new-run-abc', got %q", body["id"])
	}
}

func TestHandleDeadLetterReprocess_AlreadyExisted(t *testing.T) {
	ms := &mockStore{}
	ms.getWorkflowByIDFn = func(_ context.Context, id string) (*engine.WorkflowInstance, error) {
		return &engine.WorkflowInstance{
			ID: id, DefName: "test-def", DefVersion: 1, Status: "dead_lettered",
			Input: json.RawMessage(`{}`),
		}, nil
	}
	ms.listVersionsFn = func(_ context.Context, defName string) ([]int, error) {
		return []int{1}, nil
	}
	ms.startNewRunFn = func(_ context.Context, runID, defName string, defVersion int, input json.RawMessage, idempotencyKey, tenantID string, priority int) (string, bool, error) {
		return "existing-run", true, nil
	}
	api := newTestAPIServer(ms)
	mux := http.NewServeMux()
	registerRoutes(mux, api)

	req := httptest.NewRequest(http.MethodPost, "/api/dead-letters/wf-1/reprocess", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["already_started"] != "true" {
		t.Errorf("expected already_started=true, got %v", body)
	}
	if body["workflow_id"] != "existing-run" {
		t.Errorf("expected workflow_id 'existing-run', got %q", body["workflow_id"])
	}
}

func TestHandleDeadLetterReprocess_NotFound(t *testing.T) {
	ms := &mockStore{}
	ms.getWorkflowByIDFn = func(_ context.Context, id string) (*engine.WorkflowInstance, error) {
		return nil, nil
	}
	api := newTestAPIServer(ms)
	mux := http.NewServeMux()
	registerRoutes(mux, api)

	req := httptest.NewRequest(http.MethodPost, "/api/dead-letters/wf-nonexistent/reprocess", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleDeadLetterReprocess_NotDeadLettered(t *testing.T) {
	ms := &mockStore{}
	ms.getWorkflowByIDFn = func(_ context.Context, id string) (*engine.WorkflowInstance, error) {
		return &engine.WorkflowInstance{
			ID: id, DefName: "test-def", DefVersion: 1, Status: "running",
		}, nil
	}
	api := newTestAPIServer(ms)
	mux := http.NewServeMux()
	registerRoutes(mux, api)

	req := httptest.NewRequest(http.MethodPost, "/api/dead-letters/wf-1/reprocess", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}

	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(body["error"], "not dead-lettered") {
		t.Errorf("expected error about not dead-lettered, got %v", body)
	}
}

func TestHandleDeadLetterTerminate_Success(t *testing.T) {
	ms := &mockStore{}
	api := newTestAPIServer(ms)

	// The default mockStore.TerminateWorkflow returns nil, so success path works.
	mux := http.NewServeMux()
	registerRoutes(mux, api)

	req := httptest.NewRequest(http.MethodPost, "/api/dead-letters/wf-1/terminate", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "terminated" {
		t.Errorf("expected status=terminated, got %v", body)
	}
}

func TestHandleDeadLetterTerminate_WithReason(t *testing.T) {
	var capturedReason string
	ms := &mockStore{
		terminateWorkflowFn: func(ctx context.Context, workflowID, reason string) error {
			capturedReason = reason
			return nil
		},
	}
	api := newTestAPIServer(ms)
	mux := http.NewServeMux()
	registerRoutes(mux, api)

	body := strings.NewReader(`{"reason":"testing termination"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/dead-letters/wf-1/terminate", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if capturedReason != "testing termination" {
		t.Errorf("expected reason 'testing termination', got %q", capturedReason)
	}
}

func TestHandleDeadLetters_NotFound(t *testing.T) {
	ms := &mockStore{}
	api := newTestAPIServer(ms)
	mux := http.NewServeMux()
	registerRoutes(mux, api)

	// An unrecognized action returns 404.
	req := httptest.NewRequest(http.MethodGet, "/api/dead-letters/wf-1/nonexistent", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestStartAPIServer_EmptyAddr(t *testing.T) {
	cfg := &Config{}
	w := newTestWorker(&mockStore{})
	// Should return immediately without starting a server (no panic).
	StartAPIServer(cfg, w, nil, nil, nil, nil)
}

func TestStartAPIServer_WithNilMux(t *testing.T) {
	ms := &mockStore{}
	w := newTestWorker(ms)
	defer w.cancel()

	cfg := &Config{APIAddr: "localhost:0"}
	// Should create a new mux and start a server in background without panic.
	StartAPIServer(cfg, w, nil, nil, nil, nil)
}
