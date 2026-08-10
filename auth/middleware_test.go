package auth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/google/uuid"
)

// testTenantID is a well-known UUID used as the test tenant.
const testTenantID = "00000000-0000-0000-0000-000000000001"

// testAPIKey is the raw API key used for valid-key tests.
const testAPIKey = "cleat_sk_testvalidkey123"

// addTestKey inserts a valid API key into the fake store and returns its
// SHA-256 hex hash.
func addTestKey(store *fakeDBStore) string {
	hash := sha256.Sum256([]byte(testAPIKey))
	hashHex := fmt.Sprintf("%x", hash[:])
	store.apiKeys[hashHex] = fakeAPIKeyRow{
		keyID:       "00000000-0000-0000-0000-000000000001",
		tenantID:    testTenantID,
		keyHashHex:  hashHex,
		description: "test key",
	}
	return hashHex
}

// newTestDB creates a *sql.DB backed by the given fakeDBStore.
func newTestDB(store *fakeDBStore) *sql.DB {
	return sql.OpenDB(&fakeConnector{store: store})
}

// parsedTestTenantID is the uuid.UUID form of testTenantID.
var parsedTestTenantID = uuid.MustParse(testTenantID)

// --- Middleware tests -------------------------------------------------------

func TestMiddleware_NoAuth_PassesThrough(t *testing.T) {
	store := newFakeDBStore()
	db := newTestDB(store)
	t.Cleanup(func() { db.Close() })

	var handlerCalled bool
	var hadTenant bool
	mw := Middleware(engine.NewPostgresStore(db), false)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		_, hadTenant = TenantIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !handlerCalled {
		t.Error("expected downstream handler to be called")
	}
	if hadTenant {
		t.Error("expected no tenant ID in context without auth header")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestMiddleware_BearerToken_Valid_SetsTenantContext(t *testing.T) {
	store := newFakeDBStore()
	addTestKey(store)
	db := newTestDB(store)
	t.Cleanup(func() { db.Close() })

	var handlerCalled bool
	var gotTenant uuid.UUID
	var gotOK bool
	mw := Middleware(engine.NewPostgresStore(db), false)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		gotTenant, gotOK = TenantIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !handlerCalled {
		t.Error("expected downstream handler to be called")
	}
	if !gotOK {
		t.Fatal("expected tenant ID in context")
	}
	if gotTenant != parsedTestTenantID {
		t.Errorf("expected tenant ID %v, got %v", parsedTestTenantID, gotTenant)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestMiddleware_XCleatAPIKey_Valid_SetsTenantContext(t *testing.T) {
	store := newFakeDBStore()
	addTestKey(store)
	db := newTestDB(store)
	t.Cleanup(func() { db.Close() })

	var handlerCalled bool
	var gotTenant uuid.UUID
	var gotOK bool
	mw := Middleware(engine.NewPostgresStore(db), false)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		gotTenant, gotOK = TenantIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Cleat-API-Key", testAPIKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !handlerCalled {
		t.Error("expected downstream handler to be called")
	}
	if !gotOK {
		t.Fatal("expected tenant ID in context")
	}
	if gotTenant != parsedTestTenantID {
		t.Errorf("expected tenant ID %v, got %v", parsedTestTenantID, gotTenant)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestMiddleware_InvalidToken_ReturnsUnauthorized(t *testing.T) {
	// An API key that hasn't been stored should get 401.
	store := newFakeDBStore()
	db := newTestDB(store)
	t.Cleanup(func() { db.Close() })

	var handlerCalled bool
	mw := Middleware(engine.NewPostgresStore(db), false)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer nonexistent-key")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if handlerCalled {
		t.Error("expected downstream handler NOT to be called for invalid key")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("expected text/plain content-type, got %q", ct)
	}
	body := strings.TrimSpace(rec.Body.String())
	if !strings.Contains(body, "invalid or revoked API key") {
		t.Errorf("expected error message about invalid key, got %q", body)
	}
}

func TestMiddleware_RevokedToken_ReturnsUnauthorized(t *testing.T) {
	store := newFakeDBStore()
	addTestKey(store)
	// Immediately revoke the key we just added.
	hash := sha256.Sum256([]byte(testAPIKey))
	hashHex := fmt.Sprintf("%x", hash[:])
	store.mu.Lock()
	k := store.apiKeys[hashHex]
	now := time.Now()
	k.revokedAt = &now
	store.apiKeys[hashHex] = k
	store.mu.Unlock()

	db := newTestDB(store)
	t.Cleanup(func() { db.Close() })

	var handlerCalled bool
	mw := Middleware(engine.NewPostgresStore(db), false)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if handlerCalled {
		t.Error("expected downstream handler NOT to be called for revoked key")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestMiddleware_MalformedAuthHeader_NotBearer(t *testing.T) {
	// "Authorization: Basic ..." (not Bearer) should not panic. It should be
	// silently ignored (falls through to X-Cleat-API-Key), and if that is also
	// absent, the request passes through without tenant context.
	store := newFakeDBStore()
	db := newTestDB(store)
	t.Cleanup(func() { db.Close() })

	var handlerCalled bool
	mw := Middleware(engine.NewPostgresStore(db), false)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !handlerCalled {
		t.Error("expected downstream handler to be called for non-Bearer auth header")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestMiddleware_MalformedAuthHeader_OnlyBearerKeyword(t *testing.T) {
	// "Authorization: Bearer" (no key after it) should not panic and should
	// pass through (extracted key is empty).
	store := newFakeDBStore()
	db := newTestDB(store)
	t.Cleanup(func() { db.Close() })

	var handlerCalled bool
	mw := Middleware(engine.NewPostgresStore(db), false)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !handlerCalled {
		t.Error("expected downstream handler to be called")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestMiddleware_BearerTakesPriorityOverXCleatKey(t *testing.T) {
	// When both headers are present, the Bearer token is used.
	store := newFakeDBStore()
	addTestKey(store)
	db := newTestDB(store)
	t.Cleanup(func() { db.Close() })

	// Add a second key that would be used if X-Cleat-API-Key were consulted.
	secondKey := "cleat_sk_other"
	hash2 := sha256.Sum256([]byte(secondKey))
	hashHex2 := fmt.Sprintf("%x", hash2[:])
	store.mu.Lock()
	store.apiKeys[hashHex2] = fakeAPIKeyRow{
		keyID:       "00000000-0000-0000-0000-000000000002",
		tenantID:    "00000000-0000-0000-0000-000000000099",
		keyHashHex:  hashHex2,
		description: "second key",
	}
	store.mu.Unlock()

	mw := Middleware(engine.NewPostgresStore(db), false)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid, ok := TenantIDFromContext(r.Context())
		if !ok || tid != parsedTestTenantID {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("X-Cleat-API-Key", secondKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 (Bearer should win), got %d", rec.Code)
	}
}

func TestMiddleware_TenantIDPropagation(t *testing.T) {
	// Verify that the tenant ID from a valid token is propagated through a
	// chain of middlewares.
	store := newFakeDBStore()
	addTestKey(store)
	db := newTestDB(store)
	t.Cleanup(func() { db.Close() })

	outerMW := Middleware(engine.NewPostgresStore(db), false)
	innerMW := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tid, ok := TenantIDFromContext(r.Context())
			if !ok || tid != parsedTestTenantID {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}

	handler := outerMW(innerMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid, ok := TenantIDFromContext(r.Context())
		if !ok || tid != parsedTestTenantID {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

// --- publicPatterns tests -----------------------------------------------------
//
// S6: cleat-worker wraps plugin routes -- including externally-triggered ones
// with their own verification, like webhook-ingest's HMAC-checked
// POST /ingest/{source_id} and oauth-provider's GET /oauth/{provider}/callback
// -- in this same middleware. Without an explicit publicPatterns allowlist, an
// external caller who cannot present a cleat API key would 401 before the
// plugin's own check ever ran. These tests exercise that allowlist directly
// (not the specific patterns cmd/cleat-worker/main.go wires up, which live
// outside this package).

func TestMiddleware_PublicPattern_PassesThroughWithoutKey(t *testing.T) {
	store := newFakeDBStore()
	db := newTestDB(store)
	t.Cleanup(func() { db.Close() })

	var handlerCalled bool
	mw := Middleware(engine.NewPostgresStore(db), true, "POST /ingest/{source_id}")
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("POST", "/ingest/abc-123", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !handlerCalled {
		t.Error("expected downstream handler to be called for a declared public pattern")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for public pattern without a key, got %d", rec.Code)
	}
}

func TestMiddleware_PublicPattern_DoesNotWidenToSiblingPath(t *testing.T) {
	// "POST /ingest/{source_id}" is public; "/ingest/sources" (a different,
	// tenant-scoped route registered by the same plugin) must not become
	// public as a side effect of sharing the "/ingest/" prefix.
	store := newFakeDBStore()
	db := newTestDB(store)
	t.Cleanup(func() { db.Close() })

	var handlerCalled bool
	mw := Middleware(engine.NewPostgresStore(db), true, "POST /ingest/{source_id}")
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/ingest/sources", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if handlerCalled {
		t.Error("expected downstream handler NOT to be called for /ingest/sources")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for /ingest/sources (not covered by the public pattern), got %d", rec.Code)
	}
}

func TestMiddleware_PublicPattern_MethodIsRespected(t *testing.T) {
	// "POST /ingest/{source_id}" is public; a GET to the same path shape is a
	// different route (it does not exist here) and must still be rejected,
	// not silently treated as public because the path matched.
	store := newFakeDBStore()
	db := newTestDB(store)
	t.Cleanup(func() { db.Close() })

	var handlerCalled bool
	mw := Middleware(engine.NewPostgresStore(db), true, "POST /ingest/{source_id}")
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/ingest/abc-123", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if handlerCalled {
		t.Error("expected downstream handler NOT to be called for GET on a POST-only public pattern")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestMiddleware_NoPublicPatterns_StillRequiresAuth(t *testing.T) {
	// Baseline: calling Middleware without the variadic publicPatterns
	// (as every call site outside cmd/cleat-worker/main.go does) must behave
	// exactly as before -- requireAuth=true rejects an unauthenticated
	// request to any non-/healthz, non-/metrics path.
	store := newFakeDBStore()
	db := newTestDB(store)
	t.Cleanup(func() { db.Close() })

	var handlerCalled bool
	mw := Middleware(engine.NewPostgresStore(db), true)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("POST", "/ingest/abc-123", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if handlerCalled {
		t.Error("expected downstream handler NOT to be called: no public patterns were declared")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

// --- TenantFromAPIKey direct tests ------------------------------------------

func TestTenantFromAPIKey_Found(t *testing.T) {
	store := newFakeDBStore()
	addTestKey(store)
	db := newTestDB(store)
	t.Cleanup(func() { db.Close() })

	hash := sha256.Sum256([]byte(testAPIKey))
	tid, err := TenantFromAPIKey(context.Background(), engine.NewPostgresStore(db), hash[:])
	if err != nil {
		t.Fatalf("TenantFromAPIKey: %v", err)
	}
	if tid != parsedTestTenantID {
		t.Errorf("expected tenant %v, got %v", parsedTestTenantID, tid)
	}
}

func TestTenantFromAPIKey_NotFound(t *testing.T) {
	store := newFakeDBStore()
	db := newTestDB(store)
	t.Cleanup(func() { db.Close() })

	hash := sha256.Sum256([]byte("unknown-key"))
	_, err := TenantFromAPIKey(context.Background(), engine.NewPostgresStore(db), hash[:])
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
}

func TestTenantFromAPIKey_NilDB(t *testing.T) {
	// With a nil *sql.DB, calling TenantFromAPIKey should panic (nil deref).
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic with nil DB")
		}
	}()

	hash := sha256.Sum256([]byte("any-key"))
	_, _ = TenantFromAPIKey(context.Background(), nil, hash[:])
}

// --- sha256Hash tests -------------------------------------------------------

func TestSHA256Hash_Deterministic(t *testing.T) {
	h1 := sha256Hash("hello")
	h2 := sha256Hash("hello")
	if len(h1) != 32 {
		t.Errorf("expected 32 bytes, got %d", len(h1))
	}
	for i := range h1 {
		if h1[i] != h2[i] {
			t.Fatalf("expected deterministic hash, mismatch at byte %d", i)
		}
	}
}

func TestSHA256Hash_Different(t *testing.T) {
	h1 := sha256Hash("key-a")
	h2 := sha256Hash("key-b")
	equal := true
	for i := range h1 {
		if h1[i] != h2[i] {
			equal = false
			break
		}
	}
	if equal {
		t.Error("expected different hashes for different inputs")
	}
}

// --- extractAPIKey tests ----------------------------------------------------

func TestExtractAPIKey_Bearer(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer mykey")
	key := extractAPIKey(req)
	if key != "mykey" {
		t.Errorf("expected 'mykey', got %q", key)
	}
}

func TestExtractAPIKey_BearerCaseInsensitive(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "BEARER mykey")
	key := extractAPIKey(req)
	if key != "mykey" {
		t.Errorf("expected 'mykey', got %q", key)
	}
}

func TestExtractAPIKey_XCleatAPIKey(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Cleat-API-Key", "mykey")
	key := extractAPIKey(req)
	if key != "mykey" {
		t.Errorf("expected 'mykey', got %q", key)
	}
}

func TestExtractAPIKey_NoHeader(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	key := extractAPIKey(req)
	if key != "" {
		t.Errorf("expected empty, got %q", key)
	}
}

func TestExtractAPIKey_NonBearerAuth(t *testing.T) {
	// Basic auth should be ignored by extractAPIKey (falls through to X-Cleat-API-Key).
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	key := extractAPIKey(req)
	if key != "" {
		t.Errorf("expected empty for Basic auth, got %q", key)
	}
}

func TestExtractAPIKey_BearerOverridesXCleatKey(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer bearerkey")
	req.Header.Set("X-Cleat-API-Key", "xkey")
	key := extractAPIKey(req)
	if key != "bearerkey" {
		t.Errorf("expected 'bearerkey' (Bearer priority), got %q", key)
	}
}

func TestExtractAPIKey_BearerWithExtraSpaces(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer  key-with-leading-space")
	key := extractAPIKey(req)
	// SplitN with limit 2 gives ["Bearer", " key-with-leading-space"]
	if key != " key-with-leading-space" {
		t.Errorf("expected ' key-with-leading-space' (with leading space), got %q", key)
	}
}

// --- WithTenantID / TenantIDFromContext direct tests ------------------------

func TestWithTenantID(t *testing.T) {
	tid := uuid.MustParse("a1b2c3d4-e5f6-7890-abcd-ef1234567890")
	ctx := WithTenantID(context.Background(), tid)

	got, ok := TenantIDFromContext(ctx)
	if !ok {
		t.Fatal("TenantIDFromContext: expected ok")
	}
	if got != tid {
		t.Errorf("TenantIDFromContext = %v, want %v", got, tid)
	}
}

func TestWithTenantID_Override(t *testing.T) {
	// Calling WithTenantID twice should use the latest value.
	first := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	second := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	ctx := WithTenantID(context.Background(), first)
	ctx = WithTenantID(ctx, second)

	got, ok := TenantIDFromContext(ctx)
	if !ok {
		t.Fatal("TenantIDFromContext: expected ok")
	}
	if got != second {
		t.Errorf("TenantIDFromContext = %v, want %v (the latest)", got, second)
	}
}
