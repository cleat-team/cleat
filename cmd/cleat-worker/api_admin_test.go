package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminForceComplete_XConfirmRequired(t *testing.T) {
	// Enable admin API for this test.
	enabled := true
	old := enableAdminAPI
	enableAdminAPI = &enabled
	defer func() { enableAdminAPI = old }()

	ms := &mockStore{}
	api := newTestAPIServer(ms)
	mux := http.NewServeMux()
	registerRoutes(mux, api)

	body := `{"generation": 1, "result": "{}"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/instances/wf-1/force-complete", strings.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing X-Confirm, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "X-Confirm") {
		t.Errorf("expected X-Confirm error message, got: %s", w.Body.String())
	}
}

func TestAdminForceComplete_WrongXConfirm(t *testing.T) {
	enabled := true
	old := enableAdminAPI
	enableAdminAPI = &enabled
	defer func() { enableAdminAPI = old }()

	ms := &mockStore{}
	api := newTestAPIServer(ms)
	mux := http.NewServeMux()
	registerRoutes(mux, api)

	body := `{"generation": 1, "result": "{}"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/instances/wf-1/force-complete", strings.NewReader(body))
	req.Header.Set("X-Confirm", "wrong-value")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for wrong X-Confirm, got %d", w.Code)
	}
}

func TestAdminForceComplete_Success(t *testing.T) {
	enabled := true
	old := enableAdminAPI
	enableAdminAPI = &enabled
	defer func() { enableAdminAPI = old }()

	ms := &mockStore{}
	ms.adminForceCompleteFn = func(_ context.Context, workflowID string, generation int64, result string, operator string) error {
		return nil
	}
	api := newTestAPIServer(ms)
	mux := http.NewServeMux()
	registerRoutes(mux, api)

	body := `{"generation": 1, "result": "{}"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/instances/wf-1/force-complete", strings.NewReader(body))
	req.Header.Set("X-Confirm", "force-complete")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "completed" {
		t.Errorf("expected status 'completed', got %s", resp["status"])
	}
}

func TestAdminForceComplete_GenerationMismatch(t *testing.T) {
	enabled := true
	old := enableAdminAPI
	enableAdminAPI = &enabled
	defer func() { enableAdminAPI = old }()

	ms := &mockStore{}
	ms.adminForceCompleteFn = func(_ context.Context, workflowID string, generation int64, result string, operator string) error {
		return errors.New("admin force-complete: generation mismatch for workflow wf-1")
	}
	api := newTestAPIServer(ms)
	mux := http.NewServeMux()
	registerRoutes(mux, api)

	body := `{"generation": 1, "result": "{}"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/instances/wf-1/force-complete", strings.NewReader(body))
	req.Header.Set("X-Confirm", "force-complete")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["detail"] != "generation_mismatch" {
		t.Errorf("expected detail 'generation_mismatch', got %s", resp["detail"])
	}
}

func TestAdminForceComplete_NotFound(t *testing.T) {
	enabled := true
	old := enableAdminAPI
	enableAdminAPI = &enabled
	defer func() { enableAdminAPI = old }()

	ms := &mockStore{}
	ms.adminForceCompleteFn = func(_ context.Context, workflowID string, generation int64, result string, operator string) error {
		return errors.New("admin force-complete: workflow wf-999 not found")
	}
	api := newTestAPIServer(ms)
	mux := http.NewServeMux()
	registerRoutes(mux, api)

	body := `{"generation": 1, "result": "{}"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/instances/wf-999/force-complete", strings.NewReader(body))
	req.Header.Set("X-Confirm", "force-complete")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestAdminForceFail_Success(t *testing.T) {
	enabled := true
	old := enableAdminAPI
	enableAdminAPI = &enabled
	defer func() { enableAdminAPI = old }()

	ms := &mockStore{}
	ms.adminForceFailFn = func(_ context.Context, workflowID string, generation int64, errorMsg, errorCode string, operator string) error {
		return nil
	}
	api := newTestAPIServer(ms)
	mux := http.NewServeMux()
	registerRoutes(mux, api)

	body := `{"generation": 1, "error_message": "boom", "error_code": "ERR"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/instances/wf-1/force-fail", strings.NewReader(body))
	req.Header.Set("X-Confirm", "force-fail")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "failed" {
		t.Errorf("expected status 'failed', got %s", resp["status"])
	}
}

func TestAdminReReplay_Success(t *testing.T) {
	enabled := true
	old := enableAdminAPI
	enableAdminAPI = &enabled
	defer func() { enableAdminAPI = old }()

	ms := &mockStore{}
	ms.adminReReplayFn = func(_ context.Context, workflowID string, generation int64, operator string) error {
		return nil
	}
	api := newTestAPIServer(ms)
	mux := http.NewServeMux()
	registerRoutes(mux, api)

	body := `{"generation": 1}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/instances/wf-1/re-replay", strings.NewReader(body))
	req.Header.Set("X-Confirm", "re-replay")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "queued_for_replay" {
		t.Errorf("expected status 'queued_for_replay', got %s", resp["status"])
	}
}

func TestAdminRoutes_DisabledByDefault(t *testing.T) {
	ms := &mockStore{}
	api := newTestAPIServer(ms)
	mux := http.NewServeMux()
	registerRoutes(mux, api)

	body := `{"generation": 1}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/instances/wf-1/force-complete", strings.NewReader(body))
	req.Header.Set("X-Confirm", "force-complete")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 when admin API disabled, got %d", w.Code)
	}
}

func TestAdminRoutes_BadJSON(t *testing.T) {
	enabled := true
	old := enableAdminAPI
	enableAdminAPI = &enabled
	defer func() { enableAdminAPI = old }()

	ms := &mockStore{}
	api := newTestAPIServer(ms)
	mux := http.NewServeMux()
	registerRoutes(mux, api)

	body := `not json`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/instances/wf-1/force-complete", strings.NewReader(body))
	req.Header.Set("X-Confirm", "force-complete")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad JSON, got %d", w.Code)
	}
}
