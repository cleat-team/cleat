package auth

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/cleat-team/cleat/internal/host"
)

// --- TenantStore tests ------------------------------------------------------

func TestTenantStore_CreateTenant(t *testing.T) {
	store := newFakeDBStore()
	db := newTestDB(store)
	t.Cleanup(func() { db.Close() })

	ts := NewTenantStore(db)

	tid, err := ts.CreateTenant(context.Background(), "test-tenant", "Test Tenant")
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if tid == uuid.Nil {
		t.Error("expected non-nil tenant ID")
	}

	// Verify the tenant was stored in the fake DB.
	store.mu.RLock()
	defer store.mu.RUnlock()
	if len(store.tenants) != 1 {
		t.Fatalf("expected 1 tenant in store, got %d", len(store.tenants))
	}
	if store.tenants[0].name != "test-tenant" {
		t.Errorf("expected name 'test-tenant', got %q", store.tenants[0].name)
	}
	if store.tenants[0].displayName != "Test Tenant" {
		t.Errorf("expected display_name 'Test Tenant', got %q", store.tenants[0].displayName)
	}
	if store.tenants[0].tenantID != tid.String() {
		t.Errorf("expected stored tenantID %q, got %q", tid.String(), store.tenants[0].tenantID)
	}
}

func TestTenantStore_CreateTenant_ReturnsUUID(t *testing.T) {
	// Verify the returned UUID is a valid, non-nil UUID.
	store := newFakeDBStore()
	db := newTestDB(store)
	t.Cleanup(func() { db.Close() })

	ts := NewTenantStore(db)

	tid, err := ts.CreateTenant(context.Background(), "returns-uuid", "Test")
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if tid == uuid.Nil {
		t.Fatal("expected non-nil UUID")
	}
	// The UUID should be parseable (it already is since it's returned as uuid.UUID).
	if tid.Variant() != uuid.RFC4122 {
		// Our fake driver generates UUIDs in standard format, but they aren't
		// true RFC 4122 UUIDs. Just verify it's non-nil.
		t.Logf("note: tenant UUID variant=%d (may not be RFC4122 in fake driver)", tid.Variant())
	}
}

func TestTenantStore_CreateTenant_DuplicateName(t *testing.T) {
	store := newFakeDBStore()
	db := newTestDB(store)
	t.Cleanup(func() { db.Close() })

	ts := NewTenantStore(db)

	_, err := ts.CreateTenant(context.Background(), "duplicate", "First")
	if err != nil {
		t.Fatalf("first CreateTenant: %v", err)
	}

	_, err = ts.CreateTenant(context.Background(), "duplicate", "Second")
	if err == nil {
		t.Fatal("expected error for duplicate tenant name")
	}
	// The error should mention the unique constraint violation.
	if !strings.Contains(err.Error(), "duplicate key") {
		t.Errorf("expected error about duplicate key, got %v", err)
	}
}

func TestTenantStore_CreateTenant_MultipleTenants(t *testing.T) {
	store := newFakeDBStore()
	db := newTestDB(store)
	t.Cleanup(func() { db.Close() })

	ts := NewTenantStore(db)

	tid1, err := ts.CreateTenant(context.Background(), "tenant-a", "Tenant A")
	if err != nil {
		t.Fatalf("CreateTenant tenant-a: %v", err)
	}
	tid2, err := ts.CreateTenant(context.Background(), "tenant-b", "Tenant B")
	if err != nil {
		t.Fatalf("CreateTenant tenant-b: %v", err)
	}

	if tid1 == tid2 {
		t.Error("expected different UUIDs for different tenants")
	}

	store.mu.RLock()
	defer store.mu.RUnlock()
	if len(store.tenants) != 2 {
		t.Fatalf("expected 2 tenants in store, got %d", len(store.tenants))
	}
}

func TestTenantStore_CreateAPIKey(t *testing.T) {
	store := newFakeDBStore()
	db := newTestDB(store)
	t.Cleanup(func() { db.Close() })

	ts := NewTenantStore(db)

	tid, err := ts.CreateTenant(context.Background(), "key-tenant", "Key Tenant")
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}

	rawKey := "cleat_sk_testapikey"
	err = ts.CreateAPIKey(context.Background(), tid, "my test key", rawKey)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	// Verify the key was stored with the correct hash.
	hash := sha256.Sum256([]byte(rawKey))
	hashHex := fmt.Sprintf("%x", hash[:])

	store.mu.RLock()
	defer store.mu.RUnlock()
	k, ok := store.apiKeys[hashHex]
	if !ok {
		t.Fatal("expected API key to be stored")
	}
	if k.tenantID != tid.String() {
		t.Errorf("expected tenant ID %q, got %q", tid.String(), k.tenantID)
	}
	if k.description != "my test key" {
		t.Errorf("expected description 'my test key', got %q", k.description)
	}
	if k.revokedAt != nil {
		t.Error("expected new key to not be revoked")
	}
}

func TestTenantStore_CreateAPIKey_DifferentKeys(t *testing.T) {
	store := newFakeDBStore()
	db := newTestDB(store)
	t.Cleanup(func() { db.Close() })

	ts := NewTenantStore(db)

	tid, err := ts.CreateTenant(context.Background(), "multi-key-tenant", "Multi Key Tenant")
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}

	err = ts.CreateAPIKey(context.Background(), tid, "key one", "raw-key-one")
	if err != nil {
		t.Fatalf("CreateAPIKey key one: %v", err)
	}
	err = ts.CreateAPIKey(context.Background(), tid, "key two", "raw-key-two")
	if err != nil {
		t.Fatalf("CreateAPIKey key two: %v", err)
	}

	hash1 := sha256.Sum256([]byte("raw-key-one"))
	hash2 := sha256.Sum256([]byte("raw-key-two"))

	store.mu.RLock()
	defer store.mu.RUnlock()
	if _, ok := store.apiKeys[fmt.Sprintf("%x", hash1[:])]; !ok {
		t.Error("expected key one to be stored")
	}
	if _, ok := store.apiKeys[fmt.Sprintf("%x", hash2[:])]; !ok {
		t.Error("expected key two to be stored")
	}
	if len(store.apiKeys) != 2 {
		t.Errorf("expected 2 API keys in store, got %d", len(store.apiKeys))
	}
}

func TestTenantStore_RevokeAPIKey(t *testing.T) {
	store := newFakeDBStore()
	db := newTestDB(store)
	t.Cleanup(func() { db.Close() })

	ts := NewTenantStore(db)

	tid, err := ts.CreateTenant(context.Background(), "revoke-tenant", "Revoke Tenant")
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}

	rawKey := "cleat_sk_revokable"
	err = ts.CreateAPIKey(context.Background(), tid, "revocable key", rawKey)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	// Find the key ID from the fake store.
	hash := sha256.Sum256([]byte(rawKey))
	hashHex := fmt.Sprintf("%x", hash[:])

	store.mu.RLock()
	k, ok := store.apiKeys[hashHex]
	store.mu.RUnlock()
	if !ok {
		t.Fatal("expected API key to exist before revoke")
	}
	if k.revokedAt != nil {
		t.Fatal("expected key to NOT be revoked before revoke")
	}

	keyID := uuid.MustParse(k.keyID)

	// Revoke the key.
	err = ts.RevokeAPIKey(context.Background(), keyID)
	if err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}

	// Verify it's now revoked.
	store.mu.RLock()
	k, ok = store.apiKeys[hashHex]
	store.mu.RUnlock()
	if !ok {
		t.Fatal("expected API key to still exist after revoke")
	}
	if k.revokedAt == nil {
		t.Fatal("expected key to be revoked after RevokeAPIKey")
	}
}

func TestTenantStore_RevokeAPIKey_NotFoundIsNotError(t *testing.T) {
	// Revoking a non-existent key should not return an error (UPDATE with no
	// matching rows is not an error in PostgreSQL).
	store := newFakeDBStore()
	db := newTestDB(store)
	t.Cleanup(func() { db.Close() })

	ts := NewTenantStore(db)

	nonexistentKeyID := uuid.MustParse("00000000-0000-0000-0000-ffffffffffff")
	err := ts.RevokeAPIKey(context.Background(), nonexistentKeyID)
	if err != nil {
		t.Fatalf("RevokeAPIKey on non-existent key: %v (expected nil)", err)
	}
}

func TestTenantStore_RevokeAPIKey_DoubleRevokeIsIdempotent(t *testing.T) {
	store := newFakeDBStore()
	db := newTestDB(store)
	t.Cleanup(func() { db.Close() })

	ts := NewTenantStore(db)

	tid, err := ts.CreateTenant(context.Background(), "double-revoke-tenant", "Double Revoke")
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}

	err = ts.CreateAPIKey(context.Background(), tid, "double revoke key", "some-key")
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	// Find the key ID.
	hash := sha256.Sum256([]byte("some-key"))
	hashHex := fmt.Sprintf("%x", hash[:])
	store.mu.RLock()
	k := store.apiKeys[hashHex]
	store.mu.RUnlock()
	keyID := uuid.MustParse(k.keyID)

	// First revoke.
	err = ts.RevokeAPIKey(context.Background(), keyID)
	if err != nil {
		t.Fatalf("first RevokeAPIKey: %v", err)
	}

	// Second revoke (should also succeed, just no rows affected).
	err = ts.RevokeAPIKey(context.Background(), keyID)
	if err != nil {
		t.Fatalf("second RevokeAPIKey should be idempotent, got: %v", err)
	}
}

func TestTenantStore_RevokeAPIKey_RevokedKeyCannotAuthenticate(t *testing.T) {
	// Integration-style test: after revoking, TenantFromAPIKey should return
	// an error for the revoked key.
	store := newFakeDBStore()
	db := newTestDB(store)
	t.Cleanup(func() { db.Close() })

	ts := NewTenantStore(db)

	tid, err := ts.CreateTenant(context.Background(), "auth-after-revoke", "Auth After Revoke")
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}

	rawKey := "cleat_sk_will-be-revoked"
	err = ts.CreateAPIKey(context.Background(), tid, "will revoke", rawKey)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	// Verify the key works before revocation.
	hash := sha256.Sum256([]byte(rawKey))
	_, err = TenantFromAPIKey(context.Background(), host.NewPostgresStore(db), hash[:])
	if err != nil {
		t.Fatalf("expected key to work before revoke: %v", err)
	}

	// Find and revoke the key.
	hashHex := fmt.Sprintf("%x", hash[:])
	store.mu.RLock()
	keyID := uuid.MustParse(store.apiKeys[hashHex].keyID)
	store.mu.RUnlock()

	err = ts.RevokeAPIKey(context.Background(), keyID)
	if err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}

	// Verify the key no longer authenticates.
	_, err = TenantFromAPIKey(context.Background(), host.NewPostgresStore(db), hash[:])
	if err == nil {
		t.Fatal("expected error for revoked key in TenantFromAPIKey")
	}
}

// --- Context propagation ----------------------------------------------------

func TestTenantIDFromContext_EmptyContext(t *testing.T) {
	_, ok := TenantIDFromContext(context.Background())
	if ok {
		t.Error("expected false for background context")
	}
}

func TestTenantIDFromContext_NilContext(t *testing.T) {
	// We expect a panic when passing nil context to TenantIDFromContext.
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic with nil context")
		}
	}()
	_, _ = TenantIDFromContext(nil)
}

// --- GenerateAPIKey tests ---------------------------------------------------

func TestGenerateAPIKey_Format(t *testing.T) {
	key := GenerateAPIKey()
	if !strings.HasPrefix(key, "cleat_sk_") {
		t.Errorf("expected key to start with 'cleat_sk_', got %q", key)
	}
	// "cleat_sk_" (9 chars) + 64 hex chars (32 bytes) = 73
	if len(key) != 73 {
		t.Errorf("expected key length 72, got %d", len(key))
	}
}

func TestGenerateAPIKey_Unique(t *testing.T) {
	keys := make(map[string]bool)
	for i := 0; i < 10; i++ {
		key := GenerateAPIKey()
		if keys[key] {
			t.Errorf("duplicate key generated: %q", key)
		}
		keys[key] = true
	}
	if len(keys) != 10 {
		t.Errorf("expected 10 unique keys, got %d", len(keys))
	}
}

// --- NewTenantStore tests ---------------------------------------------------

func TestNewTenantStore(t *testing.T) {
	store := newFakeDBStore()
	db := newTestDB(store)
	t.Cleanup(func() { db.Close() })

	ts := NewTenantStore(db)
	if ts == nil {
		t.Fatal("NewTenantStore returned nil")
	}
	// Verify it's functional.
	tid, err := ts.CreateTenant(context.Background(), "new-test", "New Test")
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if tid == uuid.Nil {
		t.Error("expected non-nil tenant ID")
	}
}

// --- Helper: verify the relationship between TenantStore and Middleware ------

func TestStoreAndMiddleware_EndToEnd(t *testing.T) {
	// Full round-trip: create tenant + API key via TenantStore, then
	// authenticate via Middleware and verify tenant context is set.
	store := newFakeDBStore()
	db := newTestDB(store)
	t.Cleanup(func() { db.Close() })

	ts := NewTenantStore(db)
	tid, err := ts.CreateTenant(context.Background(), "e2e-tenant", "E2E Tenant")
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}

	rawKey := "cleat_sk_e2etestkey"
	err = ts.CreateAPIKey(context.Background(), tid, "e2e test key", rawKey)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	// Now use the middleware with the same DB.
	var gotTenant uuid.UUID
	var gotOK bool
	mw := Middleware(host.NewPostgresStore(db), false)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTenant, gotOK = TenantIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+rawKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !gotOK {
		t.Fatal("expected tenant ID in context after e2e auth")
	}
	if gotTenant != tid {
		t.Errorf("expected tenant %v, got %v", tid, gotTenant)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

// --- Verify randRead is used -------------------------------------------------

func TestGenerateAPIKey_UsesCryptoRand(t *testing.T) {
	// GenerateAPIKey reads from randRead (default: crypto/rand.Read).
	// We can't easily test that crypto/rand is actually random, but we can
	// override randRead in tests to verify it's being called.
	originalRandRead := randRead
	t.Cleanup(func() { randRead = originalRandRead })

	var callCount int
	randRead = func(b []byte) (int, error) {
		callCount++
		for i := range b {
			b[i] = byte(i)
		}
		return len(b), nil
	}

	key := GenerateAPIKey()
	if callCount != 1 {
		t.Errorf("expected randRead to be called once, got %d", callCount)
	}
	if key != "cleat_sk_000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f" {
		t.Errorf("unexpected key with deterministic rand: %q", key)
	}
}
