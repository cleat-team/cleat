package auth

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
	"github.com/google/uuid"
)

// cleat#3623. A request authenticated by a tenant API key carried no subject, so the audit log's
// user_id, a library's installed_by and a plugin-dependency override's actor recorded nothing about
// who acted. The middleware now sets "key:<key id>".

// Every production resolver must implement KeyResolver. The interface is optional on purpose (a
// resolver that cannot say which key it resolved just yields no subject), and an optional interface
// that one resolver quietly fails to satisfy is a mechanism wired to nothing: the middleware would
// compile, the unit tests below would pass, and that deployment's rows would stay blank.
var (
	_ KeyResolver = (*TenantStore)(nil)
	_ KeyResolver = (*engine.PostgresStore)(nil)
	_ KeyResolver = (*engine.MySQLStore)(nil)
	_ KeyResolver = (*engine.MSSQLStore)(nil)
	_ KeyResolver = (*engine.ShardedStore)(nil)
)

// tenantOnlyResolver is a TenantResolver that is not a KeyResolver.
type tenantOnlyResolver struct{ tenant uuid.UUID }

func (r tenantOnlyResolver) ResolveTenantFromAPIKey(context.Context, []byte) (uuid.UUID, error) {
	return r.tenant, nil
}

// servedSubject runs one request through MiddlewareWithMux and reports the subject the handler saw.
func servedSubject(t *testing.T, store TenantResolver, outer func(http.Handler) http.Handler) (subject string, has bool, code int) {
	t.Helper()
	var inner http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subject, has = SubjectFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	h := MiddlewareWithMux(store, false, nil)(inner)
	if outer != nil {
		h = outer(h)
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return subject, has, rec.Code
}

func TestAnAPIKeyRequestCarriesItsKeyIDAsTheSubject(t *testing.T) {
	fake := newFakeDBStore()
	hashHex := addTestKey(fake)
	db := newTestDB(fake)
	t.Cleanup(func() { db.Close() })

	subject, has, code := servedSubject(t, engine.NewPostgresStore(db), nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	want := KeySubjectPrefix + fake.apiKeys[hashHex].keyID
	if !has || subject != want {
		t.Fatalf("subject = %q (present=%v), want %q", subject, has, want)
	}

	// The subject is stored in an audit row and shown to other users; it must be the key's row id and
	// nothing the secret can be recovered from.
	sum := sha256.Sum256([]byte(testAPIKey))
	for _, secret := range []string{testAPIKey, hashHex, fmt.Sprintf("%x", sum[:8])} {
		if strings.Contains(subject, secret) {
			t.Errorf("subject %q contains %q", subject, secret)
		}
	}
}

// An identity that a layer OUTSIDE this one already established is not replaced by the weaker fact
// that a key was presented. This is the shape of a host that puts an identity layer in front of the
// tenant layer; cleat-worker is not one (its plugin middleware, oauthprovider included, runs inside
// MiddlewareWithMux, so there an OAuth email wins by overwriting and this guard decides nothing).
func TestAnAPIKeyDoesNotOverwriteAnExistingSubject(t *testing.T) {
	fake := newFakeDBStore()
	addTestKey(fake)
	db := newTestDB(fake)
	t.Cleanup(func() { db.Close() })

	outer := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(WithSubject(r.Context(), "someone@example.com")))
		})
	}
	subject, has, _ := servedSubject(t, engine.NewPostgresStore(db), outer)
	if !has || subject != "someone@example.com" {
		t.Errorf("subject = %q (present=%v), want the outer layer's subject untouched", subject, has)
	}

	// An empty subject is no subject: it must not block the key's.
	outer = func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(WithSubject(r.Context(), "")))
		})
	}
	subject, _, _ = servedSubject(t, engine.NewPostgresStore(db), outer)
	if !strings.HasPrefix(subject, KeySubjectPrefix) {
		t.Errorf("subject = %q after an empty one; want the key's", subject)
	}
}

// A resolver that cannot say which key it resolved behaves as every resolver did before: the request
// is authenticated and carries no subject. It must not invent one.
func TestAResolverWithoutKeyIDsYieldsNoSubject(t *testing.T) {
	subject, has, code := servedSubject(t, tenantOnlyResolver{tenant: parsedTestTenantID}, nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if has || subject != "" {
		t.Errorf("subject = %q (present=%v), want none", subject, has)
	}
}

// An operator request is served by the operator layer, around the tenant one, and keeps its
// "operator:<key id>" subject; the tenant middleware's key subject has no say in it.
func TestAnOperatorSubjectIsNotAKeySubject(t *testing.T) {
	op := Operator{KeyID: "6f5c1e9a-2b1f-4a3c-9d0e-77c8b5a4e012", Scopes: ScopeAdmin}
	var subject string
	admin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subject, _ = SubjectFromContext(r.Context())
	})
	req := httptest.NewRequest(http.MethodGet, "/api/admin/drain", nil)
	req.Header.Set("Authorization", "Bearer "+GenerateOperatorKey())
	OperatorMiddleware(&fakeOperatorResolver{op: op}, admin)(admin).ServeHTTP(httptest.NewRecorder(), req)
	if subject != op.String() || strings.HasPrefix(subject, KeySubjectPrefix) {
		t.Errorf("operator subject = %q, want %q", subject, op.String())
	}
}
