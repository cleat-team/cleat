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
// except for public paths (/healthz, /metrics, and any additional patterns passed via
// publicPatterns).
//
// publicPatterns is a hand-maintained allowlist, not a generic plugin-declared
// mechanism. It exists for endpoints that are meant to be called by parties who cannot
// present a cleat API key -- an inbound webhook receiver with its own HMAC check
// (plugins/webhookingest), a third-party IdP's OAuth redirect target
// (plugins/oauthprovider) -- and would otherwise 401 before that endpoint's own
// verification ever runs. Each entry is a Go 1.22+ http.ServeMux pattern
// ("POST /ingest/{source_id}"), matched with the exact same method+wildcard semantics
// the real mux uses, via a throwaway ServeMux built only for matching (see
// buildPublicMatcher) -- so "POST /ingest/{source_id}" does not also make
// "GET /ingest/sources" public.
//
// A plugin-declared version of this (a PublicRoutes() method plugins implement
// themselves) would need changes to plugin/plugin.go and to each plugin, which are
// outside this package's ownership; wiring the list by hand in
// cmd/cleat-worker/main.go is the option available without those changes. Anyone
// adding a new externally-triggered plugin endpoint must add it here too -- nothing
// enforces that the two stay in sync.
func Middleware(store engine.WorkflowStore, requireAuth bool, publicPatterns ...string) func(http.Handler) http.Handler {
	publicMatcher := buildPublicMatcher(publicPatterns)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Public paths are always accessible without authentication.
			path := r.URL.Path
			if path == "/healthz" || path == "/metrics" {
				next.ServeHTTP(w, r)
				return
			}
			if publicMatcher != nil {
				if _, pattern := publicMatcher.Handler(r); pattern != "" {
					next.ServeHTTP(w, r)
					return
				}
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

// buildPublicMatcher builds a ServeMux containing only the given patterns, used purely
// for its Handler(r) pattern-matching -- never for actually serving anything. This
// reuses net/http's own method + wildcard matching (the same rules the real route mux
// registered these patterns with) instead of reimplementing it, so a pattern like
// "POST /ingest/{source_id}" matches only a POST to that exact shape and not, say, a
// GET to the same path or a request to a same-prefixed but different route such as
// "/ingest/sources". Returns nil when there is nothing to match, so the hot path in
// Middleware can skip the check entirely for the common case (no publicPatterns).
func buildPublicMatcher(patterns []string) *http.ServeMux {
	if len(patterns) == 0 {
		return nil
	}
	mux := http.NewServeMux()
	noop := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	for _, p := range patterns {
		mux.HandleFunc(p, noop)
	}
	return mux
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
