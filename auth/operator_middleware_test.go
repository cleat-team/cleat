package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// operatorChain runs one request through OperatorMiddleware over a handler that
// records whether it was reached and what context it was reached with.
//
// Reached matters as much as the status code: the confinement's whole job is
// that a request which is refused never reaches the handler that would have
// acted on it, and a test that only read the status would pass against a
// middleware that wrote 403 AFTER calling next.
type operatorChainResult struct {
	status     int
	reached    bool
	ctxOp      Operator
	ctxHasOp   bool
	ctxTenant  string
	ctxHasTent bool
}

func runOperatorChain(t *testing.T, store OperatorResolver, method, path, key string) operatorChainResult {
	t.Helper()

	var got operatorChainResult
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.reached = true
		if op, ok := OperatorFromContext(r.Context()); ok {
			got.ctxOp, got.ctxHasOp = op, true
		}
		if tid, ok := TenantIDFromContext(r.Context()); ok {
			got.ctxTenant, got.ctxHasTent = tid.String(), true
		}
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(method, path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	OperatorMiddleware(store)(inner).ServeHTTP(rec, req)

	got.status = rec.Code
	return got
}

// TestAnOperatorKeyReachesTheAdminSurface is the positive case: without it every
// refusal below would pass against a middleware that refused everything.
func TestAnOperatorKeyReachesTheAdminSurface(t *testing.T) {
	want := Operator{KeyID: "6f5c1e9a-2b1f-4a3c-9d0e-77c8b5a4e012", Description: "on-call laptop"}
	got := runOperatorChain(t, &fakeOperatorResolver{op: want}, http.MethodPost, "/api/admin/drain", GenerateOperatorKey())

	if got.status != http.StatusOK {
		t.Errorf("an operator key on /api/admin/drain got %d, want 200", got.status)
	}
	if !got.reached {
		t.Fatal("the request never reached the handler, so nothing about the handler's context was measured")
	}
	if !got.ctxHasOp || got.ctxOp != want {
		t.Errorf("the handler saw operator %+v (present=%v), want %+v", got.ctxOp, got.ctxHasOp, want)
	}
	// The issue's second requirement, as a precondition: an operator request
	// carries no tenant, so no tenant-scoped path can treat it as one. A tenant
	// in this context would mean the request had been handed a tenant it never
	// proved, which is the failure the confinement exists to prevent.
	if got.ctxHasTent {
		t.Errorf("an operator request reached the handler with tenant %s in its context", got.ctxTenant)
	}
}

// TestAnOperatorKeyIsRefusedOnATenantRoute is the confinement.
func TestAnOperatorKeyIsRefusedOnATenantRoute(t *testing.T) {
	for _, path := range []string{
		"/api/workflows",
		"/api/workflows/abc",
		"/api/schedules",
		"/api/instances/abc",
		"/api/openapi.json",
	} {
		got := runOperatorChain(t, &fakeOperatorResolver{op: Operator{KeyID: "x"}}, http.MethodGet, path, GenerateOperatorKey())

		if got.status != http.StatusForbidden {
			t.Errorf("an operator key on %s got %d, want 403 (a valid credential on a route it may not use)", path, got.status)
		}
		if got.reached {
			t.Errorf("an operator key on %s reached the handler; the refusal must happen before the handler runs", path)
		}
	}
}

// TestAnOperatorKeyOutsideTheAPIIsPassedThrough covers the boundary of the
// confinement. /livez and its neighbours are answered without any credential,
// so presenting one must not turn a working endpoint into a refusal -- an
// operator key that made those worse than no key would be a trap, not a policy.
func TestAnOperatorKeyOutsideTheAPIIsPassedThrough(t *testing.T) {
	for _, path := range []string{"/livez", "/metrics", "/healthz", "/readyz"} {
		got := runOperatorChain(t, &fakeOperatorResolver{op: Operator{KeyID: "x"}}, http.MethodGet, path, GenerateOperatorKey())

		if got.status != http.StatusOK || !got.reached {
			t.Errorf("%s with an operator key got %d (reached=%v); it is answered without a credential and must stay that way",
				path, got.status, got.reached)
		}
	}
}

// TestAnOperatorKeyThatDoesNotResolveIsUnauthorized — and note this is checked
// even on an admin path, so a revoked credential cannot ride in on the route it
// is allowed for.
func TestAnOperatorKeyThatDoesNotResolveIsUnauthorized(t *testing.T) {
	store := &fakeOperatorResolver{err: errors.New("sql: no rows in result set")}

	for _, path := range []string{"/api/admin/health", "/api/workflows"} {
		got := runOperatorChain(t, store, http.MethodGet, path, GenerateOperatorKey())

		if got.status != http.StatusUnauthorized {
			t.Errorf("an unresolvable operator key on %s got %d, want 401", path, got.status)
		}
		if got.reached {
			t.Errorf("an unresolvable operator key on %s reached the handler", path)
		}
	}
}

// TestATenantKeyAndAKeylessRequestAreUntouched is the additive half. This
// middleware goes on the outside of the whole auth chain, so anything it got
// wrong about a request it does not own would break authentication for every
// tenant key in the deployment -- including the common case of no key at all,
// which must reach the chain below to be answered by it (401 from
// MiddlewareWithMux, or the public-path handling, or nothing at all when auth
// is off).
func TestATenantKeyAndAKeylessRequestAreUntouched(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
	}{
		{"a tenant key", GenerateAPIKey()},
		{"a foreign bearer token", "some-other-token"},
		{"no key at all", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeOperatorResolver{op: Operator{KeyID: "should-not-be-used"}}
			// Deliberately on an admin path: even there, a request this
			// middleware does not own must be passed through rather than
			// answered, because the chain below is what decides.
			got := runOperatorChain(t, store, http.MethodGet, "/api/admin/health", tc.key)

			if !got.reached {
				t.Errorf("%s never reached the chain below; OperatorMiddleware answered a request it does not own", tc.name)
			}
			if got.ctxHasOp {
				t.Errorf("%s came back with an operator in its context", tc.name)
			}
			if store.calls != 0 {
				t.Errorf("%s caused %d operator lookup(s); only an operator-shaped key should", tc.name, store.calls)
			}
		})
	}
}
