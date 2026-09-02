package main

// IMPROVEMENT-PLAN 3.15: the HTTP half of the allowed_signals writer.
//
// GET/PUT /api/workflows/:id/allowed-signals is what makes
// docs/reference/worker-config.md's instruction -- "add \"*\" (wildcard) to
// allowed_signals" -- an operation an operator can actually perform. The store
// method is covered on three dialects in engine/allowed_signals_writer_test.go;
// what is tested here is the routing, the body handling, and the mapping of
// engine.ErrWorkflowNotFound onto a status code.

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

func TestPutAllowedSignalsReachesTheStore(t *testing.T) {
	ms := &mockStore{}
	api := newTestAPIServer(ms)

	req := httptest.NewRequest(http.MethodPut, "/api/workflows/wf-1/allowed-signals",
		strings.NewReader(`{"allowed_signals":["billing-service","*"]}`))
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	if ms.setAllowedSignalCallersID != "wf-1" {
		t.Errorf("store was asked about %q, want wf-1", ms.setAllowedSignalCallersID)
	}
	got := ms.setAllowedSignalCallers
	if len(got) != 2 || got[0] != "billing-service" || got[1] != "*" {
		t.Errorf("store received %v, want [billing-service *]", got)
	}
}

// TestPutAllowedSignalsEmptyListClearsRatherThanNoOps — sending an empty array
// has to reach the store as an empty list, which is how a grant is revoked. If
// the handler treated "no entries" as "nothing to do" there would be no way to
// take a caller's access away short of deleting the workflow.
func TestPutAllowedSignalsEmptyListClearsRatherThanNoOps(t *testing.T) {
	ms := &mockStore{}
	ms.setAllowedSignalCallersID = "untouched"
	api := newTestAPIServer(ms)

	req := httptest.NewRequest(http.MethodPut, "/api/workflows/wf-2/allowed-signals",
		strings.NewReader(`{"allowed_signals":[]}`))
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	if ms.setAllowedSignalCallersID != "wf-2" {
		t.Error("an empty list did not reach the store, so a grant cannot be revoked")
	}
	if len(ms.setAllowedSignalCallers) != 0 {
		t.Errorf("store received %v, want an empty list", ms.setAllowedSignalCallers)
	}
}

// TestPutAllowedSignalsUnknownWorkflowIs404 — the store cannot distinguish "no
// such workflow" from "another tenant's workflow" on purpose, and neither can
// the API. Both are 404. A 403 here would tell a caller that an id exists.
func TestPutAllowedSignalsUnknownWorkflowIs404(t *testing.T) {
	ms := &mockStore{}
	ms.setAllowedSignalCallersFn = func(_ context.Context, _ string, _ []string) error {
		return engine.ErrWorkflowNotFound
	}
	api := newTestAPIServer(ms)

	req := httptest.NewRequest(http.MethodPut, "/api/workflows/nope/allowed-signals",
		strings.NewReader(`{"allowed_signals":["*"]}`))
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, req)

	if rec.Code != 404 {
		t.Errorf("status %d, want 404 for a workflow this tenant cannot see; body %s",
			rec.Code, rec.Body.String())
	}
}

func TestPutAllowedSignalsStoreErrorIs500(t *testing.T) {
	ms := &mockStore{}
	ms.setAllowedSignalCallersFn = func(_ context.Context, _ string, _ []string) error {
		return errors.New("connection reset")
	}
	api := newTestAPIServer(ms)

	req := httptest.NewRequest(http.MethodPut, "/api/workflows/wf-3/allowed-signals",
		strings.NewReader(`{"allowed_signals":["*"]}`))
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, req)

	if rec.Code != 500 {
		t.Errorf("status %d, want 500", rec.Code)
	}
}

// TestPutAllowedSignalsRejectsEmptyEntry — an empty string in the list is
// almost certainly a mistake, and a silent one: it matches no caller defName
// and is not the wildcard, so it occupies a slot in a non-empty list without
// ever permitting anybody. Refusing it is cheaper than debugging it.
func TestPutAllowedSignalsRejectsEmptyEntry(t *testing.T) {
	ms := &mockStore{}
	api := newTestAPIServer(ms)

	req := httptest.NewRequest(http.MethodPut, "/api/workflows/wf-4/allowed-signals",
		strings.NewReader(`{"allowed_signals":["billing-service",""]}`))
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, req)

	if rec.Code != 400 {
		t.Errorf("status %d, want 400", rec.Code)
	}
	if ms.setAllowedSignalCallersID != "" {
		t.Error("the rejected list still reached the store")
	}
}

func TestPutAllowedSignalsRejectsBadJSON(t *testing.T) {
	ms := &mockStore{}
	api := newTestAPIServer(ms)

	req := httptest.NewRequest(http.MethodPut, "/api/workflows/wf-5/allowed-signals",
		strings.NewReader(`{"allowed_signals":`))
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, req)

	if rec.Code != 400 {
		t.Errorf("status %d, want 400", rec.Code)
	}
}

// TestGetAllowedSignalsAlwaysReturnsAnArray — an unset column reads back as [],
// not null, so a client can tell "denies everyone" from "the field is missing"
// without special-casing. The store returns nil for both.
func TestGetAllowedSignalsAlwaysReturnsAnArray(t *testing.T) {
	ms := &mockStore{}
	ms.getAllowedSignalCallersFn = func(_ context.Context, _ string) ([]string, error) {
		return nil, nil
	}
	api := newTestAPIServer(ms)

	req := httptest.NewRequest(http.MethodGet, "/api/workflows/wf-6/allowed-signals", nil)
	rec := httptest.NewRecorder()
	api.handleWorkflows(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		AllowedSignals *[]string `json:"allowed_signals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	if body.AllowedSignals == nil {
		t.Fatalf("allowed_signals came back as JSON null, want []; body %s", rec.Body.String())
	}
	if len(*body.AllowedSignals) != 0 {
		t.Errorf("allowed_signals is %v, want empty", *body.AllowedSignals)
	}
}

// TestAllowedSignalsRejectsOtherMethods keeps the route honest: POST and DELETE
// fall through to the 404 arm rather than being silently treated as one of the
// two verbs the endpoint implements.
func TestAllowedSignalsRejectsOtherMethods(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodPatch} {
		method := method
		t.Run(method, func(t *testing.T) {
			ms := &mockStore{}
			api := newTestAPIServer(ms)
			req := httptest.NewRequest(method, "/api/workflows/wf-7/allowed-signals",
				strings.NewReader(`{"allowed_signals":["*"]}`))
			rec := httptest.NewRecorder()
			api.handleWorkflows(rec, req)

			if rec.Code != 404 {
				t.Errorf("status %d, want 404", rec.Code)
			}
			if ms.setAllowedSignalCallersID != "" {
				t.Errorf("%s reached the store", method)
			}
		})
	}
}

// TestPutAllowedSignalsUsesTheCallerTenantStore is the §3.20 trap applied to a
// grant endpoint: #297's admin handlers checked ownership against the caller's
// tenant-scoped store and then ran the operation on the process-wide one, which
// was invisible because newTestAPIServer serves every tenant from a single
// mock.
//
// This is the endpoint where that would matter most -- writing the allowed
// list to the wrong tenant's store is granting a caller access on somebody
// else's workflow -- so it is asserted against a factory that hands out a
// different store per tenant, and against the store that was *opened*, not
// only the status code. A 200 from the default tenant looks exactly like a 200
// from the right one.
func TestPutAllowedSignalsUsesTheCallerTenantStore(t *testing.T) {
	api, storeA, storeB, f := twoTenantServer(t, true)

	mux := http.NewServeMux()
	registerRoutes(mux, api)

	req := asTenant(httptest.NewRequest(http.MethodPut, "/api/workflows/wf-b/allowed-signals",
		strings.NewReader(`{"allowed_signals":["*"]}`)), tenantB)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if storeA.setAllowedSignalCallersID != "" {
		t.Errorf("a request authenticated as tenant B wrote allowed_signals through tenant A's store (%q)",
			storeA.setAllowedSignalCallersID)
	}
	if storeB.setAllowedSignalCallersID != "wf-b" {
		t.Errorf("tenant B's store received %q, want wf-b", storeB.setAllowedSignalCallersID)
	}
	if len(f.opened) != 1 || f.opened[0] != tenantB {
		t.Errorf("store opened for %v, want exactly [%s]", f.opened, tenantB)
	}
}

// TestPutAllowedSignalsUnauthenticatedIsRefusedNotDefaulted — with auth on, a
// request carrying no tenant must be refused rather than served from the
// process-wide store. Asserted on the store never being reached, because the
// failure this guards against is a silent fallback, not an error.
func TestPutAllowedSignalsUnauthenticatedIsRefusedNotDefaulted(t *testing.T) {
	api, storeA, storeB, f := twoTenantServer(t, true)

	mux := http.NewServeMux()
	registerRoutes(mux, api)

	req := httptest.NewRequest(http.MethodPut, "/api/workflows/wf-x/allowed-signals",
		strings.NewReader(`{"allowed_signals":["*"]}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Fatalf("an unauthenticated grant succeeded: %s", w.Body.String())
	}
	if storeA.setAllowedSignalCallersID != "" || storeB.setAllowedSignalCallersID != "" {
		t.Error("an unauthenticated request reached a tenant store")
	}
	if len(f.opened) != 0 {
		t.Errorf("a store was opened for %v on an unauthenticated request", f.opened)
	}
}
