package auditlog

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
)

// cleat#3559. Before this, an operator key's admin request was audited on the DEFAULT tenant's
// chain with an empty user_id: identical to that tenant's own requests, and with no trace of which
// key acted. cleat-review measured it on a real binary; this is the same observation at the point
// the row is formed.
//
// The control is the tenant key on the same route, because the claim "the operator's row is
// distinguishable" means nothing unless the row it is distinguished from is still what it was.

type opKeys map[[32]byte]auth.Operator

func (o opKeys) ResolveOperatorFromAPIKey(_ context.Context, h []byte) (auth.Operator, error) {
	var k [32]byte
	copy(k[:], h)
	if op, ok := o[k]; ok {
		return op, nil
	}
	return auth.Operator{}, context.Canceled
}

func TestAnOperatorRequestIsAuditedAsTheOperator(t *testing.T) {
	const keyID = "8d264f39-9c52-4623-aa1d-258c7bcc34d7"
	tenantB := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	p, _, store := setupTestPlugin(t)
	// setupTestPlugin's handler has no operator layer; build the chain it would sit in. The mux is
	// the bare one the plugin registered its routes on, so every /api/admin/* request answers 404
	// from it -- which is fine: the audit row records the request whatever it answered.
	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatal(err)
	}
	audited := p.Middleware(mux)
	db := sql.OpenDB(&fakeConnector{store: store})
	t.Cleanup(func() { db.Close() })
	tenantChain := auth.MiddlewareWithMux(engine.NewPostgresStore(db), false, mux)(audited)

	opKey := auth.GenerateOperatorKey()
	ops := opKeys{sha256.Sum256([]byte(opKey)): {KeyID: keyID, Scopes: auth.ScopeAdmin}}
	chain := auth.OperatorMiddleware(ops, audited)(tenantChain)

	// The header: tenant resolver runs OUTSIDE the auth chain and puts a client-chosen tenant in
	// the context of every request. A forged one must not decide an operator row's chain.
	forged := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	withForgedTenant := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chain.ServeHTTP(w, r.WithContext(auth.WithTenantID(r.Context(), forged)))
	})

	type want struct {
		tenant uuid.UUID
		user   string
	}
	rows := []struct {
		name    string
		handler http.Handler
		method  string
		path    string
		key     string
		want    want
	}{
		{"an operator on a route naming no tenant: the default chain, but attributed",
			chain, "GET", "/api/admin/health", opKey, want{uuid.Nil, "operator:" + keyID}},
		{"an operator on a route naming a tenant: that tenant's chain",
			chain, "POST", "/api/admin/tenants/" + tenantB.String() + "/instances/wf-1/force-complete", opKey,
			want{tenantB, "operator:" + keyID}},
		{"the same, spelled with braces: the same chain",
			chain, "POST", "/api/admin/tenants/{" + tenantB.String() + "}/instances/wf-1/force-complete", opKey,
			want{tenantB, "operator:" + keyID}},
		{"a tenant put in the context by something that does not authenticate is ignored: no target, default chain",
			withForgedTenant, "GET", "/api/admin/health", opKey, want{uuid.Nil, "operator:" + keyID}},
		{"...and so is one when the route does name a tenant",
			withForgedTenant, "POST", "/api/admin/tenants/" + tenantB.String() + "/instances/wf-1/force-complete", opKey,
			want{tenantB, "operator:" + keyID}},
		// The control: a tenant key's own row, which must be what it always was.
		{"the control: a tenant key on the same route, on its own chain with no subject",
			chain, "GET", "/api/admin/health", "test-api-key", want{testTenantID, ""}},
	}

	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			before := eventCount(store)
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(""))
			req.Header.Set("Authorization", "Bearer "+tc.key)
			tc.handler.ServeHTTP(httptest.NewRecorder(), req)
			if !waitForEventCount(store, before+1, time.Second) {
				t.Fatal("no audit event was recorded")
			}
			store.mu.RLock()
			ev := store.events[before]
			store.mu.RUnlock()
			if ev.tenantID != tc.want.tenant || ev.userID != tc.want.user {
				t.Errorf("recorded tenant %s, user_id %q; want tenant %s, user_id %q",
					ev.tenantID, ev.userID, tc.want.tenant, tc.want.user)
			}
		})
	}
}

func eventCount(s *fakeDBStore) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.events)
}
