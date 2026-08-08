// Package auth provides tenant-aware API key authentication for cleat's
// cleat execution framework.
//
// It implements Bearer token and header-based auth, tenant ID extraction
// from API keys via PostgreSQL lookup, and context propagation of tenant IDs.
package auth

import (
	"context"
	"crypto/sha256"
	"net/http"
	"strings"

	"github.com/cleat-team/cleat/engine"
	"github.com/google/uuid"
)

type tenantIDKey struct{}

// WithTenantID sets the tenant ID in the context. Primarily for testing.
func WithTenantID(ctx context.Context, tenantID uuid.UUID) context.Context {
	return context.WithValue(ctx, tenantIDKey{}, tenantID)
}

// TenantIDFromContext extracts the tenant ID from the request context.
func TenantIDFromContext(ctx context.Context) (uuid.UUID, bool) {
	tid, ok := ctx.Value(tenantIDKey{}).(uuid.UUID)
	return tid, ok
}

// TenantFromAPIKey looks up a tenant by API key hash.
func TenantFromAPIKey(ctx context.Context, store engine.WorkflowStore, keyHash []byte) (uuid.UUID, error) {
	return store.ResolveTenantFromAPIKey(ctx, keyHash)
}

// Middleware authenticates requests using a cleat API key.
// Supports: Authorization: Bearer cleat_sk_<key>
// Also supports: X-Cleat-API-Key: <key>
// When requireAuth is true, requests without a valid API key are rejected with 401,
// except for public paths (/healthz, /metrics).
func Middleware(store engine.WorkflowStore, requireAuth bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Public paths are always accessible without authentication.
			path := r.URL.Path
			if path == "/healthz" || path == "/metrics" {
				next.ServeHTTP(w, r)
				return
			}

			key := extractAPIKey(r)
			if key == "" {
				if requireAuth {
					http.Error(w, `{"error":"authentication required: provide an API key via Authorization: Bearer <key> or X-Cleat-API-Key: <key>"}`, http.StatusUnauthorized)
					return
				}
				// If not required, proceed without tenant — handler decides if that's OK.
				next.ServeHTTP(w, r)
				return
			}
			keyHash := sha256Hash(key)
			tenantID, err := TenantFromAPIKey(r.Context(), store, keyHash)
			if err != nil {
				http.Error(w, `{"error":"invalid or revoked API key"}`, http.StatusUnauthorized)
				return
			}
			ctx := context.WithValue(r.Context(), tenantIDKey{}, tenantID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func extractAPIKey(r *http.Request) string {
	// Support Authorization: Bearer <key>
	auth := r.Header.Get("Authorization")
	if auth != "" {
		parts := strings.SplitN(auth, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
			return parts[1]
		}
	}
	// Support X-Cleat-API-Key: <key>
	return r.Header.Get("X-Cleat-API-Key")
}

// sha256Hash hashes an API key for lookup, not for password-style
// verification. CodeQL flags this (go/weak-sensitive-data-hashing,
// alert #13) because SHA-256 is not a computationally expensive KDF and
// would be a poor choice for a low-entropy, human-chosen secret. It is a
// reasonable choice here because the input is never that: every key
// reaching this function was produced by GenerateAPIKey (tenant_store.go),
// which is 32 bytes from crypto/rand — 256 bits of entropy, hex-encoded.
// SHA-256 over a secret with that much entropy is not brute-forceable
// offline even given the hash, so a slow KDF buys no additional
// protection; its only job here is to avoid storing the plaintext key and
// to give the DB an equality-indexable lookup value (ResolveTenantFromAPIKey
// does `WHERE key_hash = $1`, not a constant-time password comparison).
// If a future caller ever stores a lower-entropy, user-chosen credential
// through this same path, this reasoning no longer holds and the hash
// needs to move to bcrypt/scrypt/argon2 (golang.org/x/crypto is already a
// dependency) with a versioned-hash migration for existing rows.
// Dismissed: see alert #13.
func sha256Hash(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}
