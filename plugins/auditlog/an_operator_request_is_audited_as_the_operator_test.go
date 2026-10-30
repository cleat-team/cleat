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
	tenantC := uuid.MustParse("44444444-4444-4444-4444-444444444444")

	p, _, store := setupTestPlugin(t)
	// setupTestPlugin's handler has no operator layer; build the chain it would sit in. The mux is
	// the one the plugin registered its routes on, plus three stand-ins for admin handlers. What an
	// admin handler does to the slot is the contract under test here; that the real handlers do it
	// is cmd/cleat-worker's a_handler_names_the_tenant_it_acted_on_test.go.
	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		t.Fatal(err)
	}
	mux.HandleFunc("/api/admin/health", func(http.ResponseWriter, *http.Request) {}) // names no tenant
	mux.HandleFunc("/api/admin/acts-on-b", func(_ http.ResponseWriter, r *http.Request) {
		auth.NoteAuditTarget(r.Context(), tenantB)
	})
	mux.HandleFunc("/api/admin/acts-on-b-then-c", func(_ http.ResponseWriter, r *http.Request) {
		auth.NoteAuditTarget(r.Context(), tenantB)
		auth.NoteAuditTarget(r.Context(), tenantC)
	})
	// The shape of a tenant drop (cleat#3552, B3b-2): DELETE on a bare tenant id, whose handler names NO
	// tenant, because the tenant's own chain is deleted with it.
	mux.HandleFunc("/api/admin/tenants/"+tenantB.String(), func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("/api/admin/notes-the-nil-tenant", func(_ http.ResponseWriter, r *http.Request) {
		auth.NoteAuditTarget(r.Context(), uuid.Nil)
	})
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
	op := "operator:" + keyID
	rows := []struct {
		name    string
		method  string // POST when empty
		handler http.Handler
		path    string
		key     string
		want    want
	}{
		{"a handler that names no tenant: the default chain, but attributed",
			"", chain, "/api/admin/health", opKey, want{uuid.Nil, op}},
		// A drop is recorded, on the DEFAULT chain, naming the operator, with the tenant's id in the path. Its
		// own chain is deleted with it, so a row on that chain would be recorded and then removed.
		{"a DELETE of a tenant, whose handler names none: the default chain, attributed, path kept",
			http.MethodDelete, chain, "/api/admin/tenants/" + tenantB.String(), opKey, want{uuid.Nil, op}},
		{"a handler that says it acted on tenant B: B's chain",
			"", chain, "/api/admin/acts-on-b", opKey, want{tenantB, op}},
		{"the first tenant a handler names wins",
			"", chain, "/api/admin/acts-on-b-then-c", opKey, want{tenantB, op}},
		{"the nil tenant is not a target: the default chain",
			"", chain, "/api/admin/notes-the-nil-tenant", opKey, want{uuid.Nil, op}},
		{"a request no handler claims (a 404): the default chain, not a chain for whatever the path named",
			"", chain, "/api/admin/tenants/" + tenantB.String() + "/instances/wf-1/force-complete", opKey, want{uuid.Nil, op}},
		{"a tenant put in the context by something that does not authenticate is ignored",
			"", withForgedTenant, "/api/admin/health", opKey, want{uuid.Nil, op}},
		{"...but a handler that acts on it says so, and then it is B's chain",
			"", withForgedTenant, "/api/admin/acts-on-b", opKey, want{tenantB, op}},
		// The control: a tenant key's own row is what it always was, and a handler that notes a
		// target does not move it -- only an operator request listens to the slot.
		{"the control: a tenant key on the same route stays on its own chain with no subject",
			"", chain, "/api/admin/acts-on-b", "test-api-key", want{testTenantID, ""}},
	}

	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			before := eventCount(store)
			method := tc.method
			if method == "" {
				method = "POST"
			}
			req := httptest.NewRequest(method, tc.path, strings.NewReader(""))
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
