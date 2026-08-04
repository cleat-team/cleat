package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/google/uuid"
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

// --------------------------------------------------------------------------
// Tenant ownership on the admin API
// --------------------------------------------------------------------------

const (
	ownerTenant     = "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"
	attackerTenant  = "bbbbbbbb-bbbb-4bbb-bbbb-bbbbbbbbbbbb"
	adminTargetWfID = "wf-owned-by-a"
)

// adminAction describes one destructive admin endpoint and how to arm the mock
// store so that reaching the store is observable.
type adminAction struct {
	path    string
	confirm string
	body    string
	arm     func(ms *mockStore, reached *bool)
}

func adminActions() []adminAction {
	return []adminAction{
		{
			path: "force-complete", confirm: "force-complete", body: `{"generation":1,"result":"{}"}`,
			arm: func(ms *mockStore, reached *bool) {
				ms.adminForceCompleteFn = func(context.Context, string, int64, string, string) error {
					*reached = true
					return nil
				}
			},
		},
		{
			path: "force-fail", confirm: "force-fail", body: `{"generation":1,"error_message":"x","error_code":"y"}`,
			arm: func(ms *mockStore, reached *bool) {
				ms.adminForceFailFn = func(context.Context, string, int64, string, string, string) error {
					*reached = true
					return nil
				}
			},
		},
		{
			path: "re-replay", confirm: "re-replay", body: `{"generation":1}`,
			arm: func(ms *mockStore, reached *bool) {
				ms.adminReReplayFn = func(context.Context, string, int64, string) error {
					*reached = true
					return nil
				}
			},
		},
	}
}

// adminRequest issues one admin call as callerTenant against a workflow whose
// stored owner is ownedBy (empty ownedBy means the workflow does not exist).
// It returns the response and whether the store's admin method was reached.
func adminRequest(t *testing.T, act adminAction, callerTenant, ownedBy string) (*httptest.ResponseRecorder, bool) {
	t.Helper()

	enabled := true
	old := enableAdminAPI
	enableAdminAPI = &enabled
	defer func() { enableAdminAPI = old }()

	reached := false
	ms := &mockStore{}
	act.arm(ms, &reached)
	ms.getWorkflowByIDFn = func(_ context.Context, id string) (*engine.WorkflowInstance, error) {
		if ownedBy == "" {
			return nil, nil
		}
		return &engine.WorkflowInstance{ID: id, TenantID: ownedBy}, nil
	}

	api := newTestAPIServer(ms)
	mux := http.NewServeMux()
	registerRoutes(mux, api)

	req := httptest.NewRequest(http.MethodPost,
		"/api/admin/instances/"+adminTargetWfID+"/"+act.path, strings.NewReader(act.body))
	req.Header.Set("X-Confirm", act.confirm)
	req = req.WithContext(auth.WithTenantID(req.Context(), uuid.MustParse(callerTenant)))

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w, reached
}

// TestAdminRoutesRejectCrossTenantTarget is the regression test for the
// ownership gap described in IMPROVEMENT-PLAN 1.7.
//
// The admin store methods take no tenant parameter, so the HTTP layer is the
// only place this can be enforced. Without the check, any authenticated caller
// who knows a workflow ID can force-complete, force-fail or re-replay another
// tenant's workflow.
//
// Asserting the status code alone would be too weak: a handler could return 404
// after having already applied the operation. The store must not be reached.
func TestAdminRoutesRejectCrossTenantTarget(t *testing.T) {
	for _, act := range adminActions() {
		t.Run(act.path, func(t *testing.T) {
			w, reached := adminRequest(t, act, attackerTenant, ownerTenant)

			if w.Code != http.StatusNotFound {
				t.Errorf("caller from tenant B got %d acting on tenant A's workflow, want 404 -- "+
					"one tenant can operate on another's workflows", w.Code)
			}
			if reached {
				t.Errorf("the store's admin method was reached for another tenant's workflow; "+
					"the operation was applied regardless of the %d returned", w.Code)
			}
			// 403 would confirm the workflow exists.
			if strings.Contains(strings.ToLower(w.Body.String()), "forbidden") {
				t.Errorf("response distinguishes 'not yours' from 'not found', which makes this "+
					"endpoint an oracle for valid workflow IDs: %s", w.Body.String())
			}
		})
	}
}

// TestAdminRoutesAllowOwnTenantTarget is the other half: the check must not
// refuse the legitimate caller. Without it this test passes trivially, which is
// why it is paired with the one above rather than standing alone.
func TestAdminRoutesAllowOwnTenantTarget(t *testing.T) {
	for _, act := range adminActions() {
		t.Run(act.path, func(t *testing.T) {
			w, reached := adminRequest(t, act, ownerTenant, ownerTenant)

			if w.Code != http.StatusOK {
				t.Errorf("owning tenant got %d, want 200: %s", w.Code, w.Body.String())
			}
			if !reached {
				t.Error("the store's admin method was not reached for the owning tenant -- " +
					"the ownership check is refusing the legitimate caller")
			}
		})
	}
}

// TestAdminRoutesRejectUnknownWorkflow pins the response for a workflow that
// does not exist to the same 404 as one owned by somebody else.
func TestAdminRoutesRejectUnknownWorkflow(t *testing.T) {
	for _, act := range adminActions() {
		t.Run(act.path, func(t *testing.T) {
			w, reached := adminRequest(t, act, ownerTenant, "")

			if w.Code != http.StatusNotFound {
				t.Errorf("got %d for a workflow that does not exist, want 404", w.Code)
			}
			if reached {
				t.Error("the store's admin method was reached for a workflow that does not exist")
			}
		})
	}
}
