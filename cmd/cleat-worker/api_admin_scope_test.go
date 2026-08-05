package main

// IMPROVEMENT-PLAN.md 3.20. The admin handlers checked ownership against the
// caller's tenant-scoped store and then applied the operation to s.store, the
// process-wide one. Nothing caught it: while the store methods were stubs both
// stores answered "not implemented yet", and newTestAPIServer serves every
// tenant from a single mock, so the two stores are the same object in every
// other test in this package.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// enableAdminRoutes turns the admin API on for the duration of a test.
func enableAdminRoutes(t *testing.T) {
	t.Helper()
	enabled := true
	old := enableAdminAPI
	enableAdminAPI = &enabled
	t.Cleanup(func() { enableAdminAPI = old })
}

// TestAdminForceResolveUsesCallerTenantStore asserts which store the operation
// reached, not the status code. The status code cannot tell the two apart: the
// old path returned 200 having force-resolved against the default tenant's
// scope, which looks exactly like success.
func TestAdminForceResolveUsesCallerTenantStore(t *testing.T) {
	for _, tc := range []struct {
		path, confirm, body string
		arm                 func(*mockStore, *bool)
	}{
		{
			path: "force-complete", confirm: "force-complete", body: `{"generation":3,"result":"{}"}`,
			arm: func(ms *mockStore, reached *bool) {
				ms.adminForceCompleteFn = func(context.Context, string, int64, string, string) error {
					*reached = true
					return nil
				}
			},
		},
		{
			path: "force-fail", confirm: "force-fail", body: `{"generation":3,"error_message":"x","error_code":"y"}`,
			arm: func(ms *mockStore, reached *bool) {
				ms.adminForceFailFn = func(context.Context, string, int64, string, string, string) error {
					*reached = true
					return nil
				}
			},
		},
	} {
		t.Run(tc.path, func(t *testing.T) {
			enableAdminRoutes(t)
			api, storeA, storeB, f := twoTenantServer(t, true)

			defaultStore, ok := api.store.(*mockStore)
			if !ok {
				t.Fatalf("process-wide store is %T, want *mockStore", api.store)
			}

			var reachedA, reachedB, reachedDefault bool
			tc.arm(storeA, &reachedA)
			tc.arm(storeB, &reachedB)
			tc.arm(defaultStore, &reachedDefault)

			storeA.getWorkflowByIDFn = func(_ context.Context, id string) (*engine.WorkflowInstance, error) {
				return &engine.WorkflowInstance{ID: id, TenantID: tenantA}, nil
			}

			mux := http.NewServeMux()
			registerRoutes(mux, api)

			req := asTenant(httptest.NewRequest(http.MethodPost,
				"/api/admin/instances/wf-owned-by-a/"+tc.path, strings.NewReader(tc.body)), tenantA)
			req.Header.Set("X-Confirm", tc.confirm)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
			}
			if !reachedA {
				t.Error("the operation did not reach the caller's tenant-scoped store")
			}
			if reachedDefault {
				t.Error("the operation was applied to the process-wide store: a force-resolve authenticated " +
					"as one tenant ran against the default tenant's scope")
			}
			if reachedB {
				t.Error("the operation reached another tenant's store")
			}
			if len(f.opened) != 1 || f.opened[0] != tenantA {
				t.Errorf("stores opened for %v, want exactly [%s]", f.opened, tenantA)
			}
		})
	}
}

// TestAdminOpNotImplementedIs501 pins the difference between an operation that
// failed and one that was never built. Every admin route answered 500 for the
// latter, which reads as a server fault the caller should retry.
func TestAdminOpNotImplementedIs501(t *testing.T) {
	enableAdminRoutes(t)

	ms := &mockStore{}
	ms.adminReReplayFn = func(context.Context, string, int64, string) error {
		return fmt.Errorf("admin re-replay: %w", engine.ErrAdminOpNotImplemented)
	}
	api := newTestAPIServer(ms)
	mux := http.NewServeMux()
	registerRoutes(mux, api)

	req := httptest.NewRequest(http.MethodPost,
		"/api/admin/instances/wf-1/re-replay", strings.NewReader(`{"generation":1}`))
	req.Header.Set("X-Confirm", "re-replay")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501 for an admin operation the store does not implement", w.Code)
	}
}

// TestAdminForceCompleteRejectsNonJSONResult covers the other half of the
// mapping: an operator-supplied result that is not JSON is a bad request, not
// a 500 carrying whichever driver noticed first.
func TestAdminForceCompleteRejectsNonJSONResult(t *testing.T) {
	enableAdminRoutes(t)

	reached := false
	ms := &mockStore{}
	ms.adminForceCompleteFn = func(context.Context, string, int64, string, string) error {
		reached = true
		return nil
	}
	api := newTestAPIServer(ms)
	mux := http.NewServeMux()
	registerRoutes(mux, api)

	req := httptest.NewRequest(http.MethodPost,
		"/api/admin/instances/wf-1/force-complete", strings.NewReader(`{"generation":1,"result":"not json"}`))
	req.Header.Set("X-Confirm", "force-complete")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a result that is not JSON: %s", w.Code, w.Body.String())
	}
	if reached {
		t.Error("the store was reached with a result that cannot be stored in a JSON column")
	}
}

// TestAdminOpNotImplementedSentinelIsWrapped is the guard on the 501 mapping
// itself: it matches with errors.Is, so a store that returns the sentinel
// unwrapped, or wrapped several times, still answers 501.
func TestAdminOpNotImplementedSentinelIsWrapped(t *testing.T) {
	wrapped := fmt.Errorf("re-replay: %w", fmt.Errorf("admin re-replay: %w", engine.ErrAdminOpNotImplemented))
	if !errors.Is(wrapped, engine.ErrAdminOpNotImplemented) {
		t.Fatal("the sentinel does not survive wrapping, so handleAdminOpError cannot recognise it")
	}
}

// TestParseWriteAheadIntentOps covers the flag parsing for 1.4 phase D. The
// empty cases matter: a trailing comma or a value split across a YAML line
// would otherwise declare an operation named "", which would never match a
// real call and would make the guarantee silently inert.
func TestParseWriteAheadIntentOps(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"one", "billing.charge", []string{"billing.charge"}},
		{"several", "billing.charge,mail.send", []string{"billing.charge", "mail.send"}},
		{"trailing comma and spaces", " billing.charge , mail.send , ", []string{"billing.charge", "mail.send"}},
		{"only separators", " , , ", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			got := parseWriteAheadIntentOps(&in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("got %q, want %q", got, tc.want)
				}
			}
		})
	}
	if got := parseWriteAheadIntentOps(nil); got != nil {
		t.Errorf("nil flag returned %q, want nil", got)
	}
}
