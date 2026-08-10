package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/google/uuid"
)

// fakeStoreFactory hands out a different store per tenant.
//
// That is the whole point: the defect under test is not that the store fails to
// filter, it is that the HTTP layer asked the wrong store in the first place. A
// factory keyed by tenant makes "which tenant's data did this request read"
// directly observable, without needing a database to enforce RLS.
type fakeStoreFactory struct {
	byTenant map[string]engine.WorkflowStore

	// fallback serves any tenant not in byTenant, so the many existing tests
	// that construct an apiServer with a single mock store keep working.
	fallback engine.WorkflowStore

	// opened records the tenant IDs OpenStore was called with, in order.
	opened []string
}

func (f *fakeStoreFactory) OpenStore(_ context.Context, tenantID string, _ ...string) (engine.WorkflowStore, io.Closer, error) {
	f.opened = append(f.opened, tenantID)
	if s, ok := f.byTenant[tenantID]; ok {
		return s, nopCloser{}, nil
	}
	if f.fallback != nil {
		return f.fallback, nopCloser{}, nil
	}
	return nil, nil, fmt.Errorf("fakeStoreFactory: no store for tenant %s", tenantID)
}

func (f *fakeStoreFactory) DriverName() string      { return "fake" }
func (f *fakeStoreFactory) Dialect() engine.Dialect { return engine.DialectPostgres }

const (
	tenantA = "11111111-1111-1111-1111-111111111111"
	tenantB = "22222222-2222-2222-2222-222222222222"
)

// twoTenantServer builds an apiServer whose tenant A and tenant B stores are
// distinguishable, and returns both stores so a test can assert which one a
// request actually reached.
func twoTenantServer(t *testing.T, requireAuth bool) (*apiServer, *mockStore, *mockStore, *fakeStoreFactory) {
	t.Helper()

	storeA := &mockStore{}
	storeB := &mockStore{}
	// The process-wide store is a third, distinct store standing in for the
	// default tenant. Any request that reaches it has taken the old path.
	defaultStore := &mockStore{}

	f := &fakeStoreFactory{byTenant: map[string]engine.WorkflowStore{
		tenantA: storeA,
		tenantB: storeB,
	}}

	api := &apiServer{
		store:       defaultStore,
		worker:      newTestWorker(defaultStore),
		maxBodySize: 1 << 20,
		factory:     f,
		requireAuth: requireAuth,
	}
	return api, storeA, storeB, f
}

func asTenant(req *http.Request, tenant string) *http.Request {
	return req.WithContext(auth.WithTenantID(req.Context(), uuid.MustParse(tenant)))
}

// TestRequestIsServedFromCallerTenantStore is the regression test for the core
// of IMPROVEMENT-PLAN 1.7: callers authenticate per-tenant and were then all
// served from one hardcoded scope.
//
// It asserts the tenant the store was opened for, not just the response body.
// A status-code assertion would be too weak here -- the old code returned 200
// with the default tenant's data, which looks identical to success.
func TestRequestIsServedFromCallerTenantStore(t *testing.T) {
	api, storeA, storeB, f := twoTenantServer(t, true)

	listedFrom := ""
	storeA.listWorkflowsFn = func(_ context.Context, _ engine.WorkflowFilter) ([]engine.WorkflowInstance, error) {
		listedFrom = "A"
		return []engine.WorkflowInstance{{ID: "wf-a", TenantID: tenantA}}, nil
	}
	storeB.listWorkflowsFn = func(_ context.Context, _ engine.WorkflowFilter) ([]engine.WorkflowInstance, error) {
		listedFrom = "B"
		return []engine.WorkflowInstance{{ID: "wf-b", TenantID: tenantB}}, nil
	}

	mux := http.NewServeMux()
	registerRoutes(mux, api)

	req := asTenant(httptest.NewRequest(http.MethodGet, "/api/workflows", nil), tenantB)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if listedFrom != "B" {
		t.Errorf("request authenticated as tenant B listed from store %q, want B", listedFrom)
	}
	if len(f.opened) != 1 || f.opened[0] != tenantB {
		t.Errorf("store opened for %v, want exactly [%s]", f.opened, tenantB)
	}
	if strings.Contains(w.Body.String(), "wf-a") {
		t.Errorf("tenant B's response contained tenant A's workflow: %s", w.Body.String())
	}
}

// TestUnauthenticatedRequestIsRefusedNotDefaulted asserts the fail-closed
// behaviour. The bug was a silent fallback to the default tenant; the fix must
// not reintroduce it under a different name, so this asserts the store is never
// reached rather than only checking the status code.
func TestUnauthenticatedRequestIsRefusedNotDefaulted(t *testing.T) {
	api, _, _, f := twoTenantServer(t, true)

	defaultStore := api.store.(*mockStore)
	reached := false
	defaultStore.listWorkflowsFn = func(_ context.Context, _ engine.WorkflowFilter) ([]engine.WorkflowInstance, error) {
		reached = true
		return nil, nil
	}

	mux := http.NewServeMux()
	registerRoutes(mux, api)

	req := httptest.NewRequest(http.MethodGet, "/api/workflows", nil) // no tenant
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401; body = %s", w.Code, w.Body.String())
	}
	if reached {
		t.Error("unauthenticated request reached the default-tenant store")
	}
	if len(f.opened) != 0 {
		t.Errorf("factory opened stores %v, want none", f.opened)
	}
}

// TestAuthOffStillServesDefaultTenant guards the other side of the trade. The
// fix must not turn single-tenant and local deployments, which run without
// --require-auth, into 401s.
func TestAuthOffStillServesDefaultTenant(t *testing.T) {
	api, _, _, _ := twoTenantServer(t, false)

	defaultStore := api.store.(*mockStore)
	reached := false
	defaultStore.listWorkflowsFn = func(_ context.Context, _ engine.WorkflowFilter) ([]engine.WorkflowInstance, error) {
		reached = true
		return []engine.WorkflowInstance{}, nil
	}

	mux := http.NewServeMux()
	registerRoutes(mux, api)

	req := httptest.NewRequest(http.MethodGet, "/api/workflows", nil) // no tenant
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if !reached {
		t.Error("with auth off, the default-tenant store should still serve the request")
	}
}

// TestStartWorkflowIgnoresBodyTenantID covers the cross-tenant *write*.
//
// Scoping the store is not sufficient for this route: StartNewRun takes the
// tenant as an argument, which came straight from the request body. An
// authenticated caller could name any tenant and have the run created there.
func TestStartWorkflowIgnoresBodyTenantID(t *testing.T) {
	api, storeA, _, _ := twoTenantServer(t, true)

	storeA.listVersionsFn = func(_ context.Context, _ string) ([]int, error) {
		return []int{1}, nil
	}
	gotTenant := ""
	storeA.startNewRunFn = func(_ context.Context, _, _ string, _ int, _ json.RawMessage, _, tenantID string, _ int) (string, bool, error) {
		gotTenant = tenantID
		return "run-1", false, nil
	}

	mux := http.NewServeMux()
	registerRoutes(mux, api)

	// Authenticated as A, asking for B.
	body := `{"input":{},"tenant_id":"` + tenantB + `"}`
	req := asTenant(httptest.NewRequest(http.MethodPost, "/api/workflows/wf/start", strings.NewReader(body)), tenantA)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403; body = %s", w.Code, w.Body.String())
	}
	if gotTenant != "" {
		t.Errorf("StartNewRun was called with tenant %q; it should not have been called at all", gotTenant)
	}
}

// TestStartWorkflowUsesAuthenticatedTenant is the companion to the test above:
// with no tenant_id in the body, the run must still be attributed to the
// caller's tenant rather than to the default one.
func TestStartWorkflowUsesAuthenticatedTenant(t *testing.T) {
	api, storeA, _, _ := twoTenantServer(t, true)

	storeA.listVersionsFn = func(_ context.Context, _ string) ([]int, error) {
		return []int{1}, nil
	}
	gotTenant := ""
	storeA.startNewRunFn = func(_ context.Context, _, _ string, _ int, _ json.RawMessage, _, tenantID string, _ int) (string, bool, error) {
		gotTenant = tenantID
		return "run-1", false, nil
	}

	mux := http.NewServeMux()
	registerRoutes(mux, api)

	req := asTenant(httptest.NewRequest(http.MethodPost, "/api/workflows/wf/start", strings.NewReader(`{"input":{}}`)), tenantA)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if gotTenant != tenantA {
		t.Errorf("StartNewRun tenant = %q, want %q (the authenticated tenant); body = %s",
			gotTenant, tenantA, w.Body.String())
	}
	if gotTenant == engine.DefaultTenantUUID {
		t.Error("run was attributed to the default tenant, which is the defect 1.7 describes")
	}
}
