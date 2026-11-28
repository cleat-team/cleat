package main

// cleat#3555. A minted operator key was answered 401 "invalid or revoked API key" by a real worker
// on /api/admin/health, because OperatorMiddleware authenticated it and then handed it to the
// tenant layer, which looked it up as a TENANT key.
//
// WHY EVERY EARLIER TEST WAS GREEN. They ran the operator layer over a stub handler, or put the
// operator in the request context by hand (asOperator in an_operator_acts_on_a_named_tenant_test.go)
// and called the mux. Neither composes the layers, and the bug lives only in the composition. So
// this test goes through composeAuthChain, the function main.go calls, with the real mux, the real
// handlers, and real key shapes (GenerateOperatorKey, GenerateAPIKey) carried in a real
// Authorization header. Nothing is installed in a context by the test.

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
)

// keyTable resolves keys the way the stores do: by the sha256 of the raw key.
type keyTable struct {
	tenants   map[[32]byte]uuid.UUID
	operators map[[32]byte]auth.Operator
}

var errNoSuchKey = errors.New("no such key")

func (k *keyTable) ResolveTenantFromAPIKey(_ context.Context, h []byte) (uuid.UUID, error) {
	var a [32]byte
	copy(a[:], h)
	if id, ok := k.tenants[a]; ok {
		return id, nil
	}
	return uuid.Nil, errNoSuchKey
}

func (k *keyTable) ResolveOperatorFromAPIKey(_ context.Context, h []byte) (auth.Operator, error) {
	var a [32]byte
	copy(a[:], h)
	if op, ok := k.operators[a]; ok {
		return op, nil
	}
	return auth.Operator{}, errNoSuchKey
}

// noDomains owns no hostname, so any tenant request under host binding is refused. An operator
// request must not be asked.
type noDomains struct{ asked *bool }

func (n noDomains) TenantForHost(context.Context, string, uuid.UUID) (bool, error) {
	*n.asked = true
	return false, nil
}

type chainFixture struct {
	handler        http.Handler
	adminKey       string // operator key, admin scope
	provisionKey   string // operator key, provision scope
	tenantAKey     string
	forgedOpKey    string // operator-shaped, in no table
	unknownTenant  string // tenant-shaped, in no table
	listed         *bool  // a tenant data route's handler ran, in any store
	reachedB       *bool  // tenant B's store was opened for a force-complete
	hostBindingAsk *bool
	storeA, storeB *mockStore
	keys           *keyTable // what resolves the keys above, for a test that builds its own chain
	api            *apiServer
	// noted is what the audit slot held when the request finished; see
	// a_handler_names_the_tenant_it_acted_on_test.go.
	noted *notedTarget
}

type notedTarget struct {
	tenant uuid.UUID
	set    bool
}

func buildChain(t *testing.T, requireHostMatch bool) chainFixture {
	t.Helper()
	return buildChainAround(t, requireHostMatch, nil)
}

// buildChainAround is buildChain with the plugin layer's place taken by wrap(f.noted): the chain's
// inner handler is that wrapped around the mux, which is where the audit-log middleware sits on a
// real worker.
func buildChainAround(t *testing.T, requireHostMatch bool, wrap func(*notedTarget) func(http.Handler) http.Handler) chainFixture {
	t.Helper()
	return buildChainResolving(t, requireHostMatch, wrap, "single-tenant")
}

// buildChainResolving is buildChainAround with the --tenant-resolver mode named, and the chain built by
// composeRequestChain, the function main.go calls, so the header wrapper goes where a worker puts it
// (cleat#3577).
func buildChainResolving(t *testing.T, requireHostMatch bool, wrap func(*notedTarget) func(http.Handler) http.Handler, tenantResolver string) chainFixture {
	t.Helper()
	enableAdminRoutes(t)
	api, storeA, storeB, _ := twoTenantServer(t, true)
	defaultStore := api.store.(*mockStore)

	var listed, reachedB, asked bool
	list := func(context.Context, engine.WorkflowFilter) ([]engine.WorkflowInstance, error) {
		listed = true
		return nil, nil
	}
	for _, s := range []*mockStore{storeA, storeB, defaultStore} {
		s.listWorkflowsFn = list
	}
	armForceComplete(storeB, &reachedB)
	ownsWorkflow(storeB, "wf-b", tenantB)

	f := chainFixture{
		adminKey: auth.GenerateOperatorKey(), provisionKey: auth.GenerateOperatorKey(),
		tenantAKey: auth.GenerateAPIKey(), forgedOpKey: auth.GenerateOperatorKey(),
		unknownTenant: auth.GenerateAPIKey(),
		listed:        &listed, reachedB: &reachedB, hostBindingAsk: &asked,
		storeA: storeA, storeB: storeB, noted: &notedTarget{}, api: api,
	}
	h := func(k string) [32]byte { return sha256.Sum256([]byte(k)) }
	keys := &keyTable{
		tenants: map[[32]byte]uuid.UUID{h(f.tenantAKey): uuid.MustParse(tenantA)},
		operators: map[[32]byte]auth.Operator{
			h(f.adminKey):     {KeyID: "8d264f39-9c52-4623-aa1d-258c7bcc34d7", Scopes: auth.ScopeAdmin},
			h(f.provisionKey): {KeyID: "1c1b7f3e-5f43-4a8e-bf3a-0d6a1a2b3c4d", Scopes: auth.ScopeProvision, Principal: "clew-service"},
		},
	}

	f.keys = keys
	mux := http.NewServeMux()
	registerRoutes(mux, api)
	var inner http.Handler = mux
	if wrap != nil {
		inner = wrap(f.noted)(mux)
	}
	f.handler = composeRequestChain(inner, mux, &authChain{
		Tenants: keys, Operators: keys, Domains: noDomains{asked: &asked}, RequireHostMatch: requireHostMatch,
	}, tenantResolver)
	return f
}

func (f chainFixture) do(method, path, key string, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(`{"generation":3,"result":"{}"}`))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)
	return w
}

func TestAMintedOperatorKeyReachesAnAdminRouteThroughTheChainAsMainComposesIt(t *testing.T) {
	for _, hostMatch := range []bool{false, true} {
		name := "without host binding"
		if hostMatch {
			name = "with host binding"
		}
		t.Run(name, func(t *testing.T) {
			f := buildChain(t, hostMatch)

			// THE KNOWN POSITIVE. 401 before the fix, 200 after. Every refusal below is only
			// evidence if this row is 200: a chain that refused every operator key would pass them all.
			w := f.do(http.MethodGet, "/api/admin/health", f.adminKey)
			if w.Code != http.StatusOK {
				t.Fatalf("an admin operator key on GET /api/admin/health = %d %s, want 200", w.Code, strings.TrimSpace(w.Body.String()))
			}
			if *f.hostBindingAsk {
				t.Error("an operator request was put through host binding, which asks a tenant's domain table about a credential that has no tenant")
			}

			// A key that is operator-shaped and in no table is refused by the operator layer.
			w = f.do(http.MethodGet, "/api/admin/health", f.forgedOpKey)
			if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "operator key") {
				t.Errorf("a forged operator key = %d %s, want 401 from the operator layer", w.Code, strings.TrimSpace(w.Body.String()))
			}

			// The key's own scope still applies on this path: provision reaches no admin route.
			w = f.do(http.MethodGet, "/api/admin/health", f.provisionKey)
			if w.Code != http.StatusForbidden {
				t.Errorf("a provision-scoped key on /api/admin/health = %d, want 403", w.Code)
			}
		})
	}
}

// The confinement of cleat#2169, over the composed chain: bypassing the tenant layer for an admin
// route must not become a way to reach a tenant data route without it.
func TestAnOperatorKeyStillCannotReachATenantDataRouteAroundTheTenantLayer(t *testing.T) {
	f := buildChain(t, false)

	// Direct: refused by the operator layer, and the message says it was that layer. Asserting the
	// status alone passed with the confinement deleted, because the handler behind it refuses a
	// request with no tenant too -- a second layer answering for the one under test.
	for _, path := range []string{"/api/workflows", "/API/WORKFLOWS"} {
		w := f.do(http.MethodGet, path, f.adminKey)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "may only be used on /api/admin/") {
			t.Errorf("GET %s with an operator key = %d %s, want the operator layer's 403", path, w.Code, strings.TrimSpace(w.Body.String()))
		}
	}

	// Spelled to look like the admin prefix and decode into the tenant API. These are admitted by
	// the prefix check and go to the mux, which must redirect or refuse and never serve.
	for _, path := range []string{
		"/api/admin/../workflows",
		"/api/admin/%2e%2e/workflows",
		"/api/admin/tenants/../../workflows",
	} {
		w := f.do(http.MethodGet, path, f.adminKey)
		if w.Code == http.StatusOK || *f.listed {
			t.Errorf("GET %s with an operator key = %d (tenant list handler ran: %v)", path, w.Code, *f.listed)
		}
	}

	// And the known positive for that table: the same route with a TENANT key does reach the
	// handler, so "never listed" above means refused and not "the handler cannot be reached".
	if w := f.do(http.MethodGet, "/api/workflows", f.tenantAKey); w.Code != http.StatusOK || !*f.listed {
		t.Fatalf("a tenant key on GET /api/workflows = %d (handler ran: %v), want 200 and the handler to run", w.Code, *f.listed)
	}
}

// A TENANT key is unchanged: it is still authenticated by the tenant layer, so an unknown one is
// still 401, and a valid one still cannot act on another tenant's workflow.
func TestATenantKeyIsStillHeldToTheTenantLayerOnAnAdminRoute(t *testing.T) {
	f := buildChain(t, false)

	w := f.do(http.MethodGet, "/api/admin/health", f.unknownTenant)
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "invalid or revoked API key") {
		t.Errorf("an unknown tenant key on /api/admin/health = %d %s, want the tenant layer's 401", w.Code, strings.TrimSpace(w.Body.String()))
	}

	w = f.do(http.MethodPost, adminTenantPath(tenantB, "wf-b", "force-complete"), f.tenantAKey, "X-Confirm", "force-complete")
	if w.Code != http.StatusNotFound {
		t.Errorf("tenant A's key force-completing tenant B's workflow = %d, want 404", w.Code)
	}
	if *f.reachedB {
		t.Error("tenant A's key reached tenant B's store")
	}

	// The known positive: the operator key DOES reach it, so the 404 above is the tenant check
	// and not a route that cannot be reached by anyone.
	w = f.do(http.MethodPost, adminTenantPath(tenantB, "wf-b", "force-complete"), f.adminKey, "X-Confirm", "force-complete")
	if w.Code != http.StatusOK || !*f.reachedB {
		t.Errorf("an operator key force-completing tenant B's workflow = %d (reached: %v), want 200 and reached", w.Code, *f.reachedB)
	}
}

// --enable-admin-api off means the route does not exist, for an operator as for anyone: a 404
// from the route, which is how a deployment learns the flag is off. It was a 401 before the fix,
// which sent the operator to look for a key problem.
func TestAnOperatorKeyOnAnAdminRouteWithTheAdminAPIOffIsNotFound(t *testing.T) {
	f := buildChain(t, false)
	off := false
	old := enableAdminAPI
	enableAdminAPI = &off
	t.Cleanup(func() { enableAdminAPI = old })

	if w := f.do(http.MethodGet, "/api/admin/health", f.adminKey); w.Code != http.StatusNotFound {
		t.Errorf("an operator key with the admin API off = %d, want 404", w.Code)
	}
}
