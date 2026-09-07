package main

// cleat#889: a deprecated version cannot be started over the API.
//
// ValidateVersion ("exists and not deprecated") had no caller on the HTTP start
// path. Deprecation was enforced for CHILD workflows and for plugins, but not
// for the one path an operator deprecating a version is actually trying to
// close -- so a version could be marked deprecated and still started by any API
// caller.
//
// Found by the store-reachability gate (#876), triaged in #889, and wired
// rather than deleted by the repository owner's decision.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStartingADeprecatedVersionIsRefused(t *testing.T) {
	started := false
	ms := &mockStore{
		listVersionsFn: func(_ context.Context, _ string) ([]int, error) {
			return []int{3}, nil
		},
		validateVersionFn: func(_ context.Context, _ string, _ int) (bool, error) {
			return false, nil // deprecated
		},
		startNewRunFn: func(_ context.Context, _, _ string, _ int, _ json.RawMessage, _, _ string, _ int) (string, bool, error) {
			started = true
			return "should-not-happen", false, nil
		},
	}
	api := newTestAPIServer(ms)
	rec := httptest.NewRecorder()
	api.handleStartWorkflow(rec, httptest.NewRequest(
		http.MethodPost, "/api/workflows/wf/start", strings.NewReader(`{"input":{}}`)), "wf")

	if rec.Code != 409 {
		t.Errorf("starting a deprecated version answered %d, want 409: %s\n\n"+
			"201 means the deprecation was ignored, which is cleat#889 -- the "+
			"store method existed and nothing on this path called it.",
			rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "deprecated") {
		t.Errorf("the refusal does not say why, so a caller cannot act on it: %s",
			rec.Body.String())
	}
	if started {
		t.Error("the run was started anyway; the check must refuse BEFORE StartNewRun, " +
			"or a deprecated version executes and the refusal is cosmetic")
	}
}

// TestStartingALiveVersionIsUnaffected is the control. Without it, a check that
// refused everything would satisfy the test above perfectly.
func TestStartingALiveVersionIsUnaffected(t *testing.T) {
	ms := &mockStore{
		listVersionsFn: func(_ context.Context, _ string) ([]int, error) {
			return []int{3}, nil
		},
		validateVersionFn: func(_ context.Context, _ string, _ int) (bool, error) {
			return true, nil
		},
	}
	api := newTestAPIServer(ms)
	rec := httptest.NewRecorder()
	api.handleStartWorkflow(rec, httptest.NewRequest(
		http.MethodPost, "/api/workflows/wf/start", strings.NewReader(`{"input":{}}`)), "wf")

	if rec.Code != 201 {
		t.Errorf("starting a live version answered %d, want 201: %s",
			rec.Code, rec.Body.String())
	}
}
