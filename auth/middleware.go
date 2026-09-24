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

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/internal/tenantctx"
)

// WithTenantID sets the tenant ID in the context.
//
// Delegates to internal/tenantctx so that engine can read the same value
// without importing this package -- auth's own tests import engine, so that
// direction is a cycle. These two functions stay the way everything outside
// engine refers to the tenant; only the key moved.
func WithTenantID(ctx context.Context, tenantID uuid.UUID) context.Context {
	return tenantctx.With(ctx, tenantID)
}

// TenantIDFromContext extracts the tenant ID from the request context.
func TenantIDFromContext(ctx context.Context) (uuid.UUID, bool) {
	return tenantctx.From(ctx)
}

// TenantIDFromRequest is TenantIDFromContext for the common case of an
// *http.Request, so a plugin route handler does not need its own copy of
// this call.
//
// ok is false exactly when Middleware set no tenant -- an unauthenticated
// request on a route with requireAuth false, or a public path. It is true
// for every authenticated request, INCLUDING one authenticated as the
// seeded default tenant (00000000-0000-0000-0000-000000000000), whose ID
// is the zero uuid.UUID. Comparing the returned UUID to uuid.Nil instead of
// checking ok cannot tell those two apart, and rejects the default tenant's
// own valid API key with a 401 on every route that does it -- cleat#2183.
func TenantIDFromRequest(r *http.Request) (uuid.UUID, bool) {
	return TenantIDFromContext(r.Context())
}

// subjectContextKey is unexported, so a second declaration of an identical
// struct elsewhere is a DIFFERENT key that silently reads nothing -- the same
// reason tenantctx exists as its own package (see WithTenantID above). This
// one does not need that package: nothing outside plugins reads or writes a
// subject, so there is no cross-package cycle to avoid, and a plain
// unexported type here is the whole mechanism.
type subjectContextKey struct{}

// WithSubject sets the authenticated identity string in the context -- the
// end of a durable call chain, not a display name: whatever an authentication
// plugin resolved a request to (an email, a subject claim, an API key's
// owner), for another plugin to attribute an action to without depending on
// which authentication method produced it. cleat#1881.
//
// Deliberately neutral rather than named after OAuth. oauthprovider is the
// only plugin that populates it today, but it is not the only way a request
// could be authenticated -- an API-key caller has nowhere else to put a
// subject, and a helper only OAuth could fill would need replacing the first
// time someone audits an API-key request.
func WithSubject(ctx context.Context, subject string) context.Context {
	return context.WithValue(ctx, subjectContextKey{}, subject)
}

// SubjectFromContext extracts the authenticated identity string set by
// WithSubject. ok is false when nothing set one -- an unauthenticated
// request, or an authentication method that has not been wired to call
// WithSubject yet -- and callers should treat that the same as an empty
// subject rather than an error: recording that a caller's identity was not
// captured is not a reason to refuse the request that revealed it.
func SubjectFromContext(ctx context.Context) (string, bool) {
	subject, ok := ctx.Value(subjectContextKey{}).(string)
	return subject, ok
}

// TenantResolver is the only thing this middleware needs from a store: turning
// an API key hash into a tenant.
//
// Narrowed from engine.WorkflowStore (99 methods) so that the resolver can be
// something that is NOT tenant-scoped. On MySQL it must be: tenant isolation
// there is one database per tenant, so a tenant-scoped store looks for the key
// in the tenant's database while every writer puts it in the base one. Both
// engine.WorkflowStore and auth.TenantStore satisfy this. See cleat#866.
type TenantResolver interface {
	ResolveTenantFromAPIKey(ctx context.Context, keyHash []byte) (uuid.UUID, error)
}

// TenantFromAPIKey looks up a tenant by API key hash.
func TenantFromAPIKey(ctx context.Context, store TenantResolver, keyHash []byte) (uuid.UUID, error) {
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
func Middleware(store TenantResolver, requireAuth bool, publicPatterns ...string) func(http.Handler) http.Handler {
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
			ctx := tenantctx.With(r.Context(), tenantID)
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
