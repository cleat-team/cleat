package main

// cleat#2169, part (3): an operator credential may act on a tenant that is not
// its own, over the tenant-named admin routes.
//
// WHY THESE ASSERT ON THE FACTORY AND NOT ON THE STATUS CODE. The capability is
// "the operation reached a store opened for the NAMED tenant", and a 200 cannot
// say that. The pre-3.20 admin path returned 200 having run against the default
// tenant's scope, which is indistinguishable from success by status alone --
// api_admin_scope_test.go exists for the same reason. fakeStoreFactory.opened
// records the tenant every OpenStore call named, so the assertion here is about
// which tenant's data was touched.
//
// The default store is armed separately in every test below, because reaching it
// is the specific failure: it is the process-wide store opened at boot against
// the default tenant, and an operator request that lands there has taken the path
// storeFor was written to close.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
)

// asOperator marks a request as carrying an operator credential -- the identity
// auth.OperatorMiddleware installs for a cleat_op_ key. A non-empty KeyID is
// what makes it a credential rather than the zero Operator.
func asOperator(req *http.Request) *http.Request {
	return req.WithContext(auth.WithOperator(req.Context(),
		auth.Operator{KeyID: "8d264f39-9c52-4623-aa1d-258c7bcc34d7"}))
}

func adminTenantPath(tenant, id, action string) string {
	return "/api/admin/tenants/" + tenant + "/instances/" + id + "/" + action
}

// armForceComplete makes a mock record that the force-complete operation reached
// it. That operation is the right one to watch: it is destructive, addressed by
// workflow id, and carries no tenant parameter of its own -- so the layer under
// test is the only thing that can decide which tenant's row it touches.
func armForceComplete(ms *mockStore, reached *bool) {
	ms.adminForceCompleteFn = func(context.Context, string, int64, string, string) error {
		*reached = true
		return nil
	}
}

// ownsWorkflow arms a store to report that it holds the workflow, so that a
// failure to reach it is a failure of THIS code and not of a lookup that found
// nothing.
func ownsWorkflow(ms *mockStore, id, tenant string) {
	ms.getWorkflowByIDFn = func(context.Context, string) (*engine.WorkflowInstance, error) {
		return &engine.WorkflowInstance{ID: id, TenantID: tenant}, nil
	}
}

func doAdminPost(t *testing.T, api *apiServer, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	registerRoutes(mux, api)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func forceCompleteRequest(path string, as opMarker) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path,
		strings.NewReader(`{"generation":3,"result":"{}"}`))
	req.Header.Set("X-Confirm", "force-complete")
	return as(req)
}

// opMarker is the identity to install on a request: asOperator for a credential,
// asTenant for a key.
type opMarker func(*http.Request) *http.Request

// TestAnOperatorActsOnANamedTenant is the headline capability: a request
// carrying an operator credential and naming tenant B acts on tenant B's
// workflow. It checks all three candidates for where the operation landed --
// B (correct), A (the wrong tenant), and the process-wide default store (the
// old path) -- because a test that only checked B would pass if the operation
// ran against MORE than one.
func TestAnOperatorActsOnANamedTenant(t *testing.T) {
	enableAdminRoutes(t)
	api, storeA, storeB, f := twoTenantServer(t, true)

	defaultStore, ok := api.store.(*mockStore)
	if !ok {
		t.Fatalf("process-wide store is %T, want *mockStore", api.store)
	}

	var reachedA, reachedB, reachedDefault bool
	armForceComplete(storeA, &reachedA)
	armForceComplete(storeB, &reachedB)
	armForceComplete(defaultStore, &reachedDefault)
	ownsWorkflow(storeA, "wf-b", tenantA)
	ownsWorkflow(storeB, "wf-b", tenantB)

	w := doAdminPost(t, api, forceCompleteRequest(adminTenantPath(tenantB, "wf-b", "force-complete"), asOperator))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if !reachedB {
		t.Errorf("the operation did not reach tenant B's store; it must act on the tenant named in the URL")
	}
	if reachedA {
		t.Errorf("the operation reached tenant A's store, which the URL did not name")
	}
	if reachedDefault {
		t.Errorf("the operation reached the process-wide default store -- that is the path storeFor exists to close")
	}
	// The factory is the authority on which tenant was scoped: exactly one open,
	// for the named tenant.
	if len(f.opened) != 1 || f.opened[0] != tenantB {
		t.Errorf("OpenStore was called for %v, want exactly [%s]; the named tenant is the only scope this route may use",
			f.opened, tenantB)
	}
}

// TestAnOperatorIsStillRefusedOnTheIdOnlyRoute pins the half of cleat#2169 that
// #2982 landed, so that widening the operator's reach cannot quietly widen it
// everywhere. An operator has no tenant, so the id-only route -- which runs
// against the CALLER's tenant -- has nothing to scope to and must refuse.
//
// This is the confinement the issue is deliberately not changing: the
// cross-tenant capability lives in a URL that names the tenant, not in making
// every existing admin route ambiently cross-tenant.
func TestAnOperatorIsStillRefusedOnTheIdOnlyRoute(t *testing.T) {
	enableAdminRoutes(t)
	api, storeA, storeB, f := twoTenantServer(t, true)
	_ = storeA
	_ = storeB

	w := doAdminPost(t, api, forceCompleteRequest("/api/admin/instances/wf-a/force-complete", asOperator))

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401; an operator on the id-only route has no tenant to scope to", w.Code)
	}
	if len(f.opened) != 0 {
		t.Errorf("a store was opened for %v on a route that should have refused before opening anything", f.opened)
	}
}

// TestANamedTenantNeedsAnIdentityToNameIt covers the branch that had no test at
// all until a reviewer probed it — and it is the more interesting half of the
// confinement, because it is where this route and the id-only one DISAGREE.
//
// The id-only route admits a request with no identity when --require-auth=false.
// That is safe THERE because the store it falls back to is s.store: ONE
// CONSTANT scope that no request can redirect. This route opens a store for
// whatever the URL NAMES, so the same fall-through let an unauthenticated caller
// reach ANY tenant — measured, before the refusal existed: naming tenant B
// opened B's store and force-completed its workflow, audited as
// operator=unknown.
//
// The second half is the control. Without it, a blanket refusal of
// identity-less requests would satisfy the first half while silently changing
// the id-only route's documented posture.
func TestANamedTenantNeedsAnIdentityToNameIt(t *testing.T) {
	enableAdminRoutes(t)
	// requireAuth=false: the deployment in which the id-only route serves an
	// identity-less request at all.
	api, _, storeB, f := twoTenantServer(t, false)
	defaultStore, ok := api.store.(*mockStore)
	if !ok {
		t.Fatalf("process-wide store is %T, want *mockStore", api.store)
	}

	var reachedB, reachedDefault bool
	armForceComplete(storeB, &reachedB)
	armForceComplete(defaultStore, &reachedDefault)
	ownsWorkflow(storeB, "wf-b", tenantB)
	ownsWorkflow(defaultStore, "wf-b", engine.DefaultTenantUUID)

	noIdentity := func(r *http.Request) *http.Request { return r }

	t.Run("a named tenant is refused", func(t *testing.T) {
		w := doAdminPost(t, api, forceCompleteRequest(
			adminTenantPath(tenantB, "wf-b", "force-complete"), noIdentity))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401; naming a tenant requires an identity", w.Code)
		}
		if reachedB {
			t.Errorf("an unauthenticated request reached tenant B's store by naming it in the URL")
		}
		if len(f.opened) != 0 {
			t.Errorf("a store was opened for %v by a request with no identity", f.opened)
		}
	})

	t.Run("CONTROL: the id-only route keeps its posture", func(t *testing.T) {
		w := doAdminPost(t, api, forceCompleteRequest("/api/admin/instances/wf-b/force-complete", noIdentity))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; the id-only route is unchanged by this PR; body = %s",
				w.Code, w.Body.String())
		}
		if !reachedDefault {
			t.Errorf("the id-only route did not use the process-wide default store")
		}
		if reachedB {
			t.Errorf("the id-only route reached a per-tenant store for a tenant it was not given")
		}
		if len(f.opened) != 0 {
			t.Errorf("the id-only route opened %v; it uses the constant default scope", f.opened)
		}
	})
}

// TestATenantKeyMayNameOnlyItsOwnTenant: the named-tenant form is available to a
// tenant key, but only for its own tenant. Naming another tenant is a 404 and
// not a 403, for the reason callerOwnsTarget gives -- a 403 confirms that the
// named tenant exists and that the named workflow does, which is an oracle.
func TestATenantKeyMayNameOnlyItsOwnTenant(t *testing.T) {
	enableAdminRoutes(t)
	api, storeA, storeB, f := twoTenantServer(t, true)

	var reachedA, reachedB bool
	armForceComplete(storeA, &reachedA)
	armForceComplete(storeB, &reachedB)
	ownsWorkflow(storeB, "wf-b", tenantB)

	w := doAdminPost(t, api, forceCompleteRequest(adminTenantPath(tenantB, "wf-b", "force-complete"),
		func(r *http.Request) *http.Request { return asTenant(r, tenantA) }))

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; tenant A naming tenant B must not be told which half failed", w.Code)
	}
	if reachedB {
		t.Errorf("tenant A reached tenant B's store by naming it in the URL")
	}
	if reachedA {
		t.Errorf("tenant A's own store was reached on a request that named tenant B")
	}
	if len(f.opened) != 0 {
		t.Errorf("a store was opened for %v; the refusal must come before any store is scoped", f.opened)
	}
}

// TestATenantKeyMayNameItsOwnTenant is the other side of the row above, and
// without it a rule that refused every tenant key would pass the test above.
func TestATenantKeyMayNameItsOwnTenant(t *testing.T) {
	enableAdminRoutes(t)
	api, storeA, storeB, f := twoTenantServer(t, true)
	_ = storeB

	var reachedA bool
	armForceComplete(storeA, &reachedA)
	ownsWorkflow(storeA, "wf-a", tenantA)

	w := doAdminPost(t, api, forceCompleteRequest(adminTenantPath(tenantA, "wf-a", "force-complete"),
		func(r *http.Request) *http.Request { return asTenant(r, tenantA) }))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if !reachedA {
		t.Errorf("the operation did not reach tenant A's store")
	}
	if len(f.opened) != 1 || f.opened[0] != tenantA {
		t.Errorf("OpenStore was called for %v, want exactly [%s]", f.opened, tenantA)
	}
}

// TestANamedTenantThatDoesNotOwnTheWorkflowGets404 is the security property that
// makes the tenant-in-the-path design safe: the SCOPE is the check. The store is
// opened for the named tenant, so a workflow that tenant does not own is not
// visible, and there is no second comparison that could disagree with it.
//
// It also pins the deliberate 404 rather than 403: "no such workflow" and
// "someone else's workflow" must be the same answer.
func TestANamedTenantThatDoesNotOwnTheWorkflowGets404(t *testing.T) {
	enableAdminRoutes(t)
	api, _, storeB, f := twoTenantServer(t, true)

	var reachedB bool
	armForceComplete(storeB, &reachedB)
	// The named tenant's store cannot see this id at all -- which is what a
	// real store does for another tenant's row, whether that row exists or not.
	storeB.getWorkflowByIDFn = func(context.Context, string) (*engine.WorkflowInstance, error) {
		return nil, nil
	}

	w := doAdminPost(t, api, forceCompleteRequest(adminTenantPath(tenantB, "wf-of-someone-else", "force-complete"), asOperator))

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
	if reachedB {
		t.Errorf("the operation ran against a workflow the named tenant's store could not see")
	}
	if len(f.opened) != 1 || f.opened[0] != tenantB {
		t.Errorf("OpenStore was called for %v, want [%s] -- it is scoped first, then asked", f.opened, tenantB)
	}
}

// TestTheNamedTenantIsCanonicalisedBeforeItReachesAStore pins the half of the
// UUID validation that checking alone does not give you.
//
// The sink this protects is real and codeql found it on this PR:
// engine/mysql_store.go builds "CREATE DATABASE IF NOT EXISTS `"+dbName+"`" out
// of the tenant, by concatenation, which is safe only because every previous
// caller handed it a UUID it had already parsed. A route where the tenant
// arrives as an HTTP string must therefore not forward THAT string, however
// well-checked: uuid.Parse accepts spellings (braces, a urn: prefix, bare
// 32-hex, either case) that are all the same tenant and none of which is the
// string the store should receive.
//
// So this asserts the value, not the status: every accepted spelling must reach
// OpenStore as the one canonical lowercase form.
func TestTheNamedTenantIsCanonicalisedBeforeItReachesAStore(t *testing.T) {
	// A tenant whose UUID contains LETTERS, which the package's two fixtures do
	// not: they are "1111…" and "2222…", so strings.ToUpper of either is the
	// same string and a case subtest built on them measures nothing. Found by
	// mutation -- with the all-digit fixtures the upper-case case passed even
	// when the caller's spelling was forwarded unchanged, i.e. the one subtest
	// covering the case hazard was vacuous.
	const tenantWithLetters = "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d"

	for _, spelling := range []struct{ name, given string }{
		{"canonical", tenantWithLetters},
		{"upper case", strings.ToUpper(tenantWithLetters)},
		{"braced", "{" + tenantWithLetters + "}"},
		{"urn", "urn:uuid:" + tenantWithLetters},
		// The fourth form uuid.Parse accepts, and the one easiest to miss: the
		// hyphens removed entirely. All four are the same tenant.
		{"bare 32-hex", strings.ReplaceAll(tenantWithLetters, "-", "")},
	} {
		t.Run(spelling.name, func(t *testing.T) {
			enableAdminRoutes(t)
			api, _, _, f := twoTenantServer(t, true)
			own := &mockStore{}
			ownsWorkflow(own, "wf-x", tenantWithLetters)
			f.byTenant[tenantWithLetters] = own

			w := doAdminPost(t, api, forceCompleteRequest(
				adminTenantPath(spelling.given, "wf-x", "force-complete"), asOperator))

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
			}
			if len(f.opened) != 1 {
				t.Fatalf("OpenStore called %d times, want 1: %v", len(f.opened), f.opened)
			}
			if f.opened[0] != tenantWithLetters {
				t.Errorf("OpenStore was called with %q, want the canonical form %q -- the value that "+
					"reaches the store must be the parsed one, not the caller's spelling of it",
					f.opened[0], tenantWithLetters)
			}
		})
	}
}

// TestANamedTenantMustBeAUUID: the tenant string is bound to cleat.tenant_id on
// PostgreSQL, where a non-UUID is a cast error raised by the policy rather than
// a clean refusal. Left unvalidated that is a 500 for a malformed request, which
// reads as a cleat bug -- so the route refuses it as a 400 first.
func TestANamedTenantMustBeAUUID(t *testing.T) {
	enableAdminRoutes(t)
	api, _, _, f := twoTenantServer(t, true)

	w := doAdminPost(t, api, forceCompleteRequest(adminTenantPath("not-a-uuid", "wf-a", "force-complete"), asOperator))

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; a malformed tenant must not reach a store", w.Code)
	}
	if len(f.opened) != 0 {
		t.Errorf("a store was opened for %v on a malformed tenant", f.opened)
	}
}
