package main

// cleat#3577. With --tenant-resolver=header:<name>, a request carrying a valid key for tenant A and
// `<name>: <tenant B>` must act on tenant A. It does because the header wrapper is the OUTERMOST layer: it
// runs first, and the tenant-key middleware inside it then sets the tenant from the KEY and shadows the
// header. Move the wrapper inside the chain and it runs last and shadows the key instead -- for /api/keys,
// minting, listing and revoking ANOTHER tenant's keys.
//
// That held only by the order of two blocks in main(), and nothing could see it: the wrapper was not part of
// composeAuthChain. composeRequestChain now carries both, main.go calls it, and these tests go through it.
//
// THE KNOWN-POSITIVES COME FIRST because the assertions are only evidence if the instrument can see the bad
// order. "A acted, not B" is what a chain with NO header wrapper at all also gives, so three rows establish
// that this harness would have noticed: the wrapper really does take effect when nothing else names a tenant;
// and the wrong order, built here by hand, makes the same request act on B.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
)

const headerMode = "header:X-Tenant"

// listedFrom wires both tenants' stores to record which one a GET /api/workflows opened.
func listedFrom(f chainFixture) *string {
	var from string
	f.storeA.listWorkflowsFn = func(context.Context, engine.WorkflowFilter) ([]engine.WorkflowInstance, error) {
		from += "A"
		return nil, nil
	}
	f.storeB.listWorkflowsFn = func(context.Context, engine.WorkflowFilter) ([]engine.WorkflowInstance, error) {
		from += "B"
		return nil, nil
	}
	return &from
}

// only reports whether from names stores of tenant want and no other. It is a SET question, not a count: the
// list handler opens its store more than once per request, so "exactly A" cannot mean one letter.
func only(from, want string) bool {
	return from != "" && strings.Trim(from, want) == ""
}

func doWith(h http.Handler, method, path, key, body string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Add(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestTheHeaderTenantResolverIsInstalledAndTheWrongOrderIsVisible(t *testing.T) {
	f := buildChainResolving(t, false, nil, headerMode)
	from := listedFrom(f)
	mux := http.NewServeMux()
	registerRoutes(mux, f.api)

	// KNOWN-POSITIVE 1: with authentication off the header is the only identity there is, and it works.
	// Without this row, every "A acted, not B" below would also be what a wrapper that was never
	// installed gives.
	*from = ""
	noAuth := composeRequestChain(mux, mux, nil, headerMode)
	if w := doWith(noAuth, http.MethodGet, "/api/workflows", "", ``, "X-Tenant", tenantB); w.Code != http.StatusOK || !only(*from, "B") {
		t.Fatalf("header %s with authentication off = %d, store opened %q; want 200 and only B. The header wrapper is not taking effect, so nothing below is evidence",
			tenantB, w.Code, *from)
	}

	// KNOWN-POSITIVE 2: the WRONG order -- the wrapper inside the chain -- lets the header win over a
	// valid key. If this row acts on A, the harness cannot tell the orders apart.
	*from = ""
	wrong := composeAuthChain(tenantResolverMiddleware(headerMode)(mux), mux, authChain{
		Tenants: f.keys, Operators: f.keys, Domains: noDomains{asked: new(bool)},
	})
	if w := doWith(wrong, http.MethodGet, "/api/workflows", f.tenantAKey, ``, "X-Tenant", tenantB); w.Code != http.StatusOK || !only(*from, "B") {
		t.Fatalf("the wrong order (wrapper inside the chain) with key A and header B = %d, store opened %q; want 200 and only B. The harness would not see a regression",
			w.Code, *from)
	}
}

func TestAKeyForOneTenantBeatsAHeaderNamingAnotherThroughTheChainAsMainComposesIt(t *testing.T) {
	f := buildChainResolving(t, false, nil, headerMode)
	from := listedFrom(f)
	fk := &fakeKeys{}
	f.api.keys = fk

	// THE TENANT-PLANE DATA ROUTE: key A, header B.
	if w := doWith(f.handler, http.MethodGet, "/api/workflows", f.tenantAKey, ``, "X-Tenant", tenantB); w.Code != http.StatusOK || !only(*from, "A") {
		t.Errorf("GET /api/workflows with key A and header %s = %d, store opened %q; want 200 and only A", tenantB, w.Code, *from)
	}

	// THE KEY ROUTES, the case the issue is about: mint, list and revoke each act on the key's tenant.
	for _, tc := range []struct{ name, method, path, body string }{
		{"mint", http.MethodPost, "/api/keys", `{}`},
		{"list", http.MethodGet, "/api/keys", ``},
		{"revoke", http.MethodDelete, "/api/keys/" + uuid.NewString(), ``},
	} {
		fk.gotTenant = uuid.Nil
		w := doWith(f.handler, tc.method, tc.path, f.tenantAKey, tc.body, "X-Tenant", tenantB)
		if fk.gotTenant.String() != tenantA {
			t.Errorf("%s with key A and header %s = %d, the store was asked for tenant %s; want tenant A. A header must not choose whose keys these are",
				tc.name, tenantB, w.Code, fk.gotTenant)
		}
	}
	if fk.mintCalls != 1 || fk.listCalls != 1 || fk.revokeCalls != 1 {
		t.Errorf("store calls mint/list/revoke = %d/%d/%d, want 1/1/1 (each ran once, each for A)", fk.mintCalls, fk.listCalls, fk.revokeCalls)
	}

	// VARIANTS OF THE ALLOWED CASE. The header named twice, and a header that is not a uuid: the key wins
	// and the request is served, neither is a way to a different tenant.
	*from = ""
	doWith(f.handler, http.MethodGet, "/api/workflows", f.tenantAKey, ``, "X-Tenant", tenantB, "X-Tenant", tenantB)
	doWith(f.handler, http.MethodGet, "/api/workflows", f.tenantAKey, ``, "X-Tenant", "not-a-uuid")
	if !only(*from, "A") {
		t.Errorf("a doubled header and a malformed header, with key A, opened %q; want only A", *from)
	}

	// NOTHING TO IMPERSONATE WITH: a header alone is not an identity while authentication is on.
	*from = ""
	fk.listCalls = 0
	if w := doWith(f.handler, http.MethodGet, "/api/workflows", "", ``, "X-Tenant", tenantB); w.Code != http.StatusUnauthorized || *from != "" {
		t.Errorf("header B and no credential on /api/workflows = %d, store opened %q; want 401 and none", w.Code, *from)
	}
	if w := doWith(f.handler, http.MethodGet, "/api/keys", "", ``, "X-Tenant", tenantB); w.Code != http.StatusUnauthorized || fk.listCalls != 0 {
		t.Errorf("header B and no credential on /api/keys = %d (store calls %d); want 401 and none", w.Code, fk.listCalls)
	}
}
