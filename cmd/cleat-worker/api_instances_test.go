package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"context"

	"github.com/cleat-team/cleat/engine"
)

func TestGetInstanceEvents_Success(t *testing.T) {
	ms := &mockStore{}
	ms.countEventHistoryFn = func(_ context.Context, workflowID string) (int, error) {
		return 2, nil
	}
	ms.loadEventHistoryPaginatedFn = func(_ context.Context, workflowID string, offset, limit int) ([]engine.EventRecord, error) {
		return []engine.EventRecord{
			{Step: 0, EventType: engine.EventTypeCall, Service: "svc", Op: "op"},
			{Step: 1, EventType: engine.EventTypeCall, Service: "svc", Op: "op2"},
		}, nil
	}
	api := newTestAPIServer(ms)
	mux := http.NewServeMux()
	registerRoutes(mux, api)

	req := httptest.NewRequest(http.MethodGet, "/api/instances/wf-1/events", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var events []engine.EventRecord
	if err := json.NewDecoder(w.Body).Decode(&events); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}

	if w.Header().Get("X-Total-Count") != "2" {
		t.Errorf("expected X-Total-Count 2, got %s", w.Header().Get("X-Total-Count"))
	}
}

func TestGetInstanceEvents_Empty(t *testing.T) {
	ms := &mockStore{}
	ms.countEventHistoryFn = func(_ context.Context, workflowID string) (int, error) {
		return 0, nil
	}
	ms.loadEventHistoryPaginatedFn = func(_ context.Context, workflowID string, offset, limit int) ([]engine.EventRecord, error) {
		return nil, nil
	}
	api := newTestAPIServer(ms)
	mux := http.NewServeMux()
	registerRoutes(mux, api)

	req := httptest.NewRequest(http.MethodGet, "/api/instances/wf-1/events", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var events []engine.EventRecord
	if err := json.NewDecoder(w.Body).Decode(&events); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events, got %d", len(events))
	}
}

func TestGetInstanceState_Success(t *testing.T) {
	ms := &mockStore{}
	api := newTestAPIServer(ms)
	mux := http.NewServeMux()
	registerRoutes(mux, api)

	req := httptest.NewRequest(http.MethodGet, "/api/instances/wf-1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	// mockStore.getWorkflowByIDFn isn't set, so it returns nil.
	// The handler should return 404 for nil workflow.
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestGetInstanceState_StatePath(t *testing.T) {
	ms := &mockStore{}
	ms.getWorkflowByIDFn = func(_ context.Context, id string) (*engine.WorkflowInstance, error) {
		return &engine.WorkflowInstance{ID: id, Status: "running"}, nil
	}
	api := newTestAPIServer(ms)
	mux := http.NewServeMux()
	registerRoutes(mux, api)

	req := httptest.NewRequest(http.MethodGet, "/api/instances/wf-1/state", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for /state path, got %d", w.Code)
	}
}

func TestGetInstanceState_MethodNotAllowed(t *testing.T) {
	ms := &mockStore{}
	api := newTestAPIServer(ms)
	mux := http.NewServeMux()
	registerRoutes(mux, api)

	req := httptest.NewRequest(http.MethodPost, "/api/instances/wf-1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}
